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

# 7) The script must still be syntactically valid.
bash -n "$HERE/machine-convert.sh" || fail "machine-convert.sh has a syntax error"

echo "ok  machine-convert.sh helpers (ensure_dir_mount, ensure_stage_dir)"
