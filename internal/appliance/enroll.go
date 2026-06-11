package appliance

import (
	"fmt"
	"net/http"
	"os"

	"github.com/tiny125/vm-replication/internal/api"
)

// migrationFromToken resolves the ?token= query to a migration, or writes an
// error response and returns ok=false.
func (s *Server) migrationFromToken(w http.ResponseWriter, r *http.Request) (api.Migration, bool) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writeErr(w, http.StatusBadRequest, "missing token")
		return api.Migration{}, false
	}
	m, err := s.st.MigrationByToken(r.Context(), token)
	if err != nil {
		writeErr(w, http.StatusForbidden, "invalid enrollment token")
		return api.Migration{}, false
	}
	return m, true
}

// handleAgentInstaller serves a self-contained bash script that installs the
// agent on a source server, drops the mTLS material, and starts a systemd timer
// that replicates to this migration's receiver. The source runs the one-liner
// shown in the console.
func (s *Server) handleAgentInstaller(w http.ResponseWriter, r *http.Request) {
	m, ok := s.migrationFromToken(w, r)
	if !ok {
		return
	}
	token := r.URL.Query().Get("token")
	base := fmt.Sprintf("%s://%s:%d", s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort)
	target := fmt.Sprintf("%s:%d", s.cfg.PublicHost, m.ReceiverPort)

	// Note: callers should already have validated the source device exists.
	script := fmt.Sprintf(`#!/usr/bin/env bash
# vm-replication source enrollment for migration %q
set -euo pipefail
BASE=%q
TOKEN=%q
DEVICE=%q
TARGET=%q
SERVER_NAME=%q
PIN=%q
BIN=/usr/local/bin/vmrepl-agent
ETC=/etc/vm-repl

[ "$(id -u)" -eq 0 ] || { echo "run as root (use sudo)"; exit 1; }
command -v curl >/dev/null || { echo "curl is required"; exit 1; }
[ -e "$DEVICE" ] || { echo "source device $DEVICE not found — re-check the device in the console"; exit 1; }

# Pin the appliance's public key so downloads are authenticated even with a
# self-signed console certificate (trust on first use). -k skips CA-chain checks
# (no public CA), while --pinnedpubkey still requires the exact server key.
CURL="curl -fsSL"
[ -n "$PIN" ] && CURL="$CURL -k --pinnedpubkey sha256//$PIN"

# Re-running enrollment is safe: stop any previous agent first so the running
# binary can be replaced (overwriting a running executable fails with
# "text file busy" / curl error 23), and swap it in atomically.
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop vmrepl-agent.timer vmrepl-agent.service 2>/dev/null || true
fi

echo ">> Downloading agent"
TMP="$(mktemp "$(dirname "$BIN")/.vmrepl-agent.XXXXXX")"
$CURL "$BASE/download/agent?token=$TOKEN" -o "$TMP"
chmod 0755 "$TMP"
mv -f "$TMP" "$BIN"   # atomic on the same filesystem; safe even if the old binary runs

echo ">> Installing TLS material"
mkdir -p "$ETC"
for f in ca.crt agent.crt agent.key; do
  $CURL "$BASE/enroll/file?token=$TOKEN&name=$f" -o "$ETC/$f"
done
chmod 600 "$ETC/agent.key"

if command -v systemctl >/dev/null 2>&1; then
  echo ">> Installing systemd timer (replicate every 60s)"
  cat >/etc/systemd/system/vmrepl-agent.service <<UNIT
[Unit]
Description=vm-replication source agent
After=network-online.target
Wants=network-online.target
[Service]
Type=oneshot
ExecStart=$BIN -device $DEVICE -target $TARGET -server-name $SERVER_NAME -manifest /var/lib/vmrepl-source.cbt -cert $ETC/agent.crt -key $ETC/agent.key -ca $ETC/ca.crt
Nice=10
IOSchedulingClass=best-effort
UNIT
  cat >/etc/systemd/system/vmrepl-agent.timer <<UNIT
[Unit]
Description=Run the vm-replication agent periodically
[Timer]
OnBootSec=30s
OnUnitActiveSec=60s
AccuracySec=5s
Persistent=true
[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now vmrepl-agent.timer
  echo ">> Starting initial full sync now (this can take a while)"
  if systemctl start vmrepl-agent.service; then
    echo ">> First sync succeeded."
  else
    echo ">> First sync FAILED — see: journalctl -u vmrepl-agent -n 20"
    echo ">> No reinstall needed: the agent retries every 60s automatically."
    echo ">> Common cause: receiver port $TARGET blocked — open TCP 5000-5100 on the"
    echo ">> replication server's firewall (including any Linode Cloud Firewall)."
    echo ">> Force a retry any time with: systemctl start vmrepl-agent.service"
  fi
  echo ">> Enrolled. Follow progress in the console; logs: journalctl -u vmrepl-agent"
else
  echo "systemd not found; run the agent manually on a schedule:"
  echo "  $BIN -device $DEVICE -target $TARGET -server-name $SERVER_NAME \\"
  echo "      -cert $ETC/agent.crt -key $ETC/agent.key -ca $ETC/ca.crt"
fi
`, m.Name, base, token, m.SourceDevice, target, s.cfg.PublicHost, s.cfg.PublicKeyPin)

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(script))
}

// handleUninstallScript serves a script that removes everything enrollment
// installed on a source server. It is deliberately NOT token-gated: tokens are
// deleted with their migration, but operators still need to clean up sources
// afterwards — and the script contains no secrets, only removal commands.
func (s *Server) handleUninstallScript(w http.ResponseWriter, r *http.Request) {
	const script = `#!/usr/bin/env bash
# vm-replication agent uninstall — removes everything enrollment installed.
set -u
[ "$(id -u)" -eq 0 ] || { echo "run as root (use sudo)"; exit 1; }

if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now vmrepl-agent.timer 2>/dev/null
  systemctl stop vmrepl-agent.service 2>/dev/null
  rm -f /etc/systemd/system/vmrepl-agent.service /etc/systemd/system/vmrepl-agent.timer
  systemctl daemon-reload
fi
rm -f /usr/local/bin/vmrepl-agent
rm -rf /etc/vm-repl
rm -f /var/lib/vmrepl-source.cbt
echo "vm-replication agent removed. This server's data and OS were never modified."
`
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(script))
}

// handleEnrollFile serves one of the agent's TLS files, token-gated.
func (s *Server) handleEnrollFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.migrationFromToken(w, r); !ok {
		return
	}
	var path string
	switch r.URL.Query().Get("name") {
	case "ca.crt":
		path = s.cfg.CACert
	case "agent.crt":
		path = s.cfg.AgentCert
	case "agent.key":
		path = s.cfg.AgentKey
	default:
		writeErr(w, http.StatusBadRequest, "unknown file")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "enrollment material unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(data)
}

// handleDownloadAgent serves the agent binary, token-gated.
func (s *Server) handleDownloadAgent(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.migrationFromToken(w, r); !ok {
		return
	}
	if s.cfg.AgentBinary == "" {
		writeErr(w, http.StatusServiceUnavailable, "agent binary not configured on the appliance")
		return
	}
	if _, err := os.Stat(s.cfg.AgentBinary); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "agent binary not found on the appliance")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="vmrepl-agent"`)
	http.ServeFile(w, r, s.cfg.AgentBinary)
}
