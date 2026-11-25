package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"multiclaude/internal/proto"
	"multiclaude/internal/runner"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func split(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func usage() {
	fmt.Fprintf(os.Stderr, `MultiClaude runner %s

Usage:
  multiclaude-runner run    --server http://nas:8080 [options]   run the daemon
  multiclaude-runner doctor --server http://nas:8080 [options]   show detected hardware/claude and exit
  multiclaude-runner mcp    --server URL --run ID                (internal) MCP bridge used by Claude Code

Options for run/doctor:
`, proto.Version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	server := fs.String("server", env("MC_SERVER", ""), "MultiClaude server URL")
	name := fs.String("name", env("MC_NAME", ""), "machine name (default: hostname)")
	roles := fs.String("roles", env("MC_ROLES", "worker,reviewer"), "comma separated roles: worker,reviewer")
	caps := fs.String("caps", env("MC_CAPS", ""), "extra capability tags, comma separated (e.g. unity,staging-db)")
	conc := fs.Int("concurrency", 2, "max parallel Claude Code sessions on this machine")
	claude := fs.String("claude", env("MC_CLAUDE", ""), "path to the claude CLI (default: auto-detect)")
	workdir := fs.String("workdir", env("MC_WORKDIR", ""), "work directory (default ~/.multiclaude)")
	perm := fs.String("permission-mode", env("MC_PERMISSION_MODE", "bypassPermissions"), "Claude Code --permission-mode")
	extra := fs.String("claude-args", env("MC_CLAUDE_ARGS", ""), "extra arguments passed to claude (space separated)")
	run := fs.String("run", "", "run id (mcp subcommand)")
	fs.Usage = func() { usage(); fs.PrintDefaults() }
	fs.Parse(os.Args[2:])

	cfg := runner.Config{
		Server: *server, Name: *name, Roles: split(*roles), ExtraCaps: split(*caps), Concurrency: *conc,
		ClaudePath: *claude, WorkDir: *workdir, PermissionMode: *perm, ExtraArgs: strings.Fields(*extra),
	}
	switch sub {
	case "mcp":
		if err := runner.ServeMCP(*server, *run, os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
	case "doctor":
		runner.Doctor(cfg)
	case "run":
		r, err := runner.New(cfg)
		if err != nil {
			log.Fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := r.Run(ctx); err != nil {
			log.Fatal(err)
		}
	case "version", "--version", "-v":
		fmt.Println(proto.Version)
	default:
		usage()
		os.Exit(2)
	}
}
