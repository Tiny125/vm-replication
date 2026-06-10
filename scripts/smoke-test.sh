#!/usr/bin/env bash
# smoke-test.sh — end-to-end proof of the replication slice on a single host,
# using file-backed images instead of real block devices. It:
#   1. builds the binaries
#   2. generates mTLS certs
#   3. creates a source image with random data
#   4. runs a FULL sync to a target image and verifies they match
#   5. mutates a few blocks in the source
#   6. runs a DELTA sync and verifies only changed blocks moved + images match
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; [ -n "${RECV_PID:-}" ] && kill "$RECV_PID" 2>/dev/null || true' EXIT

SIZE_MB="${SIZE_MB:-64}"
PORT="${PORT:-14444}"
BS=$((4 * 1024 * 1024)) # 4 MiB blocks, matches the agent default

echo "== Building binaries =="
( cd "$ROOT" && go build -o "$WORK/agent" ./cmd/agent && go build -o "$WORK/receiver" ./cmd/receiver )

echo "== Generating certs =="
DAYS=30 "$ROOT/scripts/gen-certs.sh" "$WORK/certs" localhost >/dev/null

echo "== Creating source image (${SIZE_MB} MiB) =="
dd if=/dev/urandom of="$WORK/source.img" bs=1M count="$SIZE_MB" status=none
: > "$WORK/target.img"

cd "$WORK"

start_receiver() {
  : > "$WORK/receiver.log"
  "$WORK/receiver" -listen "127.0.0.1:$PORT" -device "$WORK/target.img" \
    -manifest "$WORK/target.cbt" -once \
    -cert certs/receiver.crt -key certs/receiver.key -ca certs/ca.crt \
    >"$WORK/receiver.log" 2>&1 &
  RECV_PID=$!
  # Wait for the listener to bind (avoid a spurious TCP probe that would be
  # consumed as a failed --once session).
  for _ in $(seq 1 50); do
    if grep -q "receiver listening" "$WORK/receiver.log" 2>/dev/null; then return; fi
    sleep 0.1
  done
  echo "receiver did not come up"; cat "$WORK/receiver.log"; exit 1
}

run_agent() {
  "$WORK/agent" -device "$WORK/source.img" -target "127.0.0.1:$PORT" \
    -server-name localhost -manifest "$WORK/source.cbt" "$@" \
    -cert certs/agent.crt -key certs/agent.key -ca certs/ca.crt
}

echo "== FULL sync =="
start_receiver
run_agent
wait "$RECV_PID"; RECV_PID=""

if cmp -s "$WORK/source.img" "$WORK/target.img"; then
  echo "   OK: target matches source after full sync"
else
  echo "   FAIL: images differ after full sync"; exit 1
fi

echo "== Mutating 3 blocks in source =="
for blk in 1 5 9; do
  dd if=/dev/urandom of="$WORK/source.img" bs="$BS" count=1 seek="$blk" conv=notrunc status=none
done

echo "== DELTA sync =="
start_receiver
DELTA_LOG="$(run_agent 2>&1)"
echo "$DELTA_LOG" | sed 's/^/   agent: /'
wait "$RECV_PID"; RECV_PID=""

if cmp -s "$WORK/source.img" "$WORK/target.img"; then
  echo "   OK: target matches source after delta sync"
else
  echo "   FAIL: images differ after delta sync"; exit 1
fi

if echo "$DELTA_LOG" | grep -q "3/.* blocks changed"; then
  echo "   OK: agent shipped exactly the 3 changed blocks"
else
  echo "   FAIL: expected 3 changed blocks in delta sync"; exit 1
fi

echo
echo "ALL CHECKS PASSED"
