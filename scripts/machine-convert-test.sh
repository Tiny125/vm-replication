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

# 4) The script must still be syntactically valid.
bash -n "$HERE/machine-convert.sh" || fail "machine-convert.sh has a syntax error"

echo "ok  machine-convert.sh helpers (ensure_dir_mount)"
