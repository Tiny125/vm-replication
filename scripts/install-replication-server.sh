#!/usr/bin/env bash
# install-replication-server.sh — turn a fresh Linode into the vm-replication
# "replication server": build/install the appliance, generate certificates and
# an admin password, install a systemd service, and print the console URL +
# password.
#
# Run it on the replication server (a Linode), as root, from a checkout of this
# repo:
#
#   sudo scripts/install-replication-server.sh [--public-host IP] [--region us-ord] [--port 8080]
#
# Requires: bash, openssl, and either Go (to build) or prebuilt binaries in ./bin.
set -euo pipefail

PUBLIC_HOST=""; REGION="us-ord"; PORT="8080"
while [ $# -gt 0 ]; do
  case "$1" in
    --public-host) PUBLIC_HOST="$2"; shift 2;;
    --region)      REGION="$2"; shift 2;;
    --port)        PORT="$2"; shift 2;;
    -h|--help)     sed -n '2,16p' "$0"; exit 0;;
    *) echo "unknown arg: $1"; exit 1;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ETC=/etc/vm-repl
LIB=/var/lib/vm-repl
OPT=/opt/vm-repl
command -v openssl >/dev/null || { echo "openssl is required"; exit 1; }

# --- detect public host ---
if [ -z "$PUBLIC_HOST" ]; then
  PUBLIC_HOST="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  [ -z "$PUBLIC_HOST" ] && PUBLIC_HOST="$(hostname -I 2>/dev/null | awk '{print $1}')"
  [ -z "$PUBLIC_HOST" ] && { echo "could not detect public IP; pass --public-host"; exit 1; }
  echo ">> Detected public host: $PUBLIC_HOST"
fi

# --- build or locate binaries ---
if [ ! -x "$ROOT/bin/applianced" ] || [ ! -x "$ROOT/bin/agent" ]; then
  if command -v go >/dev/null 2>&1; then
    echo ">> Building binaries"
    ( cd "$ROOT" && make build >/dev/null )
  else
    echo "Go not found and ./bin is incomplete; install Go or provide prebuilt binaries."; exit 1
  fi
fi

# --- layout ---
install -d -m 700 "$ETC" "$LIB" "$OPT"
install -m 0755 "$ROOT/bin/applianced" /usr/local/bin/applianced
install -m 0755 "$ROOT/bin/agent" "$OPT/agent"                  # served to sources
install -m 0755 "$ROOT/scripts/machine-convert.sh" "$OPT/machine-convert.sh"

# --- certificates (CA + receiver + agent), receiver SAN = public host ---
if [ ! -f "$ETC/ca.crt" ]; then
  echo ">> Generating certificates (SAN=$PUBLIC_HOST)"
  DAYS=1825 "$ROOT/scripts/gen-certs.sh" "$ETC" "$PUBLIC_HOST" >/dev/null
  chmod 600 "$ETC"/*.key
else
  echo ">> Reusing existing certificates in $ETC"
fi

# --- systemd service ---
cat >/etc/systemd/system/applianced.service <<UNIT
[Unit]
Description=vm-replication appliance (migration console + receivers)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/applianced \\
  -listen :$PORT \\
  -data-dir $LIB \\
  -public-host $PUBLIC_HOST \\
  -region $REGION \\
  -cert $ETC/receiver.crt -key $ETC/receiver.key -ca $ETC/ca.crt \\
  -agent-cert $ETC/agent.crt -agent-key $ETC/agent.key \\
  -agent-binary $OPT/agent \\
  -convert-script $OPT/machine-convert.sh
Restart=on-failure
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now applianced.service

# --- best-effort firewall ---
if command -v ufw >/dev/null 2>&1; then
  ufw allow "$PORT"/tcp >/dev/null 2>&1 || true
  ufw allow 5000:5100/tcp >/dev/null 2>&1 || true   # per-migration receiver ports
fi

# --- wait for the generated password ---
PWFILE="$LIB/initial-admin-password.txt"
for _ in $(seq 1 30); do [ -f "$PWFILE" ] && break; sleep 0.5; done

# --- console cert fingerprint (printed so you can verify it in the browser) ---
FPR=""
if [ -f "$LIB/console.crt" ] && command -v openssl >/dev/null 2>&1; then
  FPR="$(openssl x509 -in "$LIB/console.crt" -noout -fingerprint -sha256 2>/dev/null | sed 's/.*=//')"
fi

cat <<EOF

================ REPLICATION SERVER READY ================
 Console:   https://$PUBLIC_HOST:$PORT
 Password:  $( [ -f "$PWFILE" ] && cat "$PWFILE" || echo "see: journalctl -u applianced" )
 Cert SHA-256 (verify this in your browser's certificate dialog):
   ${FPR:-see: journalctl -u applianced}

 The console uses a self-signed certificate, so your browser will warn on first
 visit — that's expected. Click through, then confirm the certificate's SHA-256
 fingerprint matches the value above before entering the password.

 Open the console in your browser, sign in with the password above, then:
   1. (optional) paste your Linode API token to enable volume provisioning
      and one-click finalize.
   2. Create a migration: enter your source server's hostname, disk device
      (e.g. /dev/sda), and disk size.
   3. Copy the generated one-line command and run it on your SOURCE server.
      (It pins this server's key, so the agent download is MITM-proof.)
   4. Watch replication status; when checks pass, click "Start migration".
   5. The migrated image (a cloned volume) can launch new Linode instances.

 The console (HTTPS) and the replication data plane (mutual TLS) are both
 encrypted. Still, restrict port $PORT to trusted networks where you can.
 Logs: journalctl -u applianced -f
==========================================================
EOF
