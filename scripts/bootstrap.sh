#!/usr/bin/env bash
# bootstrap.sh — one-command installer for vm-replication.
#
#   curl -fsSL https://raw.githubusercontent.com/Tiny125/vm-replication/main/scripts/bootstrap.sh | sudo bash
#
# Downloads the prebuilt release tarball for this machine's CPU architecture,
# VERIFIES its SHA-256 checksum, extracts it under /usr/local/src, and execs
# scripts/install-replication-server.sh from the extracted tree — passing
# through every argument this script received, so --public-host, --region and
# --port all work through the one-liner too.
#
# Env:
#   VMREPL_REF=v1.4.0   pin to a specific release tag instead of "latest".
#                       Recommended for production so an install doesn't
#                       silently track a moving target.
#
# Falls back to building from source (fetching the source tarball from
# codeload and requiring a Go toolchain + gcc, which
# install-replication-server.sh installs if missing) only when there is no
# prebuilt release asset for this arch/version — including when the repo has
# no releases at all yet.
#
# Idempotent: re-running this upgrades in place. The underlying installer
# already preserves an existing region/port on upgrade, so re-running never
# moves a console that is already running.
#
# Needs only curl and tar to get started; they are installed via the system
# package manager if missing.
set -euo pipefail

REPO="Tiny125/vm-replication"
GITHUB="https://github.com/${REPO}"
GITHUB_API_LATEST="https://api.github.com/repos/${REPO}/releases/latest"
CODELOAD_TARBALL="https://codeload.github.com/${REPO}/tar.gz"
SRC_ROOT="/usr/local/src"

# ---------------------------------------------------------------------------
# Pure helpers — no root, no network side effects beyond the curl calls named
# in each function, nothing that touches the filesystem outside /tmp-style
# scratch dirs passed in by the caller. Testable in isolation by sourcing this
# file with VMREPL_BOOTSTRAP_LIB=1 (see bootstrap-test.sh), which stubs curl.
# ---------------------------------------------------------------------------

# detect_arch [UNAME_M] — maps a `uname -m` string to the arch name used in
# release asset names, and sets ARCH. Defaults to the real `uname -m` when no
# argument is given, so tests can pass an arbitrary string without needing to
# run on that hardware. This mirrors install-replication-server.sh's
# install_go, which supports exactly the same two arches.
detect_arch() {
  local m="${1:-$(uname -m)}"
  case "$m" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *)
      echo "unsupported CPU architecture: $m (supported: x86_64/amd64, aarch64/arm64)" >&2
      return 1
      ;;
  esac
  return 0
}

# asset_name TAG ARCH — the exact tarball filename release.yml produces. This
# is the join between the release workflow and this script: if they ever
# disagree, every install breaks, so it is asserted literally in the tests.
asset_name() {
  printf 'vm-replication_%s_linux_%s.tar.gz' "$1" "$2"
}

# asset_url / sums_url TAG ARCH — where a release's assets live.
asset_url() {
  printf '%s/releases/download/%s/%s' "$GITHUB" "$1" "$(asset_name "$1" "$2")"
}
sums_url() {
  printf '%s/releases/download/%s/SHA256SUMS' "$GITHUB" "$1"
}

# parse_latest_tag JSON — pull "tag_name" out of a GitHub releases/latest
# response with grep/sed rather than jq: bootstrap has to work before any
# packages are installed, and jq is not guaranteed to be present yet. Prints
# nothing if the field is absent (e.g. the repo has no releases: GitHub
# returns {"message":"Not Found",...}).
parse_latest_tag() {
  # The trailing "|| true" matters under `set -e -o pipefail`: grep exits 1
  # when tag_name is absent (no releases yet), and that would otherwise abort
  # the whole script the moment this is used in a bare `VAR=$(...)` assignment.
  printf '%s' "$1" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/' || true
}

# resolve_version — VMREPL_REF pins the version (an operator installing into
# production should be able to avoid tracking a moving target); otherwise ask
# the GitHub API for the latest release tag. Sets VERSION (empty string if
# there are no releases yet) and VERSION_SRC. Never fails outright: an empty
# VERSION with no releases is a normal state the caller falls back to source
# for, not an error.
resolve_version() {
  if [ -n "${VMREPL_REF:-}" ]; then
    VERSION="$VMREPL_REF"; VERSION_SRC="VMREPL_REF"; return 0
  fi
  local json
  json="$(curl -fsSL --max-time 10 "$GITHUB_API_LATEST" 2>/dev/null || true)"
  VERSION="$(parse_latest_tag "$json")"
  VERSION_SRC="the GitHub API (latest release)"
  return 0
}

# check_asset_exists URL — true if a HEAD request finds it. Split out from
# needs_fallback so tests can stub curl by URL pattern without a real network.
check_asset_exists() {
  curl -fsSL --max-time 10 -o /dev/null --head "$1" 2>/dev/null
}

# needs_fallback TAG ARCH — true (shell 0) when there is no prebuilt asset to
# install: either there are no releases at all yet (TAG empty) or this
# arch/version combination was never published as a release asset.
needs_fallback() {
  local tag="$1" arch="$2"
  [ -n "$tag" ] || return 0
  check_asset_exists "$(asset_url "$tag" "$arch")" && return 1
  return 0
}

# verify_checksum FILE SUMSFILE — the whole security argument for the
# one-liner: refuse to extract anything whose SHA-256 doesn't match what the
# release published. Matches SUMSFILE by basename (with or without a leading
# "*" for sha256sum's binary-mode marker) so one SHA256SUMS can list both
# arches' tarballs.
verify_checksum() {
  local file="$1" sumsfile="$2" base expected actual
  base="$(basename "$file")"
  if [ ! -f "$sumsfile" ]; then
    echo "checksum file not found: $sumsfile" >&2
    return 1
  fi
  expected="$(awk -v f="$base" '($2==f || $2=="*"f){print $1; exit}' "$sumsfile")"
  if [ -z "$expected" ]; then
    echo "no checksum entry for $base in $sumsfile" >&2
    return 1
  fi
  actual="$(sha256sum "$file" | awk '{print $1}')"
  if [ "$expected" != "$actual" ]; then
    echo "CHECKSUM MISMATCH for $base: expected $expected, got $actual" >&2
    return 1
  fi
  return 0
}

# detect_pkg_mgr / install_prereqs — mirrors install-replication-server.sh's
# own detect_pkg_mgr/install_packages style: try known package managers in
# order, install only what's missing. Bootstrap needs far less than the full
# installer (no compiler, no Go) since it just fetches and extracts a tarball.
detect_pkg_mgr() {
  for m in apt-get dnf yum zypper; do command -v "$m" >/dev/null 2>&1 && { echo "$m"; return; }; done
  echo ""
}

install_prereqs() {
  command -v curl >/dev/null 2>&1 && command -v tar >/dev/null 2>&1 && return 0
  local mgr; mgr="$(detect_pkg_mgr)"
  echo ">> Installing curl/tar via ${mgr:-<none>}"
  case "$mgr" in
    apt-get)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -y || apt-get update -y
      apt-get install -y --no-install-recommends curl tar ca-certificates
      ;;
    dnf)    dnf install -y curl tar ca-certificates ;;
    yum)    yum install -y curl tar ca-certificates ;;
    zypper) zypper --non-interactive install curl tar ca-certificates ;;
    *) echo "WARNING: no supported package manager found; please install curl and tar manually" ;;
  esac
}

# Test hook: sourcing with VMREPL_BOOTSTRAP_LIB=1 loads the helpers above and
# returns before the OS/root checks and any real work, so the resolution and
# verification logic can be unit-tested without root, a real download, or a
# live network — see bootstrap-test.sh.
if [ -n "${VMREPL_BOOTSTRAP_LIB:-}" ]; then return 0 2>/dev/null || exit 0; fi

# ---------------------------------------------------------------------------
# Real work starts here.
# ---------------------------------------------------------------------------

[ "$(uname -s)" = "Linux" ] || {
  echo "vm-replication only installs on Linux; detected: $(uname -s)"; exit 1;
}
[ "$(id -u)" -eq 0 ] || {
  echo "run as root, e.g.: curl -fsSL .../bootstrap.sh | sudo bash"; exit 1;
}

install_prereqs
command -v curl >/dev/null 2>&1 || { echo "curl is required and could not be installed automatically"; exit 1; }
command -v tar  >/dev/null 2>&1 || { echo "tar is required and could not be installed automatically"; exit 1; }

detect_arch

echo ">> Resolving version..."
resolve_version
if [ -n "$VERSION" ]; then
  echo ">> Version: $VERSION (from $VERSION_SRC)"
else
  echo ">> No published releases found for $REPO."
fi

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

if [ -n "$VERSION" ] && ! needs_fallback "$VERSION" "$ARCH"; then
  # -------------------------- prebuilt path --------------------------------
  TARBALL_NAME="$(asset_name "$VERSION" "$ARCH")"
  TARBALL_URL="$(asset_url "$VERSION" "$ARCH")"
  SUMS_URL="$(sums_url "$VERSION")"

  echo ">> Downloading $TARBALL_NAME"
  curl -fsSL --max-time 120 "$TARBALL_URL" -o "$WORKDIR/$TARBALL_NAME"
  echo ">> Downloading SHA256SUMS"
  curl -fsSL --max-time 30 "$SUMS_URL" -o "$WORKDIR/SHA256SUMS"

  echo ">> Verifying checksum"
  if ! verify_checksum "$WORKDIR/$TARBALL_NAME" "$WORKDIR/SHA256SUMS"; then
    echo "!! Checksum verification FAILED — deleting the download and aborting." >&2
    rm -f "$WORKDIR/$TARBALL_NAME" "$WORKDIR/SHA256SUMS"
    exit 1
  fi
  echo ">> Checksum OK"

  DEST="$SRC_ROOT/vm-replication-$VERSION"
  install -d "$DEST"
  echo ">> Extracting to $DEST"
  tar -xzf "$WORKDIR/$TARBALL_NAME" -C "$DEST"
else
  # -------------------------- source fallback -------------------------------
  if [ -z "$VERSION" ]; then
    echo ">> No release asset available: this repository has no published releases yet."
  else
    echo ">> No release asset available for $ARCH at $VERSION."
  fi
  echo ">> Falling back to building from source. This path needs a Go toolchain"
  echo ">> and a C compiler; install-replication-server.sh will install them if missing."

  REF="${VERSION:-main}"
  echo ">> Fetching source ($REF) from codeload"
  curl -fsSL --max-time 120 "$CODELOAD_TARBALL/$REF" -o "$WORKDIR/source.tar.gz"

  DEST="$SRC_ROOT/vm-replication-$REF"
  install -d "$DEST"
  # A codeload tarball wraps everything in one top-level directory
  # (e.g. vm-replication-main/); strip it so DEST matches the prebuilt layout.
  tar -xzf "$WORKDIR/source.tar.gz" -C "$DEST" --strip-components=1
fi

# Stable symlink so an operator can see what is installed and re-run the
# installer by hand later, without hunting for the versioned directory.
ln -sfn "$DEST" "$SRC_ROOT/vm-replication"
echo ">> Installed tree: $SRC_ROOT/vm-replication -> $DEST"

echo ">> Running scripts/install-replication-server.sh"
exec bash "$SRC_ROOT/vm-replication/scripts/install-replication-server.sh" "$@"
