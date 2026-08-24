#!/usr/bin/env bash
# install-replication-server.sh — turn a fresh Linode into the vm-replication
# "replication server": build/install the appliance, generate certificates and
# an admin password, install a systemd service, and print the console URL +
# password.
#
# Run it on the replication server (a Linode), as root, from a checkout of this
# repo:
#
#   sudo scripts/install-replication-server.sh [--public-host IP] [--region REGION] [--port PORT]
#
# It installs everything it needs (git, make, gcc, curl, openssl, jq, tar and a
# recent Go) using the system package manager (apt/dnf/yum/zypper), builds the
# binaries, and sets up the service. Requires: bash, root, and internet access.
#
# The region is detected from the Linode Metadata service, so --region is only
# needed to override it (or when metadata is unavailable). Region and port are
# stored in /etc/vm-repl/applianced.env, which is written once and NEVER
# overwritten — re-run this script to upgrade and your settings survive.
set -euo pipefail

# Flags are recorded separately from the resolved values: an explicit flag has to
# outrank a stored setting, and "" has to mean "not given" rather than "empty".
PUBLIC_HOST=""; REGION_FLAG=""; PORT_FLAG=""
REGION=""; PORT=""
REGION_DEFAULT="us-ord"; PORT_DEFAULT="8080"
# The region this installer shipped as a hardcoded default before it learned to
# detect one. A unit still carrying it was never an operator's choice, so it must
# not outrank detection — see resolve_region.
REGION_LEGACY_DEFAULT="us-ord"
while [ $# -gt 0 ]; do
  case "$1" in
    --public-host) PUBLIC_HOST="$2"; shift 2;;
    --region)      REGION_FLAG="$2"; shift 2;;
    --port)        PORT_FLAG="$2"; shift 2;;
    -h|--help)     sed -n '2,20p' "$0"; exit 0;;
    *) echo "unknown arg: $1"; exit 1;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ETC=/etc/vm-repl
LIB=/var/lib/vm-repl
OPT=/opt/vm-repl
UNIT_PATH=/etc/systemd/system/applianced.service
ENVFILE="$ETC/applianced.env"

# ---------------------------------------------------------------------------
# Settings that belong to the OPERATOR, not to the build.
#
# These used to be baked straight into the systemd unit, which this script
# rewrites wholesale on every run — so upgrading silently reset a corrected
# region (and port) back to the built-in default. They now live in an
# EnvironmentFile that is seeded once and never overwritten, so re-running the
# installer refreshes the unit (new flags, new binary paths) without touching
# anything the operator chose.
# ---------------------------------------------------------------------------

# env_file_get KEY — the stored value, or empty.
env_file_get() {
  [ -f "$ENVFILE" ] || return 0
  sed -n "s/^$1=//p" "$ENVFILE" | tail -1
}

# unit_get FLAG [PATH] — the value of a flag in an already-installed unit, so an
# install predating the env file can have its settings adopted rather than lost.
unit_get() {
  local flag="$1" unit="${2:-$UNIT_PATH}"
  [ -f "$unit" ] || return 0
  sed -n "s/.*-$flag[[:space:]]\{1,\}\([^[:space:]\\\\]*\).*/\1/p" "$unit" | tail -1
}

# detect_region — ask the Linode Metadata service which region we are in. The
# appliance already reads this endpoint to learn its own instance id, so the
# region comes from the same place rather than being guessed. Prints nothing and
# never fails when metadata is unavailable (not every image/region provides it).
detect_region() {
  local tok region
  tok="$(curl -sX PUT -H "Metadata-Token-Expiry-Seconds: 60" --max-time 5 \
        http://169.254.169.254/v1/token 2>/dev/null || true)"
  [ -n "$tok" ] || return 0
  region="$(curl -s -H "Metadata-Token: $tok" -H "Accept: application/json" --max-time 5 \
           http://169.254.169.254/v1/instance 2>/dev/null | jq -r '.region // empty' 2>/dev/null || true)"
  [ -n "$region" ] && printf '%s' "$region"
  return 0
}

# resolve_region [UNIT_PATH] — highest precedence first:
#   1. --region on this invocation
#   2. the stored value (an operator's earlier choice must survive upgrades)
#   3. a value already in the unit, unless it is the legacy hardcoded default
#   4. the Linode Metadata service
#   5. the value in the unit even if it is the legacy default
#   6. the built-in default
resolve_region() {
  local unit="${1:-$UNIT_PATH}" stored detected in_unit
  REGION_SRC=""
  if [ -n "$REGION_FLAG" ]; then REGION="$REGION_FLAG"; REGION_SRC="the --region flag"; return 0; fi
  stored="$(env_file_get REGION)"
  if [ -n "$stored" ]; then REGION="$stored"; REGION_SRC="$ENVFILE"; return 0; fi
  in_unit="$(unit_get region "$unit")"
  if [ -n "$in_unit" ] && [ "$in_unit" != "$REGION_LEGACY_DEFAULT" ]; then
    REGION="$in_unit"; REGION_SRC="the existing service unit"; return 0
  fi
  detected="$(detect_region)"
  if [ -n "$detected" ]; then REGION="$detected"; REGION_SRC="the Linode Metadata service"; return 0; fi
  if [ -n "$in_unit" ]; then REGION="$in_unit"; REGION_SRC="the existing service unit"; return 0; fi
  REGION="$REGION_DEFAULT"; REGION_SRC="the built-in default"
  return 0
}

# resolve_port [UNIT_PATH] — flag, then stored, then the installed unit, then the
# default. No legacy-default exception: 8080 is a real default, not a mistake.
resolve_port() {
  local unit="${1:-$UNIT_PATH}" stored in_unit
  PORT_SRC=""
  if [ -n "$PORT_FLAG" ]; then PORT="$PORT_FLAG"; PORT_SRC="the --port flag"; return 0; fi
  stored="$(env_file_get PORT)"
  if [ -n "$stored" ]; then PORT="$stored"; PORT_SRC="$ENVFILE"; return 0; fi
  # -listen carries a ":PORT" form, so it needs its own pattern rather than
  # unit_get. Guard the file first: under `set -e` a failing sed inside a command
  # substitution aborts the whole install.
  in_unit=""
  if [ -f "$unit" ]; then
    in_unit="$(sed -n 's/.*-listen[[:space:]]\{1,\}:\([0-9]\{1,\}\).*/\1/p' "$unit" 2>/dev/null | tail -1 || true)"
  fi
  if [ -n "$in_unit" ]; then PORT="$in_unit"; PORT_SRC="the existing service unit"; return 0; fi
  PORT="$PORT_DEFAULT"; PORT_SRC="the built-in default"
  return 0
}

# seed_env_file — write the operator settings ONCE. Never overwrites: that is the
# whole point. systemd does not shell-expand these, so no quoting.
seed_env_file() {
  [ -f "$ENVFILE" ] && return 0
  cat > "$ENVFILE" <<ENV
# vm-replication appliance settings. Edit here, then: systemctl restart applianced
# The installer seeds this file once and never overwrites it, so your changes
# survive upgrades.
REGION=$REGION
PORT=$PORT
ENV
  chmod 600 "$ENVFILE"
  return 0
}

# Test hook: sourcing with VMREPL_INSTALL_LIB=1 loads the helpers above and
# returns before the root check and any real work, so the resolution logic can be
# unit-tested without root or a live metadata service.
if [ -n "${VMREPL_INSTALL_LIB:-}" ]; then return 0 2>/dev/null || exit 0; fi

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }

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
# Upgrade path: re-running the installer after `git pull` must NOT silently
# reuse stale binaries. If this is a git checkout and the last commit is newer
# than the built applianced, rebuild.
if [ "$NEED_BUILD" -eq 0 ] && command -v git >/dev/null 2>&1 && [ -d "$ROOT/.git" ]; then
  commit_ts="$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || echo 0)"
  bin_ts="$(stat -c %Y "$ROOT/bin/applianced" 2>/dev/null || echo 0)"
  if [ "${commit_ts:-0}" -gt "${bin_ts:-0}" ]; then
    echo ">> Repository is newer than the built binaries — rebuilding"
    NEED_BUILD=1
  fi
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

# --- resolve operator settings, then persist them once ---
# Done after $ETC exists (the settings file lives there) and before the unit is
# written. Re-running the installer re-reads the stored values, so an upgrade
# refreshes the unit without discarding anything the operator chose.
resolve_region
resolve_port
echo ">> Region: $REGION (from $REGION_SRC)"
echo ">> Console port: $PORT (from $PORT_SRC)"
if [ ! -f "$ENVFILE" ]; then
  seed_env_file
  echo ">> Saved these to $ENVFILE — edit there and restart to change them; upgrades will not overwrite it"
fi
install -m 0755 "$ROOT/bin/applianced" /usr/local/bin/applianced
install -m 0755 "$ROOT/bin/agent" "$OPT/agent"                  # served to sources
install -m 0755 "$ROOT/bin/receiver" "$OPT/receiver"            # standalone receiver, for manual/testing use
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
# REGION and PORT come from the EnvironmentFile, NOT from this unit: the
# installer rewrites this file on every upgrade, so anything baked in here would
# silently discard the operator's choice. Change them in $ENVFILE and restart.
EnvironmentFile=$ENVFILE
ExecStart=/usr/local/bin/applianced \\
  -listen :\${PORT} \\
  -data-dir $LIB \\
  -public-host $PUBLIC_HOST \\
  -region \${REGION} \\
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
# `enable --now` does NOT restart an already-running service, so an upgrade
# would keep the old binary in memory. Restart to pick up what we installed.
systemctl restart applianced.service

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
 Guide:     https://$PUBLIC_HOST:$PORT/documentation
 Password:  $( [ -f "$PWFILE" ] && cat "$PWFILE" || echo "see: journalctl -u applianced" )
 Cert SHA-256 (verify in the browser's certificate dialog):
   ${FPR:-see: journalctl -u applianced}

 The browser warns about the self-signed certificate on first visit — verify
 the fingerprint above, then sign in. The Guide covers everything from there.

 Forgot the password?  sudo /usr/local/bin/applianced -data-dir $LIB -show-password
 Logs:                 journalctl -u applianced -f
==========================================================
EOF
