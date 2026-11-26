package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
)

// Let's Encrypt via the DNS-01 challenge. Unlike TLS-ALPN/HTTP challenges it
// needs no inbound port 80/443, so the server can run on any port (e.g. 8943).
// The TXT record is created through the DNS provider's API.

// dnsProvider creates and removes the _acme-challenge TXT record.
type dnsProvider interface {
	SetTXT(ctx context.Context, fqdn, value string) (cleanup func(), err error)
}

type dnsCertManager struct {
	domains  []string
	email    string
	dir      string // certificate storage
	provider dnsProvider
	client   *acme.Client
	cert     atomic.Pointer[tls.Certificate]
	// resolvers used to check the TXT record is visible before validation
	resolvers []string
}

func newDNSCertManager(domains []string, email, storeDir, directoryURL string, p dnsProvider) *dnsCertManager {
	if directoryURL == "" {
		directoryURL = acme.LetsEncryptURL
	}
	os.MkdirAll(storeDir, 0o700)
	return &dnsCertManager{domains: domains, email: email, dir: storeDir, provider: p,
		client:    &acme.Client{DirectoryURL: directoryURL, UserAgent: "multiclaude"},
		resolvers: []string{"1.1.1.1:53", "8.8.8.8:53"}}
}

func (m *dnsCertManager) certPath() string { return filepath.Join(m.dir, m.domains[0]+".pem") }

// Start loads a stored certificate (or a temporary self-signed one) and keeps
// a valid Let's Encrypt certificate in place in the background.
func (m *dnsCertManager) Start(ctx context.Context) {
	if c, err := loadPEMCert(m.certPath()); err == nil {
		m.cert.Store(c)
	} else {
		m.cert.Store(selfSigned(m.domains))
	}
	go func() {
		backoff := time.Minute
		for {
			wait := 6 * time.Hour
			if renewDue(m.cert.Load()) {
				log.Printf("tls: requesting a Let's Encrypt certificate for %s (DNS challenge)…", strings.Join(m.domains, ", "))
				if err := m.obtain(ctx); err != nil {
					log.Printf("tls: certificate request failed: %v (retrying in %s)", err, backoff)
					wait, backoff = backoff, min(backoff*2, 6*time.Hour)
				} else {
					backoff = time.Minute
					log.Printf("tls: certificate installed, valid until %s", m.cert.Load().Leaf.NotAfter.Format("2006-01-02"))
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

func (m *dnsCertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.cert.Load(), nil
}

func renewDue(c *tls.Certificate) bool {
	if c == nil || c.Leaf == nil || c.Leaf.Issuer.CommonName == "MultiClaude temporary" {
		return true
	}
	// renew with a third of the lifetime left (works for 90-day and shorter certs)
	life := c.Leaf.NotAfter.Sub(c.Leaf.NotBefore)
	return time.Until(c.Leaf.NotAfter) < life/3
}

func (m *dnsCertManager) accountKey() (crypto.Signer, error) {
	p := filepath.Join(m.dir, "acme-account.key")
	if b, err := os.ReadFile(p); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			return x509.ParseECPrivateKey(blk.Bytes)
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalECPrivateKey(k)
	return k, os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func (m *dnsCertManager) obtain(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if m.client.Key == nil {
		k, err := m.accountKey()
		if err != nil {
			return err
		}
		m.client.Key = k
		acct := &acme.Account{}
		if m.email != "" {
			acct.Contact = []string{"mailto:" + m.email}
		}
		if _, err := m.client.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
			m.client.Key = nil
			return fmt.Errorf("ACME account: %w", err)
		}
	}
	order, err := m.client.AuthorizeOrder(ctx, acme.DomainIDs(m.domains...))
	if err != nil {
		return fmt.Errorf("order: %w", err)
	}
	for _, u := range order.AuthzURLs {
		z, err := m.client.GetAuthorization(ctx, u)
		if err != nil {
			return err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, c := range z.Challenges {
			if c.Type == "dns-01" {
				chal = c
			}
		}
		if chal == nil {
			return fmt.Errorf("no dns-01 challenge offered for %s", z.Identifier.Value)
		}
		val, err := m.client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return err
		}
		fqdn := "_acme-challenge." + strings.TrimPrefix(z.Identifier.Value, "*.")
		cleanup, err := m.provider.SetTXT(ctx, fqdn, val)
		if err != nil {
			return fmt.Errorf("creating TXT record %s: %w", fqdn, err)
		}
		waitForTXT(ctx, m.resolvers, fqdn, val)
		_, err = m.client.Accept(ctx, chal)
		if err == nil {
			_, err = m.client.WaitAuthorization(ctx, z.URI)
		}
		cleanup()
		if err != nil {
			return fmt.Errorf("validating %s: %w", z.Identifier.Value, err)
		}
	}
	if order, err = m.client.WaitOrder(ctx, order.URI); err != nil {
		return fmt.Errorf("order: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: m.domains[0]}, DNSNames: m.domains}, key)
	if err != nil {
		return err
	}
	ders, _, err := m.client.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		return fmt.Errorf("issuing certificate: %w", err)
	}
	var buf bytes.Buffer
	kder, _ := x509.MarshalECPrivateKey(key)
	pem.Encode(&buf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	for _, d := range ders {
		pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: d})
	}
	if err := os.WriteFile(m.certPath(), buf.Bytes(), 0o600); err != nil {
		return err
	}
	c, err := loadPEMCert(m.certPath())
	if err != nil {
		return err
	}
	m.cert.Store(c)
	return nil
}

func loadPEMCert(path string) (*tls.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, err
	}
	if c.Leaf == nil {
		c.Leaf, _ = x509.ParseCertificate(c.Certificate[0])
	}
	return &c, nil
}

// selfSigned serves HTTPS until the real certificate has arrived.
func selfSigned(domains []string) *tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	tpl.Subject.CommonName = "MultiClaude temporary"
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// waitForTXT polls public resolvers until the record is visible (or ~3 min).
func waitForTXT(ctx context.Context, resolvers []string, fqdn, val string) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		seen := 0
		for _, ns := range resolvers {
			r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, ns)
			}}
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
			txts, _ := r.LookupTXT(c, fqdn)
			cancel()
			for _, t := range txts {
				if t == val {
					seen++
					break
				}
			}
		}
		if seen == len(resolvers) {
			time.Sleep(5 * time.Second)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
	log.Printf("tls: TXT record %s not visible yet on public resolvers; trying anyway", fqdn)
}

// ---- IONOS DNS API (https://developer.hosting.ionos.de/docs/dns) ----------------

type ionosDNS struct {
	apiKey string // "prefix.secret"
	base   string
	http   *http.Client
}

func newIONOS(apiKey string) *ionosDNS {
	return &ionosDNS{apiKey: apiKey, base: "https://api.hosting.ionos.com/dns/v1", http: &http.Client{Timeout: 30 * time.Second}}
}

type ionosRecord struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl,omitempty"`
	Prio     int    `json:"prio"`
	Disabled bool   `json:"disabled"`
}

func (p *ionosDNS) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", p.apiKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("IONOS API %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// zoneFor finds the zone (e.g. example.de) that contains name (e.g. mc.example.de).
func (p *ionosDNS) zoneFor(ctx context.Context, name string) (string, error) {
	var zones []struct{ ID, Name string }
	if err := p.do(ctx, "GET", "/zones", nil, &zones); err != nil {
		return "", err
	}
	best, bestLen := "", 0
	for _, z := range zones {
		if (name == z.Name || strings.HasSuffix(name, "."+z.Name)) && len(z.Name) > bestLen {
			best, bestLen = z.ID, len(z.Name)
		}
	}
	if best == "" {
		return "", fmt.Errorf("no IONOS DNS zone contains %s (is the domain in this IONOS account?)", name)
	}
	return best, nil
}

func (p *ionosDNS) SetTXT(ctx context.Context, fqdn, value string) (func(), error) {
	zone, err := p.zoneFor(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	var created []ionosRecord
	err = p.do(ctx, "POST", "/zones/"+zone+"/records", []ionosRecord{{Name: fqdn, Type: "TXT", Content: value, TTL: 60}}, &created)
	if err != nil {
		return nil, err
	}
	return func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, r := range created {
			if err := p.do(c, "DELETE", "/zones/"+zone+"/records/"+r.ID, nil, nil); err != nil {
				log.Printf("tls: could not remove TXT record %s: %v", fqdn, err)
			}
		}
	}, nil
}

// ---- dynamic DNS: keep the A record pointing at this connection's public IP ----

func publicIPv4(ctx context.Context) (string, error) {
	for _, u := range []string{"https://api.ipify.org", "https://ipv4.icanhazip.com"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
		resp.Body.Close()
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	return "", errors.New("could not determine the public IPv4 address")
}

func (p *ionosDNS) ensureA(ctx context.Context, name, ip string) (changed bool, err error) {
	zone, err := p.zoneFor(ctx, name)
	if err != nil {
		return false, err
	}
	var z struct{ Records []ionosRecord }
	if err := p.do(ctx, "GET", "/zones/"+zone+"?recordName="+name+"&recordType=A", nil, &z); err != nil {
		return false, err
	}
	for _, r := range z.Records {
		if r.Name == name && r.Type == "A" {
			if r.Content == ip {
				return false, nil
			}
			upd := map[string]any{"content": ip, "ttl": 60, "prio": r.Prio, "disabled": false}
			return true, p.do(ctx, "PUT", "/zones/"+zone+"/records/"+r.ID, upd, nil)
		}
	}
	return true, p.do(ctx, "POST", "/zones/"+zone+"/records", []ionosRecord{{Name: name, Type: "A", Content: ip, TTL: 60}}, nil)
}

// runDynDNS updates the A records of the domains every 5 minutes.
func runDynDNS(ctx context.Context, p *ionosDNS, domains []string) {
	last := ""
	for {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		ip, err := publicIPv4(c)
		if err == nil && ip != last {
			for _, d := range domains {
				changed, e := p.ensureA(c, d, ip)
				if e != nil {
					err = e
					log.Printf("dyndns: updating %s: %v", d, e)
				} else if changed {
					log.Printf("dyndns: %s → %s", d, ip)
				}
			}
			if err == nil {
				last = ip
			}
		} else if err != nil {
			log.Printf("dyndns: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// ---- Cloudflare DNS API ------------------------------------------------------------
// Token needs Zone → DNS → Edit and Zone → Zone → Read for the zone.

type cloudflareDNS struct {
	token string
	base  string
	http  *http.Client
}

func newCloudflare(token string) *cloudflareDNS {
	return &cloudflareDNS{token: token, base: "https://api.cloudflare.com/client/v4", http: &http.Client{Timeout: 30 * time.Second}}
}

func (p *cloudflareDNS) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(b, &env); err != nil || !env.Success {
		msg := strings.TrimSpace(string(b))
		if len(env.Errors) > 0 {
			msg = fmt.Sprintf("%d %s", env.Errors[0].Code, env.Errors[0].Message)
		}
		return fmt.Errorf("Cloudflare API %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// zoneFor tries internal.example.dev, then example.dev, … until a zone matches.
func (p *cloudflareDNS) zoneFor(ctx context.Context, name string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		var zones []struct{ ID, Name string }
		if err := p.do(ctx, "GET", "/zones?name="+strings.Join(labels[i:], "."), nil, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("no Cloudflare zone found for %s (does the token have Zone:Read for it?)", name)
}

func (p *cloudflareDNS) SetTXT(ctx context.Context, fqdn, value string) (func(), error) {
	zone, err := p.zoneFor(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	var rec struct{ ID string }
	if err := p.do(ctx, "POST", "/zones/"+zone+"/dns_records",
		map[string]any{"type": "TXT", "name": fqdn, "content": value, "ttl": 60}, &rec); err != nil {
		return nil, err
	}
	return func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.do(c, "DELETE", "/zones/"+zone+"/dns_records/"+rec.ID, nil, nil); err != nil {
			log.Printf("tls: could not remove TXT record %s: %v", fqdn, err)
		}
	}, nil
}
