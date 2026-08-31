#!/usr/bin/env bash
# bootstrap-test.sh — unit tests for bootstrap.sh's pure helpers: arch
# detection, version resolution, asset naming (the join with release.yml),
# checksum verification, and the fallback trigger.
#
# It loads bootstrap.sh in "library mode" (VMREPL_BOOTSTRAP_LIB=1), which
# defines the helper functions and returns before the root/OS checks and any
# real work, then drives them against a scratch dir with a stubbed GitHub API
# and stubbed asset-existence checks. No network, no root, no real download.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A fake curl on PATH stands in for the GitHub API (releases/latest) and for
# the HEAD request used to check whether a release asset exists, so these
# tests never touch the network and behave the same on a laptop as in CI.
#   STUB_LATEST_JSON     — body returned for a GET to .../releases/latest
#   STUB_ASSET_EXISTS=1  — a --head request succeeds (asset found)
mkdir -p "$WORK/bin"
cat > "$WORK/bin/curl" <<'STUB'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *--head*)
    [ "${STUB_ASSET_EXISTS:-0}" = "1" ] && exit 0
    exit 22
    ;;
  *releases/latest*)
    printf '%s' "${STUB_LATEST_JSON:-{}}"
    exit 0
    ;;
esac
exit 1
STUB
chmod +x "$WORK/bin/curl"
PATH="$WORK/bin:$PATH"

# shellcheck source=/dev/null
VMREPL_BOOTSTRAP_LIB=1 source "$HERE/bootstrap.sh"

# --- 1) arch detection: x86_64/amd64 -> amd64, aarch64/arm64 -> arm64 --------
ARCH=""
detect_arch x86_64
[ "$ARCH" = amd64 ] || fail "x86_64 should map to amd64, got $ARCH"

ARCH=""
detect_arch amd64
[ "$ARCH" = amd64 ] || fail "amd64 should map to amd64, got $ARCH"

ARCH=""
detect_arch aarch64
[ "$ARCH" = arm64 ] || fail "aarch64 should map to arm64, got $ARCH"

ARCH=""
detect_arch arm64
[ "$ARCH" = arm64 ] || fail "arm64 should map to arm64, got $ARCH"

# --- 2) an unsupported arch is rejected, with a clear message ----------------
ARCH=""
if detect_arch mips 2>/dev/null; then fail "an unsupported arch must be rejected, got ARCH=$ARCH"; fi
err="$(detect_arch riscv64 2>&1 >/dev/null || true)"
case "$err" in
  *"unsupported CPU architecture"*"riscv64"*) ;;
  *) fail "expected a clear error naming the arch and the supported set, got: $err" ;;
esac

# --- 3) version resolution: VMREPL_REF wins; otherwise the latest tag is -----
#        parsed from a stubbed API response.
unset VMREPL_REF || true
export STUB_LATEST_JSON='{"url":"x","tag_name":"v1.4.0","name":"v1.4.0","assets":[{"name":"a"}]}'
resolve_version
[ "$VERSION" = "v1.4.0" ] || fail "expected the latest tag from the API, got $VERSION"
[ "$VERSION_SRC" = "the GitHub API (latest release)" ] || fail "unexpected VERSION_SRC: $VERSION_SRC"

export VMREPL_REF="v2.0.0-pinned"
resolve_version
[ "$VERSION" = "v2.0.0-pinned" ] || fail "VMREPL_REF must win over the API, got $VERSION"
[ "$VERSION_SRC" = "VMREPL_REF" ] || fail "unexpected VERSION_SRC: $VERSION_SRC"
unset VMREPL_REF

# No releases published yet -> VERSION is empty, not an error (the caller
# falls back to source for this).
export STUB_LATEST_JSON='{"message":"Not Found"}'
resolve_version
[ -z "$VERSION" ] || fail "with no releases, VERSION should be empty, got $VERSION"

got="$(parse_latest_tag '{"url":"x","tag_name":"v1.4.0","assets":[{"name":"a"}]}')"
[ "$got" = "v1.4.0" ] || fail "parse_latest_tag misparsed a realistic payload, got $got"

# --- 4) asset naming matches exactly what release.yml produces --------------
[ "$(asset_name v1.4.0 amd64)" = "vm-replication_v1.4.0_linux_amd64.tar.gz" ] \
  || fail "asset_name (amd64) mismatch: $(asset_name v1.4.0 amd64)"
[ "$(asset_name v1.4.0 arm64)" = "vm-replication_v1.4.0_linux_arm64.tar.gz" ] \
  || fail "asset_name (arm64) mismatch: $(asset_name v1.4.0 arm64)"

# --- 5) checksum verification: the most important test in this file ---------
TARBALL="$WORK/vm-replication_v1.4.0_linux_amd64.tar.gz"
echo "hello vm-replication" > "$TARBALL"
real_hash="$(sha256sum "$TARBALL" | awk '{print $1}')"

printf '%s  vm-replication_v1.4.0_linux_amd64.tar.gz\n' "$real_hash" > "$WORK/SHA256SUMS"
verify_checksum "$TARBALL" "$WORK/SHA256SUMS" \
  || fail "verify_checksum must PASS on a matching hash"

bad_hash="0000000000000000000000000000000000000000000000000000000000000000"
printf '%s  vm-replication_v1.4.0_linux_amd64.tar.gz\n' "$bad_hash" > "$WORK/SHA256SUMS.bad"
if verify_checksum "$TARBALL" "$WORK/SHA256SUMS.bad" 2>/dev/null; then
  fail "verify_checksum must FAIL on a mismatched hash"
fi

# No entry for this file at all (e.g. wrong tarball name).
printf '%s  some-other-file.tar.gz\n' "$real_hash" > "$WORK/SHA256SUMS.missing"
if verify_checksum "$TARBALL" "$WORK/SHA256SUMS.missing" 2>/dev/null; then
  fail "verify_checksum must FAIL when there is no entry for the file"
fi

# --- 6) fallback trigger: only when no asset matches -------------------------
# No tag at all (no releases published anywhere) -> fallback.
if ! needs_fallback "" amd64; then fail "an empty tag (no releases) must trigger fallback"; fi

# A tag exists and the asset for this arch is present -> no fallback.
export STUB_ASSET_EXISTS=1
if needs_fallback v1.4.0 amd64; then fail "an existing asset must NOT trigger fallback"; fi

# A tag exists but there is no asset for this arch/version -> fallback.
export STUB_ASSET_EXISTS=0
if ! needs_fallback v1.4.0 arm64; then fail "a missing asset must trigger fallback"; fi
unset STUB_ASSET_EXISTS

echo "ok  bootstrap.sh (arch detection, version resolution, asset naming, checksum verification, fallback trigger)"
