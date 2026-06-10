#!/usr/bin/env bash
# install.sh — install vm-replication binaries, config, and systemd units.
#
# Usage:
#   sudo scripts/install.sh agent      # source server
#   sudo scripts/install.sh receiver   # target/staging server
#   sudo scripts/install.sh controld   # control plane host
#
# It builds the binaries (if Go is present and ./bin is missing), installs them
# to /usr/local/bin, seeds /etc/vm-repl/<role>.env from the example (without
# overwriting an existing one), installs the unit files, and runs daemon-reload.
# It does NOT enable/start units — it prints the commands so you can review the
# config first.
set -euo pipefail

ROLE="${1:-}"
case "$ROLE" in
  agent|receiver|controld) ;;
  *) echo "usage: $0 {agent|receiver|controld}"; exit 1;;
esac
[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BINDIR=/usr/local/bin
ETCDIR=/etc/vm-repl
LIBDIR=/var/lib/vm-repl
UNITDIR=/etc/systemd/system

# Ensure binaries exist (build if needed).
if [ ! -x "$ROOT/bin/agent" ] || [ ! -x "$ROOT/bin/receiver" ] || [ ! -x "$ROOT/bin/controld" ]; then
  if command -v go >/dev/null 2>&1; then
    echo ">> Building binaries"
    ( cd "$ROOT" && make build >/dev/null && CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/controld ./cmd/controld )
  else
    echo "Go not found and ./bin is incomplete; build the binaries first (make build)."; exit 1
  fi
fi

install -d "$ETCDIR" "$LIBDIR"

install_env() {
  local name="$1"
  if [ -f "$ETCDIR/$name.env" ]; then
    echo ">> Keeping existing $ETCDIR/$name.env"
  else
    install -m 0640 "$ROOT/deploy/systemd/$name.env.example" "$ETCDIR/$name.env"
    echo ">> Installed $ETCDIR/$name.env (EDIT THIS before starting)"
  fi
}

install_unit() {
  install -m 0644 "$ROOT/deploy/systemd/$1" "$UNITDIR/$1"
  echo ">> Installed $UNITDIR/$1"
}

case "$ROLE" in
  agent)
    install -m 0755 "$ROOT/bin/agent" "$BINDIR/agent"
    install_env agent
    install_unit vm-repl-agent.service
    install_unit vm-repl-agent.timer
    ENABLE="systemctl enable --now vm-repl-agent.timer"
    ;;
  receiver)
    install -m 0755 "$ROOT/bin/receiver" "$BINDIR/receiver"
    install_env receiver
    install_unit vm-repl-receiver.service
    ENABLE="systemctl enable --now vm-repl-receiver.service"
    ;;
  controld)
    install -m 0755 "$ROOT/bin/controld" "$BINDIR/controld"
    install_env controld
    install_unit vm-repl-controld.service
    ENABLE="systemctl enable --now vm-repl-controld.service"
    ;;
esac

systemctl daemon-reload
echo
echo "Installed role '$ROLE'. Next:"
echo "  1. Edit $ETCDIR/$ROLE.env and place TLS certs in $ETCDIR (see scripts/gen-certs.sh)."
echo "  2. Enable it:  $ENABLE"
[ "$ROLE" = agent ] && echo "  3. Trigger one pass now:  systemctl start vm-repl-agent.service"
