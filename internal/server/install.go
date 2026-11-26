package server

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var dlRe = regexp.MustCompile(`^multiclaude-runner-(linux|darwin)-(amd64|arm64)$`)

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	f := r.PathValue("file")
	if !dlRe.MatchString(f) || s.DownloadDir == "" {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.DownloadDir, f)
	if _, err := os.Stat(p); err != nil {
		http.Error(w, "runner binary not bundled with this server build", 404)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, p)
}

const installTmpl = `#!/bin/sh
# MultiClaude runner installer. Usage:
#   curl -fsSL __SERVER__/install.sh | sh -s -- --token <join token>
#   curl -fsSL __SERVER__/install.sh | sh -s -- --token <join token> --name gpu-box --roles worker --concurrency 4
# The join token is shown in the web UI under Machines → Add a machine. It is
# only used once to enroll; the machine then gets its own token.
set -eu
SERVER="__SERVER__"
TOKEN=""
ARGS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --token=*) TOKEN="${1#--token=}"; shift ;;
    *) ARGS="$ARGS $1"; shift ;;
  esac
done

if [ "$(id -u)" = "0" ]; then
  echo "Please run as the normal user that is logged into Claude Code (not root)." >&2
  exit 1
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported arch $arch" >&2; exit 1 ;; esac

BIN_DIR="$HOME/.local/bin"
mkdir -p "$BIN_DIR"
echo "Downloading runner for $os/$arch ..."
curl -fsSL "$SERVER/download/multiclaude-runner-$os-$arch" -o "$BIN_DIR/multiclaude-runner.tmp"
chmod +x "$BIN_DIR/multiclaude-runner.tmp"
mv "$BIN_DIR/multiclaude-runner.tmp" "$BIN_DIR/multiclaude-runner"

command -v git >/dev/null || echo "WARNING: git is not installed" >&2
command -v git-lfs >/dev/null || echo "WARNING: git-lfs is not installed (needed for LFS files)" >&2

if [ -n "$TOKEN" ]; then
  "$BIN_DIR/multiclaude-runner" enroll --server "$SERVER" --token "$TOKEN" $ARGS
elif [ ! -f "$HOME/.multiclaude/credentials.json" ]; then
  echo "ERROR: this machine is not enrolled. Re-run with --token <join token> (Machines → Add a machine)." >&2
  exit 1
fi
"$BIN_DIR/multiclaude-runner" doctor --server "$SERVER" $ARGS || true

if [ "$os" = "linux" ] && command -v systemctl >/dev/null; then
  UNIT_DIR="$HOME/.config/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/multiclaude-runner.service" <<EOF
[Unit]
Description=MultiClaude runner
After=network-online.target

[Service]
ExecStart=$BIN_DIR/multiclaude-runner run --server $SERVER $ARGS
Restart=always
RestartSec=5
Environment=PATH=$PATH

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now multiclaude-runner.service
  echo
  echo "Runner installed as a systemd user service:"
  echo "  systemctl --user status multiclaude-runner"
  echo "  journalctl --user -u multiclaude-runner -f"
  echo "To keep it running while you are logged out:  sudo loginctl enable-linger $(id -un)"
elif [ "$os" = "darwin" ]; then
  PLIST="$HOME/Library/LaunchAgents/dev.multiclaude.runner.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  set -- $ARGS
  EXTRA=""
  for a in "$@"; do EXTRA="$EXTRA<string>$a</string>"; done
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.multiclaude.runner</string>
<key>ProgramArguments</key><array><string>$BIN_DIR/multiclaude-runner</string><string>run</string><string>--server</string><string>$SERVER</string>$EXTRA</array>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>$PATH</string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>$HOME/Library/Logs/multiclaude-runner.log</string>
<key>StandardErrorPath</key><string>$HOME/Library/Logs/multiclaude-runner.log</string>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "Runner installed as a LaunchAgent (logs: ~/Library/Logs/multiclaude-runner.log)"
else
  echo "Start the runner with:  $BIN_DIR/multiclaude-runner run --server $SERVER $ARGS"
fi
echo
echo "Now open $SERVER/machines and assign this machine to projects."
echo "(To remove it later: delete it on the Machines page — that revokes its token.)"
`

func (s *Server) installScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(strings.ReplaceAll(installTmpl, "__SERVER__", baseURL(r))))
}
