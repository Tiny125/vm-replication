#!/usr/bin/env bash
# linode-provision.sh — stand up a Linode "staging" target for replication.
#
# It creates a Linode instance, attaches an empty RAW disk sized to your source
# (plus an optional swap disk), and boots the instance into Rescue Mode (Finnix)
# so the receiver daemon can write incoming blocks straight to the raw disk at
# /dev/sda — exactly the AWS MGN "staging area" pattern.
#
# Requirements: bash, curl, jq, and a Linode API token in LINODE_TOKEN.
#
# Usage:
#   LINODE_TOKEN=xxxxx scripts/linode-provision.sh \
#       --label mig-web01 --region us-ord --type g6-standard-2 \
#       --disk-mb 81920 --swap-mb 512 --root-pass 'S3cret!'
#
# After it prints the rescue SSH details:
#   1. scp the receiver binary + certs to the rescue shell
#   2. run: receiver -listen :4444 -device /dev/sda -cert ... -key ... -ca ...
#   3. run the agent on your source pointing at this Linode's public IP
#   4. see docs/CUTOVER.md to convert the disk and boot it for real
set -euo pipefail

API="https://api.linode.com/v4"
LABEL=""; REGION="us-ord"; TYPE="g6-standard-2"
DISK_MB=""; SWAP_MB="512"; ROOT_PASS=""
IMAGE_FOR_RESCUE_ONLY="linode/debian12" # only used to satisfy create; we boot rescue

usage() { sed -n '2,30p' "$0"; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --label)     LABEL="$2"; shift 2;;
    --region)    REGION="$2"; shift 2;;
    --type)      TYPE="$2"; shift 2;;
    --disk-mb)   DISK_MB="$2"; shift 2;;
    --swap-mb)   SWAP_MB="$2"; shift 2;;
    --root-pass) ROOT_PASS="$2"; shift 2;;
    -h|--help)   usage;;
    *) echo "unknown arg: $1"; usage;;
  esac
done

: "${LINODE_TOKEN:?set LINODE_TOKEN to your Linode API token}"
[ -n "$LABEL" ]   || { echo "--label required"; exit 1; }
[ -n "$DISK_MB" ] || { echo "--disk-mb required (>= source disk size in MiB)"; exit 1; }
[ -n "$ROOT_PASS" ] || { echo "--root-pass required (rescue/root password)"; exit 1; }

api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -fsS -X "$method" "$API$path" \
      -H "Authorization: Bearer $LINODE_TOKEN" \
      -H "Content-Type: application/json" -d "$body"
  else
    curl -fsS -X "$method" "$API$path" -H "Authorization: Bearer $LINODE_TOKEN"
  fi
}

echo ">> Creating instance '$LABEL' ($TYPE in $REGION), unbooted"
CREATE_BODY=$(jq -n --arg label "$LABEL" --arg region "$REGION" --arg type "$TYPE" \
  --arg img "$IMAGE_FOR_RESCUE_ONLY" --arg pass "$ROOT_PASS" \
  '{label:$label, region:$region, type:$type, image:$img, root_pass:$pass, booted:false}')
LINODE_ID=$(api POST /linode/instances "$CREATE_BODY" | jq -r '.id')
echo "   linode id = $LINODE_ID"

wait_running_offline() {
  for _ in $(seq 1 60); do
    s=$(api GET "/linode/instances/$LINODE_ID" | jq -r '.status')
    [ "$s" = "offline" ] && return 0
    sleep 5
  done
  echo "timed out waiting for instance to settle"; exit 1
}
echo ">> Waiting for instance to provision"
wait_running_offline

# The create step lays down a default distro disk; for a clean block target we
# want our own RAW disk. Remove auto-created disks, then create raw + swap.
echo ">> Clearing auto-created disks"
for d in $(api GET "/linode/instances/$LINODE_ID/disks" | jq -r '.data[].id'); do
  api DELETE "/linode/instances/$LINODE_ID/disks/$d" >/dev/null || true
done

echo ">> Creating RAW data disk (${DISK_MB} MiB)"
RAW_BODY=$(jq -n --argjson size "$DISK_MB" '{label:"replica-raw", filesystem:"raw", size:$size}')
RAW_DISK_ID=$(api POST "/linode/instances/$LINODE_ID/disks" "$RAW_BODY" | jq -r '.id')
echo "   raw disk id = $RAW_DISK_ID"

SWAP_DISK_ID=""
if [ "${SWAP_MB:-0}" -gt 0 ]; then
  echo ">> Creating swap disk (${SWAP_MB} MiB)"
  SWAP_BODY=$(jq -n --argjson size "$SWAP_MB" '{label:"replica-swap", filesystem:"swap", size:$size}')
  SWAP_DISK_ID=$(api POST "/linode/instances/$LINODE_ID/disks" "$SWAP_BODY" | jq -r '.id')
  echo "   swap disk id = $SWAP_DISK_ID"
fi

echo ">> Booting into Rescue Mode with raw disk on /dev/sda"
RESCUE_BODY=$(jq -n --argjson raw "$RAW_DISK_ID" '{devices:{sda:{disk_id:$raw}}}')
api POST "/linode/instances/$LINODE_ID/rescue" "$RESCUE_BODY" >/dev/null

PUBIP=$(api GET "/linode/instances/$LINODE_ID" | jq -r '.ipv4[0]')

cat <<EOF

================ STAGING TARGET READY ================
 Linode ID    : $LINODE_ID
 Public IP    : $PUBIP
 Raw disk id  : $RAW_DISK_ID  -> /dev/sda in rescue
 Swap disk id : ${SWAP_DISK_ID:-<none>}
 Rescue login : ssh root@$PUBIP   (password: the --root-pass you set)

 Next steps:
   1. Wait ~30s for Rescue (Finnix) to boot, then SSH in.
   2. Copy the static receiver binary + certs (certs/receiver.* and ca.crt):
        scp receiver certs/receiver.crt certs/receiver.key certs/ca.crt root@$PUBIP:/root/
   3. On the Linode, start the receiver against the raw disk:
        ./receiver -listen :4444 -device /dev/sda \\
            -cert receiver.crt -key receiver.key -ca ca.crt
   4. On your SOURCE server, run the agent:
        ./agent -device /dev/sdX -target $PUBIP:4444 -server-name $PUBIP \\
            -cert agent.crt -key agent.key -ca ca.crt
      (regenerate receiver certs with this IP as the SAN:
        scripts/gen-certs.sh certs $PUBIP )
   5. When replication lag is acceptable, follow docs/CUTOVER.md to make the
      disk bootable and switch the instance to boot from it.

 Save these IDs for cutover:
   LINODE_ID=$LINODE_ID RAW_DISK_ID=$RAW_DISK_ID SWAP_DISK_ID=${SWAP_DISK_ID:-}
======================================================
EOF
