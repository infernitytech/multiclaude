package server

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"multiclaude/internal/proto"
)

// The server checks GitHub releases so the UI can say when an update exists.
// MC_UPDATE_REPO=owner/name changes the repository, "off" disables the check.

var latestTag atomic.Value // string

func latestRelease() string {
	v, _ := latestTag.Load().(string)
	if v == "" || !newerVersion(v, proto.Version) {
		return ""
	}
	return v
}

func (s *Server) updateCheckLoop() {
	repo := os.Getenv("MC_UPDATE_REPO")
	if repo == "" {
		repo = "infernitytech/multiclaude"
	}
	if repo == "off" {
		return
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var rel struct {
			TagName string `json:"tag_name"`
		}
		if _, err := gitHubAPI(ctx, "GET", "/repos/"+repo+"/releases/latest", "", nil, &rel); err == nil && rel.TagName != "" {
			if old, _ := latestTag.Load().(string); old != rel.TagName && newerVersion(rel.TagName, proto.Version) {
				log.Printf("update available: %s (running %s)", rel.TagName, proto.Version)
			}
			latestTag.Store(rel.TagName)
		}
		cancel()
		time.Sleep(6 * time.Hour)
	}
}

// newerVersion reports whether a (vX.Y.Z) is newer than b. Dev builds never nag.
func newerVersion(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.SplitN(v, "-", 2)[0], "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
