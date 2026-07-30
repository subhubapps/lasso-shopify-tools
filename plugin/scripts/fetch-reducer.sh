#!/usr/bin/env sh
# fetch-reducer.sh - platform-detect, download, SHA256-verify, and cache the
# lasso-shopify-tools `reducer` binary for the Claude payout-upload skill.
#
# Prints the absolute path of the verified, executable reducer binary to STDOUT.
# All diagnostics go to STDERR. On ANY error - including a checksum mismatch -
# it prints nothing to STDOUT and exits non-zero. An unverified binary is NEVER
# executed or left in the cache.
#
# Version resolution (first wins): arg $1, $REDUCER_VERSION, then plugin.json.
# Repo override for testing: $LASSO_SHOPIFY_TOOLS_REPO
#   (default: subhubapps/lasso-shopify-tools).
#
# The download tag is "v<version>" and the asset name is
#   reducer_<goos>_<goarch>[.exe]
# which MUST match .goreleaser.yaml's archives.name_template exactly.
set -eu

die() { printf 'fetch-reducer: %s\n' "$*" >&2; exit 1; }

# --- locate plugin root + resolve the version ----------------------------
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PLUGIN_JSON="$SCRIPT_DIR/../.claude-plugin/plugin.json"

VERSION="${1:-${REDUCER_VERSION:-}}"
if [ -z "$VERSION" ]; then
  [ -f "$PLUGIN_JSON" ] || die "cannot find plugin.json at $PLUGIN_JSON; pass a version as arg 1 or set REDUCER_VERSION"
  # Extract the first `"version": "X"` string value without needing jq.
  VERSION=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$PLUGIN_JSON" | head -n1)
fi
[ -n "$VERSION" ] || die "could not resolve the plugin version"

REPO="${LASSO_SHOPIFY_TOOLS_REPO:-subhubapps/lasso-shopify-tools}"
TAG="v$VERSION"

# --- detect platform (matches goreleaser asset names) --------------------
os_raw=$(uname -s 2>/dev/null || echo unknown)
arch_raw=$(uname -m 2>/dev/null || echo unknown)

case "$os_raw" in
  Darwin) GOOS=darwin ;;
  Linux) GOOS=linux ;;
  MINGW*|MSYS*|CYGWIN*|Windows_NT) GOOS=windows ;;
  *) die "unsupported OS '$os_raw' (need Darwin, Linux, or Windows)" ;;
esac

case "$arch_raw" in
  x86_64|amd64) GOARCH=amd64 ;;
  arm64|aarch64) GOARCH=arm64 ;;
  *) die "unsupported CPU arch '$arch_raw' (need x86_64/amd64 or arm64/aarch64)" ;;
esac

if [ "$GOOS" = windows ]; then EXT=".exe"; else EXT=""; fi

# Asset name scheme - keep in lockstep with .goreleaser.yaml
# archives.name_template ("reducer_{{ .Os }}_{{ .Arch }}"; goreleaser appends
# .exe for windows in the `binary` format):
ASSET="reducer_${GOOS}_${GOARCH}${EXT}"

CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/lasso-shopify-tools/$VERSION"
CACHED_BIN="$CACHE_DIR/reducer${EXT}"
CACHED_SUM="$CACHE_DIR/reducer.sha256"

# --- pick a sha256 tool --------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
  sha256_hex() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_hex() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "no sha256 tool found (need 'sha256sum' or 'shasum')"
fi

# --- cache hit: reuse offline (re-verify locally when a sum is stored) ----
if [ -x "$CACHED_BIN" ]; then
  if [ -f "$CACHED_SUM" ]; then
    want=$(cut -d' ' -f1 "$CACHED_SUM" 2>/dev/null || echo "")
    got=$(sha256_hex "$CACHED_BIN")
    if [ -n "$want" ] && [ "$want" = "$got" ]; then
      printf 'reducer %s cached and verified: %s\n' "$VERSION" "$CACHED_BIN" >&2
      printf '%s\n' "$CACHED_BIN"
      exit 0
    fi
    printf 'cached reducer failed local re-verification; refetching\n' >&2
    rm -f "$CACHED_BIN" "$CACHED_SUM"
  else
    printf 'reducer %s cached: %s\n' "$VERSION" "$CACHED_BIN" >&2
    printf '%s\n' "$CACHED_BIN"
    exit 0
  fi
fi

# --- pick a downloader ---------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q "$1" -O "$2"; }
else
  die "no downloader found (need 'curl' or 'wget')"
fi

BASE_URL="https://github.com/$REPO/releases/download/$TAG"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/lasso-reducer.XXXXXX") || die "cannot create temp dir"
trap 'rm -rf "$TMP"' EXIT INT TERM

printf 'downloading reducer %s (%s) from %s release %s ...\n' "$VERSION" "$ASSET" "$REPO" "$TAG" >&2
fetch "$BASE_URL/$ASSET" "$TMP/$ASSET" || die "download failed: $BASE_URL/$ASSET (is release $TAG published for $REPO?)"
fetch "$BASE_URL/SHA256SUMS" "$TMP/SHA256SUMS" || die "download failed: $BASE_URL/SHA256SUMS"

# --- verify BEFORE first exec --------------------------------------------
EXPECTED=$(awk -v f="$ASSET" '$2==f {print $1; exit}' "$TMP/SHA256SUMS")
[ -n "$EXPECTED" ] || die "no checksum for $ASSET in SHA256SUMS (asset-name mismatch?)"
ACTUAL=$(sha256_hex "$TMP/$ASSET")
if [ "$EXPECTED" != "$ACTUAL" ]; then
  rm -f "$TMP/$ASSET"
  die "SHA256 mismatch for $ASSET: expected $EXPECTED, got $ACTUAL - refusing to execute an unverified binary"
fi

# --- install into cache (+x) ---------------------------------------------
mkdir -p "$CACHE_DIR" || die "cannot create cache dir $CACHE_DIR"
mv -f "$TMP/$ASSET" "$CACHED_BIN" 2>/dev/null || cp -f "$TMP/$ASSET" "$CACHED_BIN" || die "cannot install binary to $CACHED_BIN"
chmod +x "$CACHED_BIN" 2>/dev/null || true
printf '%s  reducer%s\n' "$EXPECTED" "$EXT" > "$CACHED_SUM" || true

printf 'reducer %s verified and cached: %s\n' "$VERSION" "$CACHED_BIN" >&2
printf '%s\n' "$CACHED_BIN"
