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

# 12) MULTI-DISK RESOLUTION. Local-disk boot now carries data disks alongside the
#     boot disk, so "did this device come along with the migration?" stops being
#     a question about ONE disk. dev_on_migrated_disk must answer against the
#     whole migrated SET, while still defaulting to $DEV alone when no set is
#     given (which is what keeps the single-disk swap tests above honest).
DEV=/dev/sdc
unset MIGRATED_DEVS
dev_on_migrated_disk /dev/sdc  || fail "the migrated disk itself must count"
dev_on_migrated_disk /dev/sdc2 || fail "a partition of the migrated disk must count"
dev_on_migrated_disk /dev/sdd  && fail "an unrelated disk must NOT count when no set is configured"

# With a set, every member and their partitions count -- and nothing else does.
MIGRATED_DEVS="/dev/sdc /dev/sdd"
dev_on_migrated_disk /dev/sdc   || fail "first member of the migrated set must count"
dev_on_migrated_disk /dev/sdd   || fail "second member of the migrated set must count"
dev_on_migrated_disk /dev/sdd3  || fail "a partition of a data disk in the set must count"
dev_on_migrated_disk /dev/sdc1  || fail "a partition of the boot disk must count"
dev_on_migrated_disk /dev/sde   && fail "a disk outside the set must NOT count"
dev_on_migrated_disk /dev/sde1  && fail "a partition of a disk outside the set must NOT count"
dev_on_migrated_disk /dev/mapper/sdd2 || fail "a kpartx mapping of a set member must count"
unset MIGRATED_DEVS

# 13) fstab_spec_device: resolve each spec form to a device. This is the
#     extraction of the logic swap_spec_exists already had, generalised so it
#     can serve ANY fstab entry rather than only swap.
DEV=/dev/sdc
MIGRATED_DEVS="/dev/sdc /dev/sdd"
blkid() {
  case "$*" in
    "-U 11111111-1111-1111-1111-111111111111") echo /dev/sdd1; return 0 ;;  # on the migrated set
    "-U 99999999-9999-9999-9999-999999999999") echo /dev/sde1; return 0 ;;  # HOST disk, not migrated
    "-L datalabel") echo /dev/sdd2; return 0 ;;
    "-t PARTUUID=aaaa-bbbb -o device") echo /dev/sdd3; return 0 ;;
    *) return 2 ;;
  esac
}
[ "$(fstab_spec_device 'UUID=11111111-1111-1111-1111-111111111111')" = /dev/sdd1 ] \
  || fail "fstab_spec_device must resolve UUID="
[ "$(fstab_spec_device 'LABEL=datalabel')" = /dev/sdd2 ] \
  || fail "fstab_spec_device must resolve LABEL="
[ "$(fstab_spec_device 'PARTUUID=aaaa-bbbb')" = /dev/sdd3 ] \
  || fail "fstab_spec_device must resolve PARTUUID="
[ "$(fstab_spec_device '/dev/sdd4')" = /dev/sdd4 ] \
  || fail "fstab_spec_device must pass a bare device through"
[ -z "$(fstab_spec_device 'UUID=00000000-0000-0000-0000-000000000000')" ] \
  || fail "an unresolvable UUID must resolve to nothing"

# 14) fstab_entry_action: the three-way policy. An entry that resolves onto the
#     migrated set is left EXACTLY as it was; one that resolves nowhere (or only
#     on the converting host) gets nofail so the destination still boots; and a
#     non-block filesystem is never touched at all.
#     Signature: fstab_entry_action <spec> <fstype> -> keep|nofail|skip
[ "$(fstab_entry_action 'UUID=11111111-1111-1111-1111-111111111111' ext4)" = keep ] \
  || fail "an entry resolving onto the migrated set must be kept as-is"
[ "$(fstab_entry_action 'LABEL=datalabel' ext4)" = keep ] \
  || fail "a LABEL= entry on the migrated set must be kept"
[ "$(fstab_entry_action 'PARTUUID=aaaa-bbbb' xfs)" = keep ] \
  || fail "a PARTUUID= entry on the migrated set must be kept"
[ "$(fstab_entry_action 'UUID=00000000-0000-0000-0000-000000000000' ext4)" = nofail ] \
  || fail "an entry resolving nowhere must be marked nofail"

# The shared-UUID trap again, now for DATA entries: resolving on the converting
# host proves nothing. /dev/sde1 is the appliance's own disk, not migrated.
[ "$(fstab_entry_action 'UUID=99999999-9999-9999-9999-999999999999' ext4)" = nofail ] \
  || fail "an entry resolving only on the CONVERTING HOST must be marked nofail, not kept"

# Network and virtual filesystems are somebody else's problem -- never rewrite them.
for fs in nfs nfs4 cifs tmpfs proc sysfs devpts overlay; do
  [ "$(fstab_entry_action 'whatever' "$fs")" = skip ] \
    || fail "$fs entries must be left alone entirely"
done
# swap keeps its own dedicated handling (disable_stale_swap), not this one.
[ "$(fstab_entry_action 'UUID=00000000-0000-0000-0000-000000000000' swap)" = skip ] \
  || fail "swap must be left to disable_stale_swap, not double-handled here"

# 15) fix_data_fstab end to end. Verified entries survive byte-for-byte; a bare
#     /dev/sdX that resolves onto the migrated set is rewritten to the UUID we
#     read off it (device names do NOT survive a migration -- source /dev/sdc
#     becomes destination /dev/sdb -- but the filesystem UUID does); and an
#     unverifiable entry gains nofail with its original preserved as a comment.
FSTAB4="$WORK/fstab4"
cat > "$FSTAB4" <<'EOF'
# comment stays
UUID=16829997-c4bf-8fc8-89a5-e49ca9f84956 / ext4 errors=remount-ro 0 1
UUID=11111111-1111-1111-1111-111111111111 /srv/keep ext4 defaults 0 2
UUID=00000000-0000-0000-0000-000000000000 /srv/ghost ext4 defaults 0 2
nfsserver:/export /srv/net nfs defaults 0 0
EOF
fix_data_fstab "$FSTAB4" >/dev/null
grep -q '^# comment stays' "$FSTAB4" || fail "fix_data_fstab must keep comments"
grep -q '^UUID=11111111.* /srv/keep ext4 defaults 0 2$' "$FSTAB4" \
  || fail "a verified entry must be left byte-for-byte unchanged"
grep -q '^nfsserver:/export /srv/net nfs defaults 0 0$' "$FSTAB4" \
  || fail "an NFS entry must be left untouched"
grep -qE '^UUID=00000000[^#]*nofail' "$FSTAB4" \
  || fail "an unverifiable entry must gain nofail"
grep -q '^# vmrepl: ' "$FSTAB4" \
  || fail "the original line must be preserved as a commented vmrepl marker"

# The root entry must never be given nofail -- a root that silently does not
# mount is not a bootable machine, and there is no degraded mode worth having.
grep -q '^UUID=16829997.* / ext4 errors=remount-ro 0 1$' "$FSTAB4" \
  || fail "the root entry must never be rewritten"

# 15b) An fstab with nothing to fix is left byte-identical (no gratuitous churn).
FSTAB5="$WORK/fstab5"
cat > "$FSTAB5" <<'EOF'
UUID=16829997-c4bf-8fc8-89a5-e49ca9f84956 / ext4 errors=remount-ro 0 1
UUID=11111111-1111-1111-1111-111111111111 /srv/keep ext4 defaults 0 2
EOF
cp "$FSTAB5" "$FSTAB5.orig"
fix_data_fstab "$FSTAB5" >/dev/null
cmp -s "$FSTAB5" "$FSTAB5.orig" || fail "an fstab needing no changes must be untouched"

unset -f blkid
unset MIGRATED_DEVS
unset DEV


# 16) F-18: the migrated image must not carry the SOURCE's replication agent.
#     The agent is installed on the source, so a block-for-block copy brings it
#     along; on the destination it can never reach its job and fails, leaving
#     EVERY migrated instance permanently `degraded`. It also carries the mTLS
#     client key for the replication data plane, which has no business living on
#     a migrated production box.
IMG="$WORK/img-agent"
mkdir -p "$IMG/etc/systemd/system/timers.target.wants" \
         "$IMG/etc/systemd/system/multi-user.target.wants" \
         "$IMG/usr/local/bin" "$IMG/etc/vm-repl"
: > "$IMG/etc/systemd/system/vmrepl-agent.service"
: > "$IMG/etc/systemd/system/vmrepl-agent.timer"
ln -s /etc/systemd/system/vmrepl-agent.timer "$IMG/etc/systemd/system/timers.target.wants/vmrepl-agent.timer"
: > "$IMG/usr/local/bin/vmrepl-agent"
: > "$IMG/etc/vm-repl/agent.key"
: > "$IMG/etc/vm-repl/agent.crt"
: > "$IMG/etc/vm-repl/ca.crt"
# Things that must survive untouched.
ln -s /usr/lib/systemd/system/fstrim.timer "$IMG/etc/systemd/system/timers.target.wants/fstrim.timer"
: > "$IMG/usr/local/bin/keep-me"
mkdir -p "$IMG/etc/ssh"; : > "$IMG/etc/ssh/sshd_config"

strip_migrated_agent "$IMG" >/dev/null

[ ! -e "$IMG/etc/systemd/system/timers.target.wants/vmrepl-agent.timer" ] \
  || fail "the agent timer must be un-enabled, or it starts on the migrated machine"
[ ! -e "$IMG/etc/systemd/system/vmrepl-agent.service" ] \
  || fail "the agent service unit must be removed"
[ ! -e "$IMG/etc/systemd/system/vmrepl-agent.timer" ] \
  || fail "the agent timer unit must be removed"
[ ! -e "$IMG/usr/local/bin/vmrepl-agent" ] \
  || fail "the agent binary must be removed"
[ ! -e "$IMG/etc/vm-repl/agent.key" ] \
  || fail "the agent's mTLS PRIVATE KEY must not ship on the migrated machine"
[ ! -e "$IMG/etc/vm-repl" ] \
  || fail "the agent's TLS directory must be removed entirely"

# Collateral damage check: unrelated timers, binaries and config must survive.
[ -L "$IMG/etc/systemd/system/timers.target.wants/fstrim.timer" ] \
  || fail "strip_migrated_agent deleted an unrelated systemd timer"
[ -f "$IMG/usr/local/bin/keep-me" ] \
  || fail "strip_migrated_agent deleted an unrelated binary"
[ -f "$IMG/etc/ssh/sshd_config" ] \
  || fail "strip_migrated_agent touched unrelated configuration"

# 16b) Idempotent, and safe on an image that never had the agent.
IMG2="$WORK/img-noagent"
mkdir -p "$IMG2/etc/systemd/system"
: > "$IMG2/etc/systemd/system/other.service"
strip_migrated_agent "$IMG2" >/dev/null
[ -f "$IMG2/etc/systemd/system/other.service" ] || fail "strip_migrated_agent damaged an agent-free image"
strip_migrated_agent "$IMG" >/dev/null || fail "strip_migrated_agent must be idempotent"

# 16c) An empty root must be refused outright — a bare `rm -rf /etc/vm-repl`
#      would be running against the APPLIANCE's own filesystem, deleting the
#      live receiver's TLS material mid-migration.
strip_migrated_agent "" 2>/dev/null && fail "strip_migrated_agent must refuse an empty root"
strip_migrated_agent "/" 2>/dev/null && fail "strip_migrated_agent must refuse / (that is the appliance itself)"
echo "machine-convert-test: all tests passed"
