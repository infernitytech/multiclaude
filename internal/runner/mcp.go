package runner

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"multiclaude/internal/proto"
)

// ServeMCP runs a stdio MCP server that forwards the coordination tools to the
// MultiClaude server. Claude Code launches it via --mcp-config.
func ServeMCP(server, runID string, in io.Reader, out io.Writer) error {
	server = strings.TrimRight(server, "/")
	// Long timeout: ask_human may wait for a human answer (the server caps one
	// call at X-MC-Max-Wait seconds).
	client := &http.Client{Timeout: 32 * time.Minute}
	enc := json.NewEncoder(out)
	var encMu sync.Mutex // tool calls run concurrently (ask_human may wait a long time)
	var wg sync.WaitGroup
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 32<<20)

	reply := func(id json.RawMessage, result any) {
		encMu.Lock()
		defer encMu.Unlock()
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(id json.RawMessage, code int, msg string) {
		encMu.Lock()
		defer encMu.Unlock()
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}

	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		isNotification := len(req.ID) == 0 || string(req.ID) == "null"
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			reply(req.ID, map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "multiclaude", "version": proto.Version},
				"instructions":    "Coordination tools for the MultiClaude agent swarm: issue board, project chat, machine list.",
			})
		case "ping":
			reply(req.ID, map[string]any{})
		case "tools/list":
			reply(req.ID, map[string]any{"tools": proto.Tools})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			b, _ := json.Marshal(proto.AgentCall{RunID: runID, Tool: p.Name, Args: p.Arguments})
			wg.Add(1)
			go func(id json.RawMessage) {
				defer wg.Done()
				res := callServer(client, server, b)
				reply(id, map[string]any{
					"content": []map[string]string{{"type": "text", "text": res.Text}},
					"isError": res.IsError,
				})
			}(req.ID)
		default:
			if !isNotification {
				fail(req.ID, -32601, "method not found: "+req.Method)
			}
		}
	}
	wg.Wait()
	return sc.Err()
}

// callServer forwards one tool call. While the server is unreachable (restart,
// update) it retries for up to 15 minutes instead of failing the agent's tool
// call; the call id lets the server recognise a retry of a call it already
// carried out, so nothing happens twice.
func callServer(client *http.Client, server string, body []byte) proto.AgentResult {
	callID := newCallID()
	wait, deadline := 2*time.Second, time.Now().Add(15*time.Minute)
	for {
		hreq, _ := http.NewRequest("POST", server+"/api/agent/call", bytes.NewReader(body))
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("X-MC-Max-Wait", "1800")
		hreq.Header.Set("X-MC-Call-ID", callID)
		if tok := os.Getenv("MC_TOKEN"); tok != "" {
			hreq.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := client.Do(hreq)
		if err == nil && (resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504) {
			resp.Body.Close()
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if err != nil {
			if time.Now().After(deadline) {
				return proto.AgentResult{Text: "MultiClaude server unreachable for 15 minutes: " + err.Error() + ". Continue without the coordination tools for now and try again later.", IsError: true}
			}
			time.Sleep(wait)
			wait = min(wait*2, 20*time.Second)
			continue
		}
		var res proto.AgentResult
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			res = proto.AgentResult{Text: fmt.Sprintf("bad server response (HTTP %d)", resp.StatusCode), IsError: true}
		}
		resp.Body.Close()
		return res
	}
}

func newCallID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
