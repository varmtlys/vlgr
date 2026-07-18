#!/usr/bin/env bash
#
# Build script for Linux (Bash)
# Cross-compiles VLGR server and client for all supported platforms.
#
# Usage:
#   ./scripts/build.sh                                  # Build all platforms
#   ./scripts/build.sh -t "linux/amd64"                 # Single platform
#   ./scripts/build.sh -t "linux/amd64,linux/arm64"     # Specific platforms
#
# Available targets: windows/amd64, windows/x86, linux/amd64, linux/x86, linux/arm64, darwin/amd64, darwin/arm64
#
# Signed self-update (optional): pass an ed25519 private seed (hex) via -s or the
# VLGR_SIGNING_KEY env var. The public key is then compiled into every binary and
# a <binary>.sig is written alongside it. Generate a key once with:
#   go run ./scripts/sign genkey
# Without a key the binaries build unsigned and cannot self-update (fail-safe).
#
# Requires: Go 1.26+ installed and in PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BUILD_DIR="${PROJECT_ROOT}/build"

# Derive version from git (tag, or commit hash if no tags).
GIT_VERSION="$(git -C "$PROJECT_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
GIT_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-s -w -X vlgr/internal/version.Version=${GIT_VERSION} -X vlgr/internal/version.GitCommit=${GIT_COMMIT} -X vlgr/internal/version.BuildDate=${BUILD_DATE}"

TARGETS_FILTER=""
SIGN_KEY="${VLGR_SIGNING_KEY:-}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -t) TARGETS_FILTER="$2"; shift 2 ;;
        -s) SIGN_KEY="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# With a signing key, derive its public key on the host and compile it into
# every binary, so each build verifies its own updates.
SIGN_PUBKEY=""
if [ -n "$SIGN_KEY" ]; then
    SIGN_PUBKEY="$(go run ./scripts/sign pubkey "$SIGN_KEY")"
    [ -n "$SIGN_PUBKEY" ] || { echo "Failed to derive signing public key" >&2; exit 1; }
    LDFLAGS="$LDFLAGS -X vlgr/internal/selfupdate.SigningPublicKey=${SIGN_PUBKEY}"
fi

# Absolute paths of every artifact produced, for the signing pass.
BUILT=()

# All supported targets: OS GoArch Label Ext
ALL_TARGETS=(
    "windows amd64 amd64 .exe"
    "windows 386   x86   .exe"
    "linux   amd64 amd64"
    "linux   386   x86"
    "linux   arm64 arm64"
    "darwin  amd64 amd64"
    "darwin  arm64 arm64"
)

APPS=("vlgr-server:./cmd/server" "vlgr-client:./cmd/client")

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
GRAY='\033[0;37m'
NC='\033[0m'

build_app() {
    local os="$1"
    local goarch="$2"
    local label="$3"
    local ext="$4"
    local name="$5"
    local src="$6"

    local suffix
    if [ -n "$ext" ]; then
        suffix="${os}-${label}${ext}"
    else
        suffix="${os}-${label}"
    fi

    local out_name="${name}-${suffix}"
    local out_path="${BUILD_DIR}/${out_name}"

    echo -e "${CYAN}[$os/$label] Building $name...${NC}"

    GOOS="$os" GOARCH="$goarch" CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "$LDFLAGS" \
        -o "$out_path" \
        "$src" || {
        echo -e "${RED}  ERROR: Build failed for $name ($os/$label)${NC}"
        exit 1
    }

    BUILT+=("$out_path")

    local size
    size=$(du -h "$out_path" | cut -f1)
    echo -e "${GREEN}  -> $out_name ($size)${NC}"
}

echo -e "${YELLOW}========================================"
echo -e "  VLGR Build Script"
echo -e "  Version: ${GIT_VERSION} (${GIT_COMMIT}, ${BUILD_DATE})"
echo -e "========================================${NC}"
echo ""

cd "$PROJECT_ROOT"

go_ver=$(go version 2>&1 || true)
echo -e "${GRAY}Go version: $go_ver${NC}"

echo -e "${CYAN}Downloading dependencies...${NC}"
go mod tidy
go mod download
echo ""

for entry in "${ALL_TARGETS[@]}"; do
    read -r os goarch label ext <<< "$entry"

    if [ -n "$TARGETS_FILTER" ]; then
        key="${os}/${label}"
        # shellcheck disable=SC2076
        if [[ ! ",${TARGETS_FILTER}," =~ ",${key}," ]]; then
            continue
        fi
    fi

    echo -e "${YELLOW}--- $os ($label) ---${NC}"

    for app_def in "${APPS[@]}"; do
        name="${app_def%%:*}"
        src="${app_def##*:}"
        build_app "$os" "$goarch" "$label" "$ext" "$name" "$src"
    done
    echo ""
done

# Sign every artifact so the self-updater can verify it before installing.
if [ -n "$SIGN_KEY" ]; then
    echo -e "${CYAN}Signing ${#BUILT[@]} binaries (public key $SIGN_PUBKEY)...${NC}"
    go run ./scripts/sign sign "$SIGN_KEY" "${BUILT[@]}"
    echo ""
else
    echo -e "${YELLOW}No signing key set (VLGR_SIGNING_KEY / -s): binaries are unsigned and will not self-update.${NC}"
    echo ""
fi

echo -e "${YELLOW}========================================"
echo -e "  ${GREEN}Build complete!${YELLOW}"
echo -e "  Output:${NC}"

for f in "$BUILD_DIR"/*; do
    if [ -f "$f" ]; then
        name=$(basename "$f")
        size=$(du -h "$f" | cut -f1)
        echo -e "    $name ($size)"
    fi
done | sort

echo -e "${YELLOW}========================================${NC}"
