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
#   sudo scripts/install-replication-server.sh [--public-host IP] [--region us-ord] [--port 8080]
#
# It installs everything it needs (git, make, gcc, curl, openssl, jq, tar and a
# recent Go) using the system package manager (apt/dnf/yum/zypper), builds the
# binaries, and sets up the service. Requires: bash, root, and internet access.
set -euo pipefail

PUBLIC_HOST=""; REGION="us-ord"; PORT="8080"
while [ $# -gt 0 ]; do
  case "$1" in
    --public-host) PUBLIC_HOST="$2"; shift 2;;
    --region)      REGION="$2"; shift 2;;
    --port)        PORT="$2"; shift 2;;
    -h|--help)     sed -n '2,18p' "$0"; exit 0;;
    *) echo "unknown arg: $1"; exit 1;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ETC=/etc/vm-repl
LIB=/var/lib/vm-repl
OPT=/opt/vm-repl

# ---------------------------------------------------------------------------
# Dependency bootstrap: make the one-liner work on a bare server.
# ---------------------------------------------------------------------------
detect_pkg_mgr() {
  for m in apt-get dnf yum zypper; do command -v "$m" >/dev/null 2>&1 && { echo "$m"; return; }; done
  echo ""
}

install_packages() {
  local mgr; mgr="$(detect_pkg_mgr)"
  echo ">> Installing system packages (git make gcc curl openssl jq tar ca-certificates) via ${mgr:-<none>}"
  case "$mgr" in
    apt-get)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -y || apt-get update -y
      apt-get install -y --no-install-recommends git make gcc curl ca-certificates openssl jq tar
      ;;
    dnf)    dnf install -y git make gcc curl ca-certificates openssl jq tar ;;
    yum)    yum install -y git make gcc curl ca-certificates openssl jq tar ;;
    zypper) zypper --non-interactive install git make gcc curl ca-certificates openssl jq tar ;;
    *) echo "WARNING: no supported package manager found; please install: git make gcc curl openssl jq tar" ;;
  esac
}

# go_ok: true if a Go >= 1.21 toolchain is on PATH (older Go auto-downloads the
# version this module needs via GOTOOLCHAIN).
go_ok() {
  command -v go >/dev/null 2>&1 || return 1
  local v major rest minor
  v="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
  major="${v%%.*}"; rest="${v#*.}"; minor="${rest%%.*}"
  [ -n "$major" ] || return 1
  if [ "$major" -gt 1 ] 2>/dev/null; then return 0; fi
  if [ "$major" -eq 1 ] 2>/dev/null && [ "${minor:-0}" -ge 21 ] 2>/dev/null; then return 0; fi
  return 1
}

install_go() {
  local arch goarch gover url
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)  goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    *) echo "unsupported CPU arch for Go auto-install: $arch — install Go manually"; exit 1 ;;
  esac
  gover="$(curl -fsSL --max-time 10 'https://go.dev/VERSION?m=text' 2>/dev/null | head -1)"
  case "$gover" in go*) ;; *) gover="go1.25.1" ;; esac
  url="https://go.dev/dl/${gover}.linux-${goarch}.tar.gz"
  echo ">> Installing ${gover} (${goarch}) from go.dev"
  curl -fsSL "$url" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
  rm -f /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  export PATH="/usr/local/go/bin:$PATH"
}

# Decide whether we must build (no complete set of prebuilt binaries).
NEED_BUILD=1
if [ -x "$ROOT/bin/applianced" ] && [ -x "$ROOT/bin/agent" ] && [ -x "$ROOT/bin/receiver" ] \
   && [ -x "$ROOT/bin/controld" ] && [ -x "$ROOT/bin/replctl" ]; then
  NEED_BUILD=0
fi

# Install OS packages if any required tool is missing (runtime tools always;
# build tools only when we must compile).
missing=0
for t in curl openssl tar; do command -v "$t" >/dev/null 2>&1 || missing=1; done
if [ "$NEED_BUILD" -eq 1 ]; then
  for t in git make gcc; do command -v "$t" >/dev/null 2>&1 || missing=1; done
fi
[ "$missing" -eq 1 ] && install_packages

# Install Go only if we must build and a suitable one isn't already present.
if [ "$NEED_BUILD" -eq 1 ] && ! go_ok; then install_go; fi

command -v openssl >/dev/null 2>&1 || { echo "openssl unavailable after install; aborting"; exit 1; }

# --- detect public host ---
if [ -z "$PUBLIC_HOST" ]; then
  PUBLIC_HOST="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  [ -z "$PUBLIC_HOST" ] && PUBLIC_HOST="$(hostname -I 2>/dev/null | awk '{print $1}')"
  [ -z "$PUBLIC_HOST" ] && { echo "could not detect public IP; pass --public-host"; exit 1; }
  echo ">> Detected public host: $PUBLIC_HOST"
fi

# --- build binaries (deps are now in place) ---
if [ "$NEED_BUILD" -eq 1 ]; then
  if ! command -v make >/dev/null 2>&1 || ! go_ok; then
    echo "build tools missing after bootstrap (need make + Go >= 1.21); aborting"; exit 1
  fi
  echo ">> Building binaries"
  ( cd "$ROOT" && make build >/dev/null )
else
  echo ">> Using prebuilt binaries in $ROOT/bin"
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

 Forgot the password? Retrieve it any time on this server with:
   sudo /usr/local/bin/applianced -data-dir $LIB -show-password
 (Signing out of the console does NOT stop migrations — replication keeps
  running in the background regardless of console sessions.)

 The console (HTTPS) and the replication data plane (mutual TLS) are both
 encrypted. Still, restrict port $PORT to trusted networks where you can.
 Logs: journalctl -u applianced -f
==========================================================
EOF
