package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"multiclaude/internal/proto"
)

// ServeMCP runs a stdio MCP server that forwards the coordination tools to the
// MultiClaude server. Claude Code launches it via --mcp-config.
func ServeMCP(server, runID string, in io.Reader, out io.Writer) error {
	server = strings.TrimRight(server, "/")
	client := &http.Client{Timeout: 2 * time.Minute}
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 32<<20)

	reply := func(id json.RawMessage, result any) {
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(id json.RawMessage, code int, msg string) {
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
			var res proto.AgentResult
			resp, err := client.Post(server+"/api/agent/call", "application/json", bytes.NewReader(b))
			if err != nil {
				res = proto.AgentResult{Text: "MultiClaude server unreachable: " + err.Error(), IsError: true}
			} else {
				if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
					res = proto.AgentResult{Text: fmt.Sprintf("bad server response (HTTP %d)", resp.StatusCode), IsError: true}
				}
				resp.Body.Close()
			}
			reply(req.ID, map[string]any{
				"content": []map[string]string{{"type": "text", "text": res.Text}},
				"isError": res.IsError,
			})
		default:
			if !isNotification {
				fail(req.ID, -32601, "method not found: "+req.Method)
			}
		}
	}
	return sc.Err()
}

var _ = os.Stdin
