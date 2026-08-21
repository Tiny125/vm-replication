#!/usr/bin/env bash
# machine-convert-test.sh — unit tests for machine-convert.sh helpers that don't
# need root, block devices or real mounts.
#
# It loads machine-convert.sh in "library mode" (VMREPL_CONVERT_LIB=1), which
# defines the helper functions and returns before doing any real work, then
# exercises them against a scratch directory.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
VMREPL_CONVERT_LIB=1 source "$HERE/machine-convert.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# 1) A missing mount point is created as a directory.
ensure_dir_mount "$WORK/root/proc"
[ -d "$WORK/root/proc" ] || fail "ensure_dir_mount did not create a missing directory"

# 2) A stray NON-directory at the mount point (the exact bug from migration #2:
#    "mount point is not a directory") is replaced with a directory.
mkdir -p "$WORK/root2"
: > "$WORK/root2/proc"            # a regular file where /proc should be a dir
[ -f "$WORK/root2/proc" ] || fail "test setup: expected a file at proc"
ensure_dir_mount "$WORK/root2/proc"
[ -d "$WORK/root2/proc" ] || fail "ensure_dir_mount did not replace a stray file with a directory"

# 3) An existing directory is left intact (idempotent).
mkdir -p "$WORK/root3/sys"
touch "$WORK/root3/sys/keep"
ensure_dir_mount "$WORK/root3/sys"
[ -d "$WORK/root3/sys" ] && [ -f "$WORK/root3/sys/keep" ] || fail "ensure_dir_mount clobbered an existing directory"

# 3b) A DANGLING SYMLINK at the mount point (left by a heavy fsck repair; the
#     exact "mkdir: cannot create directory '…/proc': File exists" crash from
#     migration testest — `-e` follows symlinks so the old check missed it) is
#     replaced with a real directory.
mkdir -p "$WORK/root4"
ln -s /nonexistent-target "$WORK/root4/proc"
ensure_dir_mount "$WORK/root4/proc"
[ -d "$WORK/root4/proc" ] && [ ! -L "$WORK/root4/proc" ] || fail "ensure_dir_mount did not replace a dangling symlink"

# 3c) A symlink pointing AT a directory is also replaced — mounts must land on
#     a real directory in the image, not wherever a symlink points.
mkdir -p "$WORK/root5/elsewhere"
ln -s elsewhere "$WORK/root5/sys"
ensure_dir_mount "$WORK/root5/sys"
[ -d "$WORK/root5/sys" ] && [ ! -L "$WORK/root5/sys" ] || fail "ensure_dir_mount did not replace a symlink-to-directory"
[ -d "$WORK/root5/elsewhere" ] || fail "ensure_dir_mount must not delete the symlink's target"

# 4) ensure_stage_dir: a heavy fsck repair (e.g. after an interrupted
#    replication pass) can drop the image's /root — the convert must recreate
#    it (0700) instead of crashing at "cat > $MNT/root/.convert-inner.sh".
mkdir -p "$WORK/img1"
ensure_stage_dir "$WORK/img1" >/dev/null
[ -d "$WORK/img1/root" ] || fail "ensure_stage_dir did not recreate a missing /root"
perms=$(stat -c %a "$WORK/img1/root")
[ "$perms" = "700" ] || fail "recreated /root should be 0700, got $perms"

# 5) An existing /root (with contents) is left untouched.
mkdir -p "$WORK/img2/root"
touch "$WORK/img2/root/.bashrc"
chmod 750 "$WORK/img2/root"
ensure_stage_dir "$WORK/img2" >/dev/null
[ -f "$WORK/img2/root/.bashrc" ] || fail "ensure_stage_dir clobbered an existing /root"
[ "$(stat -c %a "$WORK/img2/root")" = "750" ] || fail "ensure_stage_dir changed an existing /root's permissions"

# 6) A stray file where /root should be is replaced with a directory.
mkdir -p "$WORK/img3"
: > "$WORK/img3/root"
ensure_stage_dir "$WORK/img3" >/dev/null
[ -d "$WORK/img3/root" ] || fail "ensure_stage_dir did not replace a stray file at /root"

# 8) disable_stale_swap: fstab swap entries pointing at devices that were NOT
#    migrated (a separate swap disk — Linode's /dev/sdb, or a UUID that no
#    longer resolves) must be commented out, or the migrated instance stalls
#    ~90s at boot waiting for a ghost device. Root and comment lines untouched.
#
#    blkid is STUBBED here. These are host-independent unit tests, and the real
#    blkid would answer from the machine running the tests — which is exactly the
#    bug this covers: every Linode's swap image carries the SAME UUID
#    (f1408ea6-…), so on a Linode appliance the real blkid resolves the source's
#    stale swap UUID to the APPLIANCE's own /dev/sdb and the entry looks alive.
#    The stub models a converting host that has its own swap disk at /dev/sdb
#    with that shared UUID, while the migrated disk is /dev/sdc.
DEV=/dev/sdc
blkid() {
  case "$*" in
    "-U f1408ea6-59a0-11ed-bc9d-525400000001") echo /dev/sdb; return 0 ;;  # the HOST's swap — not migrated
    "-U 5ea51b1e-0000-4000-8000-000000000001") echo /dev/sdc2; return 0 ;; # swap ON the migrated disk
    "-L linode-swap") echo /dev/sdb; return 0 ;;                            # same collision by label
    *) return 2 ;;                                                          # unknown: resolves nowhere
  esac
}

FSTAB="$WORK/fstab"
cat > "$FSTAB" <<'EOF'
# /etc/fstab: static file system information.
UUID=16829997-c4bf-8fc8-89a5-e49ca9f84956 / ext4 errors=remount-ro 0 1
UUID=f1408ea6-59a0-11ed-bc9d-525400000001 none swap sw 0 0
/dev/sdb none swap sw 0 0
EOF
disable_stale_swap "$FSTAB" >/dev/null
grep -q '^UUID=16829997.* / ext4' "$FSTAB" || fail "disable_stale_swap must not touch the root entry"
grep -q '^# /etc/fstab' "$FSTAB" || fail "disable_stale_swap must keep comments"
grep -q '^# vmrepl: disabled.*UUID=f1408ea6' "$FSTAB" || fail "missing-UUID swap entry should be disabled"
grep -q '^# vmrepl: disabled.*/dev/sdb' "$FSTAB" || fail "separate-disk swap entry should be disabled"
if grep -qE '^[^#].*swap' "$FSTAB"; then fail "no active swap entries should remain"; fi

# 8b) A file with no swap entries is left byte-identical.
FSTAB2="$WORK/fstab2"
printf 'UUID=abc / ext4 defaults 0 1\n' > "$FSTAB2"
cp "$FSTAB2" "$FSTAB2.orig"
disable_stale_swap "$FSTAB2" >/dev/null
cmp -s "$FSTAB2" "$FSTAB2.orig" || fail "a swap-free fstab must be untouched"

# 8c) Swap that lives ON the migrated disk must be KEPT — it comes along with the
#     migration, so disabling it would cost the migrated system its swap.
FSTAB3="$WORK/fstab3"
cat > "$FSTAB3" <<'EOF'
UUID=5ea51b1e-0000-4000-8000-000000000001 none swap sw 0 0
EOF
disable_stale_swap "$FSTAB3" >/dev/null
grep -q '^UUID=5ea51b1e.* swap' "$FSTAB3" || fail "swap on the MIGRATED disk must stay enabled"

# 8d) The shared-UUID trap, stated directly: a swap UUID that resolves on the
#     CONVERTING HOST but is not part of the migrated disk must still be
#     disabled. Resolving somewhere is not evidence it was migrated.
swap_spec_exists "UUID=f1408ea6-59a0-11ed-bc9d-525400000001" &&
  fail "a swap UUID resolving to the converting host's own disk must NOT count as migrated"
swap_spec_exists "UUID=5ea51b1e-0000-4000-8000-000000000001" ||
  fail "a swap UUID resolving to a partition of the migrated disk must count as migrated"
swap_spec_exists "LABEL=linode-swap" &&
  fail "a swap LABEL resolving to the converting host's own disk must NOT count as migrated"
swap_spec_exists "UUID=00000000-0000-0000-0000-000000000000" &&
  fail "a swap UUID that resolves nowhere must NOT count as migrated"

unset -f blkid
unset DEV

# 10) filter_vgs_on_disk: only volume groups whose PVs live on the MIGRATED
#     disk (kernel partitions or kpartx mappings) may be activated — the
#     appliance's own LVM (if any) must never be touched.
OUT=$(printf '  vg_root /dev/sdc2\n  vg_other /dev/sda3\n  vg_map /dev/mapper/sdc2\n  vg_root /dev/sdc3\n' | filter_vgs_on_disk /dev/sdc sdc)
echo "$OUT" | grep -qx 'vg_root' || fail "VG on the migrated disk's partition should be selected"
echo "$OUT" | grep -qx 'vg_map' || fail "VG on a kpartx mapping of the migrated disk should be selected"
if echo "$OUT" | grep -qx 'vg_other'; then fail "VG on another disk (the appliance's own) must NOT be selected"; fi
[ "$(echo "$OUT" | grep -cx 'vg_root')" = "1" ] || fail "VG names should be de-duplicated"

# 11) The universal-source hardenings must be present in the conversion:
#     LVM-root activation, cloud-agent disabling, and SELinux relabel.
grep -q 'vgchange -ay' "$HERE/machine-convert.sh" || fail "convert should activate LVM volume groups when no plain-partition root is found"
grep -q 'vgchange -an' "$HERE/machine-convert.sh" || fail "convert must deactivate the VGs it activated (cleanup)"
grep -q 'cloud-init.disabled' "$HERE/machine-convert.sh" || fail "convert should disable cloud-init on the migrated image"
grep -q 'google-guest-agent' "$HERE/machine-convert.sh" || fail "convert should disable the source cloud's agents"
grep -q '/.autorelabel' "$HERE/machine-convert.sh" || fail "convert should schedule an SELinux relabel for enforcing sources"

# 12) The script must still be syntactically valid.
bash -n "$HERE/machine-convert.sh" || fail "machine-convert.sh has a syntax error"

echo "ok  machine-convert.sh helpers (ensure_dir_mount, ensure_stage_dir)"

# 11) shrink_decision: the disk-boot cutover shrinks the replicated filesystem to
#     fit the destination's local disk. The guard used to compare the target only
#     against the DEVICE size, never the FILESYSTEM size — so when the filesystem
#     was smaller than the target (the normal case: a 50688 MiB source disk
#     replicated onto a 51200 MiB volume, target 51184), it fell through to
#     `resize2fs DEV 51184M`, which GREW the filesystem by ~496 MiB. The step
#     named "shrink" inflated the image, streamed the extra bytes over the rescue
#     console, and consumed the entire remaining margin — for nothing, since the
#     first boot grows the root to fill the disk anyway.
#     fs, target, dev (all MiB) -> "shrink" | "skip"

# The real-world default: filesystem already smaller than the target.
[ "$(shrink_decision 50688 51184 51200)" = "skip" ] \
  || fail "a filesystem already smaller than the target must NOT be resized (it would grow)"

# Genuinely too big: must still shrink.
[ "$(shrink_decision 51200 51184 51200)" = "shrink" ] \
  || fail "a filesystem larger than the target must still be shrunk"

# Exactly at the target: nothing to do.
[ "$(shrink_decision 51184 51184 51200)" = "skip" ] \
  || fail "a filesystem exactly at the target needs no resize"

# Pre-existing guard: target not smaller than the device.
[ "$(shrink_decision 20480 51184 20480)" = "skip" ] \
  || fail "a target no smaller than the device must skip (pre-existing guard)"

# Nonsense target.
[ "$(shrink_decision 50688 0 51200)" = "skip" ] \
  || fail "a zero target must skip"

# Unreadable superblock: we cannot prove the filesystem already fits, so attempt
# the shrink rather than silently skipping and failing the copy later.
[ "$(shrink_decision 0 51184 51200)" = "shrink" ] \
  || fail "an unknown filesystem size must attempt the shrink, not skip"
