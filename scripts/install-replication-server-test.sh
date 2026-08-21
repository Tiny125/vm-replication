#!/usr/bin/env bash
# install-replication-server-test.sh — unit tests for the installer's settings
# resolution: where REGION and PORT come from, and (the bug this exists for)
# that re-running the installer to upgrade never discards an operator's choice.
#
# It loads install-replication-server.sh in "library mode"
# (VMREPL_INSTALL_LIB=1), which defines the helper functions and returns before
# the root check and any real work, then drives them against a scratch dir with
# a stubbed metadata service.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A fake curl on PATH stands in for the Linode Metadata service, so these tests
# never touch the network and behave the same on a laptop as on a Linode.
# METADATA_REGION="" makes it unreachable.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Stub metadata service. Emits a token, then an instance document.
case "$*" in
  *169.254.169.254/v1/token*)    [ -n "${METADATA_REGION:-}" ] && echo "stub-token"; exit 0 ;;
  *169.254.169.254/v1/instance*) [ -n "${METADATA_REGION:-}" ] && printf '{"id":1,"label":"x","region":"%s"}\n' "$METADATA_REGION"; exit 0 ;;
esac
exit 1
STUB
chmod +x "$WORK/bin/curl"
PATH="$WORK/bin:$PATH"

# shellcheck source=/dev/null
VMREPL_INSTALL_LIB=1 source "$HERE/install-replication-server.sh"

# The library must not have run any of the install; ETC is only a path here.
ETC="$WORK/etc"; mkdir -p "$ETC"
ENVFILE="$ETC/applianced.env"
UNIT="$WORK/applianced.service"

reset() { rm -f "$ENVFILE" "$UNIT"; REGION_FLAG=""; PORT_FLAG=""; }
write_env() { printf 'REGION=%s\nPORT=%s\n' "$1" "$2" > "$ENVFILE"; }
write_unit() { printf 'ExecStart=/usr/local/bin/applianced \\\n  -listen :%s \\\n  -region %s \\\n' "$2" "$1" > "$UNIT"; }

# --- 1) An explicit flag beats everything else. -----------------------------
reset; write_env "ap-south" "9000"; export METADATA_REGION="us-east"
REGION_FLAG="eu-west"; resolve_region
[ "$REGION" = "eu-west" ] || fail "an explicit --region must win, got $REGION"

# --- 2) A stored value beats detection. This is the bug: an upgrade must not
#        silently replace the operator's region with a detected or default one.
reset; write_env "ap-south" "9000"; export METADATA_REGION="us-east"
resolve_region
[ "$REGION" = "ap-south" ] || fail "a stored region must survive an upgrade, got $REGION"

# --- 3) Detection is used on a fresh install. --------------------------------
reset; export METADATA_REGION="ap-northeast"
resolve_region
[ "$REGION" = "ap-northeast" ] || fail "a fresh install should detect its region, got $REGION"

# --- 4) The built-in default only when nothing else is available. ------------
reset; export METADATA_REGION=""
resolve_region
[ "$REGION" = "us-ord" ] || fail "with no metadata and no stored value, expected the default, got $REGION"

# --- 5) One-time migration: an existing unit carrying the OLD HARDCODED
#        DEFAULT is not an operator choice, so a detected region wins over it.
reset; write_unit "us-ord" "8080"; export METADATA_REGION="sg-sin-2"
resolve_region "$UNIT"
[ "$REGION" = "sg-sin-2" ] || fail "the stale default in an old unit must not outrank detection, got $REGION"

# --- 6) But a unit carrying a value the operator actually chose is preserved,
#        even when detection disagrees.
reset; write_unit "ap-south" "8080"; export METADATA_REGION="us-east"
resolve_region "$UNIT"
[ "$REGION" = "ap-south" ] || fail "an operator's hand-edited region must be preserved, got $REGION"

# --- 7) THE REGRESSION THIS FILE EXISTS FOR: install, correct the region,
#        re-run the installer with no flags, and the correction must survive.
reset; export METADATA_REGION=""
resolve_region                       # fresh install -> default
[ "$REGION" = "us-ord" ] || fail "setup: expected the default on a fresh install"
write_env "sg-sin-2" "8080"          # operator corrects it (persisted)
REGION_FLAG=""; resolve_region       # upgrade re-run, no flags
[ "$REGION" = "sg-sin-2" ] || fail "re-running the installer wiped the operator's region (the F-01 bug), got $REGION"

# --- 8) PORT follows the same rules; an operator's port must also survive. ---
reset; write_env "us-ord" "9443"
resolve_port
[ "$PORT" = "9443" ] || fail "a stored port must survive an upgrade, got $PORT"

reset
resolve_port
[ "$PORT" = "8080" ] || fail "expected the default port with nothing stored, got $PORT"

reset; PORT_FLAG="9999"; write_env "us-ord" "9443"
resolve_port
[ "$PORT" = "9999" ] || fail "an explicit --port must win, got $PORT"

reset; write_unit "us-ord" "9443"
resolve_port "$UNIT"
[ "$PORT" = "9443" ] || fail "a port already in the unit must be preserved, got $PORT"

# --- 9) The env file is only seeded once and never clobbered afterwards. -----
reset; write_env "ap-south" "9443"
REGION="us-ord"; PORT="8080"
seed_env_file
grep -q '^REGION=ap-south' "$ENVFILE" || fail "seed_env_file overwrote an existing region"
grep -q '^PORT=9443' "$ENVFILE" || fail "seed_env_file overwrote an existing port"

reset
REGION="sg-sin-2"; PORT="8080"
seed_env_file
grep -q '^REGION=sg-sin-2' "$ENVFILE" || fail "seed_env_file did not create the file"

# --- 10) Detection failures must never abort the install (set -e safety). ----
reset; export METADATA_REGION=""
got="$(detect_region || echo "RETURNED_NONZERO")"
[ "$got" != "RETURNED_NONZERO" ] || fail "detect_region must not fail the script when metadata is unreachable"
[ -z "$got" ] || fail "detect_region should print nothing when metadata is unreachable, got '$got'"

echo "ok  install-replication-server.sh settings resolution (region, port, upgrade preservation)"
