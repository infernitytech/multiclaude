package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"multiclaude/internal/server"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	fs := flag.NewFlagSet("multiclaude-server", flag.ExitOnError)
	addr := fs.String("addr", env("MC_ADDR", ":8090"), `HTTP listen address ("off" to disable plain HTTP)`)
	data := fs.String("data", env("MC_DATA", "./data"), "data directory (repos, LFS objects, database)")
	dl := fs.String("downloads", env("MC_DOWNLOADS", ""), "directory with prebuilt runner binaries served at /download/")
	domain := fs.String("domain", env("MC_DOMAIN", ""), "public domain name(s), comma separated: enables HTTPS with automatic Let's Encrypt certificates")
	email := fs.String("acme-email", env("MC_ACME_EMAIL", ""), "contact e-mail for Let's Encrypt (optional)")
	cert := fs.String("tls-cert", env("MC_TLS_CERT", ""), "TLS certificate file (instead of Let's Encrypt)")
	key := fs.String("tls-key", env("MC_TLS_KEY", ""), "TLS key file")
	tlsAddr := fs.String("tls-addr", env("MC_TLS_ADDR", ":8443"), "HTTPS listen address")
	dnsProv := fs.String("dns-provider", env("MC_DNS_PROVIDER", ""), `get certificates via the DNS challenge (any port works): "cloudflare" or "ionos"`)
	dnsKey := fs.String("dns-api-key", env("MC_DNS_API_KEY", ""), "DNS provider API key (Cloudflare: API token; IONOS: prefix.secret)")
	acmeDir := fs.String("acme-directory", env("MC_ACME_DIRECTORY", ""), "ACME directory URL (default Let's Encrypt; staging: https://acme-staging-v02.api.letsencrypt.org/directory)")
	dyndns := fs.Bool("dyndns", env("MC_DYNDNS", "") == "on" || env("MC_DYNDNS", "") == "true", "keep the domain's A record pointed at this connection's public IPv4 (IONOS)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  multiclaude-server [options]                 run the server
  multiclaude-server reset-password [user]     set a new random password for a user (creates it if missing)

Options:
`)
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs.Parse(args)
	var pos []string // positional args may come before or after flags
	for fs.NArg() > 0 {
		pos = append(pos, fs.Arg(0))
		fs.Parse(fs.Args()[1:])
	}

	s, err := server.New(*data, *addr, *dl)
	if err != nil {
		log.Fatal(err)
	}
	switch cmd {
	case "":
	case "reset-password":
		user := "admin"
		if len(pos) > 0 {
			user = pos[0]
		}
		pw, err := s.ResetPassword(user)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("New password for %s: %s\n", user, pw)
		return
	default:
		fs.Usage()
		os.Exit(2)
	}
	for _, d := range strings.Split(*domain, ",") {
		if d = strings.TrimSpace(d); d != "" {
			s.Domains = append(s.Domains, d)
		}
	}
	s.ACMEEmail, s.CertFile, s.KeyFile, s.TLSAddr = *email, *cert, *key, *tlsAddr
	s.DNSProvider, s.DNSAPIKey, s.ACMEDirectory, s.DynDNS = strings.ToLower(*dnsProv), *dnsKey, *acmeDir, *dyndns
	if (*cert == "") != (*key == "") {
		log.Fatal("--tls-cert and --tls-key must be given together")
	}
	log.Fatal(s.Run())
}
