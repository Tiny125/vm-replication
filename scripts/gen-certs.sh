#!/usr/bin/env bash
# gen-certs.sh — create a tiny internal CA plus agent and receiver certificates
# for mutually-authenticated TLS. This is the lightweight built-in CA referenced
# in the architecture; for production, issue short-lived certs from the control
# server instead of long-lived files.
#
# Usage:
#   scripts/gen-certs.sh [OUTPUT_DIR] [RECEIVER_CN]
#
#   OUTPUT_DIR    where to write the PEM files (default: ./certs)
#   RECEIVER_CN   SAN/CN the agent will verify on the receiver cert
#                 (default: localhost). Use the receiver's IP/hostname in prod,
#                 e.g. the Linode's public IP or a DNS name.
set -euo pipefail

OUT="${1:-certs}"
RECEIVER_CN="${2:-localhost}"
DAYS="${DAYS:-825}"

mkdir -p "$OUT"
cd "$OUT"

echo ">> Generating internal CA"
openssl req -x509 -newkey rsa:4096 -nodes -keyout ca.key -out ca.crt \
  -days "$DAYS" -subj "/CN=vm-replication-ca" >/dev/null 2>&1

gen_cert() {
  local name="$1" cn="$2" san="$3"
  echo ">> Generating $name certificate (CN=$cn)"
  openssl req -newkey rsa:2048 -nodes -keyout "$name.key" -out "$name.csr" \
    -subj "/CN=$cn" >/dev/null 2>&1
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out "$name.crt" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth,clientAuth\n' "$san") \
    >/dev/null 2>&1
  rm -f "$name.csr"
}

# Receiver: verified by SAN, used as a TLS server (and client for reverse sync).
case "$RECEIVER_CN" in
  *[0-9].[0-9]*) RECEIVER_SAN="IP:$RECEIVER_CN" ;;
  *)             RECEIVER_SAN="DNS:$RECEIVER_CN" ;;
esac
gen_cert receiver "$RECEIVER_CN" "$RECEIVER_SAN"

# Agent: identified by CN, used as a TLS client.
gen_cert agent "vm-replication-agent" "DNS:vm-replication-agent"

rm -f ca.srl
echo ">> Done. Certificates written to $(pwd)"
echo "   CA:       ca.crt"
echo "   Receiver: receiver.crt / receiver.key  (SAN=$RECEIVER_SAN)"
echo "   Agent:    agent.crt / agent.key"
