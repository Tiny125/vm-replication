#!/usr/bin/env bash
# dm-era-setup.sh — wrap a data device in a device-mapper "era" target so the
# agent can do true change-block tracking (only read/ship blocks the kernel says
# were written) instead of rescanning the whole disk each cycle.
#
# Requires: root, device-mapper (dmsetup), and thin-provisioning-tools
# (era_invalidate / era_check) — Debian/Ubuntu: apt install thin-provisioning-tools.
#
# You need a SMALL separate metadata device (a few MiB is plenty; e.g. an LVM LV
# or spare partition). It will be ZEROED.
#
# Usage:
#   sudo scripts/dm-era-setup.sh --name vmrepl-data \
#        --data /dev/sda --meta /dev/vg/era_meta [--era-block-sectors 8]
#
# Then run the agent against the mapped device:
#   ./agent -device /dev/mapper/vmrepl-data --cbt dmera \
#           --dmera-name vmrepl-data --dmera-meta /dev/vg/era_meta \
#           -target <ip>:4444 ...
#
# Teardown:  sudo dmsetup remove vmrepl-data
set -euo pipefail

NAME=""; DATA=""; META=""; ERA_SECTORS=8
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    --data) DATA="$2"; shift 2;;
    --meta) META="$2"; shift 2;;
    --era-block-sectors) ERA_SECTORS="$2"; shift 2;;
    -h|--help) sed -n '2,28p' "$0"; exit 0;;
    *) echo "unknown arg: $1"; exit 1;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
[ -n "$NAME" ] && [ -b "$DATA" ] && [ -b "$META" ] || { echo "--name, --data <blockdev>, --meta <blockdev> required"; exit 1; }
for t in dmsetup blockdev; do command -v "$t" >/dev/null || { echo "missing tool: $t"; exit 1; }; done

if dmsetup info "$NAME" >/dev/null 2>&1; then
  echo "device-mapper target '$NAME' already exists; remove it first (dmsetup remove $NAME)"; exit 1
fi

SECTORS=$(blockdev --getsz "$DATA")
echo ">> Data device $DATA = $SECTORS sectors; era block = $ERA_SECTORS sectors ($((ERA_SECTORS*512)) bytes)"

echo ">> Zeroing era metadata header on $META (first 4 MiB)"
dd if=/dev/zero of="$META" bs=1M count=4 conv=fsync status=none

echo ">> Creating dm-era target '$NAME'"
# table: <start> <len> era <metadata_dev> <origin_dev> <era_block_size_sectors>
dmsetup create "$NAME" --table "0 $SECTORS era $META $DATA $ERA_SECTORS"

echo
echo "Created /dev/mapper/$NAME"
echo "Use it as the agent's source device with the dm-era backend:"
echo "  ./agent -device /dev/mapper/$NAME --cbt dmera \\"
echo "          --dmera-name $NAME --dmera-meta $META --dmera-era-block-sectors $ERA_SECTORS \\"
echo "          -target <ip>:4444 -server-name <ip> -cert agent.crt -key agent.key -ca ca.crt"
echo
echo "NOTE: anything that mounts/uses $DATA must now use /dev/mapper/$NAME instead,"
echo "      so the era target sees the writes. Teardown: dmsetup remove $NAME"
