#!/usr/bin/env bash
# controld-smoke.sh — end-to-end check of the control plane + agent reporting.
# Starts controld, creates a job via replctl, runs a file-image replication that
# reports to the control plane, then verifies status and Prometheus metrics.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
PORT="${PORT:-18088}"
RPORT="${RPORT:-14445}"
TOKEN="test-token"
trap 'rm -rf "$WORK"; for p in ${CTRL_PID:-} ${RECV_PID:-}; do kill "$p" 2>/dev/null || true; done' EXIT

echo "== Building =="
( cd "$ROOT" && go build -o "$WORK/agent" ./cmd/agent && go build -o "$WORK/receiver" ./cmd/receiver \
   && go build -o "$WORK/controld" ./cmd/controld && go build -o "$WORK/replctl" ./cmd/replctl )

echo "== Certs + images =="
DAYS=30 "$ROOT/scripts/gen-certs.sh" "$WORK/certs" localhost >/dev/null
dd if=/dev/urandom of="$WORK/source.img" bs=1M count=32 status=none
: > "$WORK/target.img"
cd "$WORK"

export CONTROL_URL="http://127.0.0.1:$PORT" CONTROL_TOKEN="$TOKEN"

echo "== Start controld =="
./controld -listen "127.0.0.1:$PORT" -db "$WORK/controld.db" -token "$TOKEN" >"$WORK/controld.log" 2>&1 &
CTRL_PID=$!
for _ in $(seq 1 50); do grep -q "controld listening" "$WORK/controld.log" && break; sleep 0.1; done

echo "== Create job via replctl =="
./replctl register -name web01 -role source -device "$WORK/source.img"
JOB_OUT=$(./replctl create-job -name mig-web01 -target "127.0.0.1:$RPORT" -rpo 60)
echo "   $JOB_OUT"
JOB_ID=$(echo "$JOB_OUT" | grep -o 'id=[0-9]*' | head -1 | cut -d= -f2)
[ -n "$JOB_ID" ] || { echo "FAIL: no job id"; exit 1; }

echo "== Receiver + agent (reporting to control plane) =="
./receiver -listen "127.0.0.1:$RPORT" -device "$WORK/target.img" -once \
  -cert certs/receiver.crt -key certs/receiver.key -ca certs/ca.crt >"$WORK/recv.log" 2>&1 &
RECV_PID=$!
for _ in $(seq 1 50); do grep -q "receiver listening" "$WORK/recv.log" && break; sleep 0.1; done

./agent -device "$WORK/source.img" -target "127.0.0.1:$RPORT" -server-name localhost \
  -manifest "$WORK/source.cbt" -control-job "$JOB_ID" \
  -cert certs/agent.crt -key certs/agent.key -ca certs/ca.crt
wait "$RECV_PID"; RECV_PID=""

cmp -s "$WORK/source.img" "$WORK/target.img" || { echo "FAIL: images differ"; exit 1; }
echo "   OK: target matches source"

echo "== Verify control plane recorded the sync =="
./replctl jobs
STATUS=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$CONTROL_URL/api/v1/status")
echo "$STATUS" | grep -q '"total_syncs":1' || { echo "FAIL: sync not recorded ($STATUS)"; exit 1; }
echo "   OK: control plane shows total_syncs=1"

METRICS=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$CONTROL_URL/metrics")
echo "$METRICS" | grep -q 'vm_repl_rpo_seconds{job="mig-web01"' || { echo "FAIL: metrics missing"; exit 1; }
echo "   OK: /metrics exposes RPO gauge"

echo
echo "CONTROL PLANE SMOKE PASSED"
