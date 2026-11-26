package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// challtestsrv sets TXT records in Pebble's test DNS server.
type challtestProvider struct{ api string }

func (p challtestProvider) SetTXT(ctx context.Context, fqdn, value string) (func(), error) {
	body := `{"host":"` + fqdn + `.","value":"` + value + `"}`
	if _, err := http.Post(p.api+"/set-txt", "application/json", strings.NewReader(body)); err != nil {
		return nil, err
	}
	return func() { http.Post(p.api+"/clear-txt", "application/json", strings.NewReader(`{"host":"`+fqdn+`."}`)) }, nil
}

// Run with a local Pebble: PEBBLE=https://127.0.0.1:14000/dir CHALLTEST=http://127.0.0.1:8055 go test -run Pebble
func TestDNSCertPebble(t *testing.T) {
	dir := os.Getenv("PEBBLE")
	if dir == "" {
		t.Skip("PEBBLE not set")
	}
	m := newDNSCertManager([]string{"mc.example.test"}, "", t.TempDir(), dir, challtestProvider{os.Getenv("CHALLTEST")})
	m.client.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	m.resolvers = []string{"127.0.0.1:8053"}
	if err := m.obtain(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := m.cert.Load()
	if c.Leaf.DNSNames[0] != "mc.example.test" || renewDue(c) {
		t.Fatalf("unexpected cert: %v %v", c.Leaf.DNSNames, c.Leaf.NotAfter)
	}
	// a restart loads the stored certificate
	if c2, err := loadPEMCert(m.certPath()); err != nil || renewDue(c2) {
		t.Fatalf("reload: %v", err)
	}
	t.Logf("issued by %q, valid until %s", c.Leaf.Issuer.CommonName, c.Leaf.NotAfter.Format(time.DateOnly))
}

// The IONOS client against a fake API that checks the request shapes.
func TestIONOS(t *testing.T) {
	var log []string
	records := map[string]ionosRecord{"r1": {ID: "r1", Name: "mc.example.de", Type: "A", Content: "1.2.3.4"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "pre.secret" {
			w.WriteHeader(401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		log = append(log, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		switch {
		case r.URL.Path == "/zones":
			w.Write([]byte(`[{"id":"z0","name":"example.com","type":"NATIVE"},{"id":"z1","name":"example.de","type":"NATIVE"}]`))
		case r.Method == "GET" && r.URL.Path == "/zones/z1":
			var out []ionosRecord
			for _, rec := range records {
				if rec.Name == r.URL.Query().Get("recordName") && rec.Type == r.URL.Query().Get("recordType") {
					out = append(out, rec)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"records": out})
		case r.Method == "POST" && r.URL.Path == "/zones/z1/records":
			var in []ionosRecord
			json.Unmarshal(b, &in)
			in[0].ID = "new1"
			records["new1"] = in[0]
			json.NewEncoder(w).Encode(in)
		case r.Method == "PUT":
			id := strings.TrimPrefix(r.URL.Path, "/zones/z1/records/")
			rec := records[id]
			json.Unmarshal(b, &rec)
			records[id] = rec
		case r.Method == "DELETE":
			delete(records, strings.TrimPrefix(r.URL.Path, "/zones/z1/records/"))
		}
	}))
	defer srv.Close()
	p := newIONOS("pre.secret")
	p.base = srv.URL
	ctx := context.Background()
	cleanup, err := p.SetTXT(ctx, "_acme-challenge.mc.example.de", "tok")
	if err != nil || records["new1"].Type != "TXT" || records["new1"].Name != "_acme-challenge.mc.example.de" {
		t.Fatalf("SetTXT: %v %+v", err, records)
	}
	cleanup()
	if _, ok := records["new1"]; ok {
		t.Fatal("TXT not removed")
	}
	if ch, err := p.ensureA(ctx, "mc.example.de", "1.2.3.4"); err != nil || ch {
		t.Fatalf("unchanged A: %v %v", ch, err)
	}
	if ch, err := p.ensureA(ctx, "mc.example.de", "5.6.7.8"); err != nil || !ch || records["r1"].Content != "5.6.7.8" {
		t.Fatalf("update A: %v %v %+v", ch, err, records["r1"])
	}
	if _, err := p.zoneFor(ctx, "mc.other.org"); err == nil {
		t.Fatal("expected no zone")
	}
	for _, l := range log {
		t.Log(l)
	}
}

func TestCloudflare(t *testing.T) {
	recs := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
			return
		}
		switch {
		case r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				w.Write([]byte(`{"success":true,"result":[{"id":"z9","name":"example.com"}]}`))
			} else {
				w.Write([]byte(`{"success":true,"result":[]}`))
			}
		case r.Method == "POST" && r.URL.Path == "/zones/z9/dns_records":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			recs["rec1"] = in
			w.Write([]byte(`{"success":true,"result":{"id":"rec1"}}`))
		case r.Method == "DELETE" && r.URL.Path == "/zones/z9/dns_records/rec1":
			delete(recs, "rec1")
			w.Write([]byte(`{"success":true,"result":{"id":"rec1"}}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"not found"}]}`))
		}
	}))
	defer srv.Close()
	p := newCloudflare("tok")
	p.base = srv.URL
	cleanup, err := p.SetTXT(context.Background(), "_acme-challenge.multiclaude.example.com", "val")
	if err != nil || recs["rec1"]["name"] != "_acme-challenge.multiclaude.example.com" || recs["rec1"]["type"] != "TXT" {
		t.Fatalf("SetTXT: %v %v", err, recs)
	}
	cleanup()
	if len(recs) != 0 {
		t.Fatal("TXT not removed")
	}
	bad := newCloudflare("wrong")
	bad.base = srv.URL
	if _, err := bad.SetTXT(context.Background(), "_acme-challenge.multiclaude.example.com", "v"); err == nil || !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("expected auth error, got %v", err)
	}
}
