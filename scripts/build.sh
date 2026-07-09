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
# Requires: Go 1.26+ installed and in PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BUILD_DIR="${PROJECT_ROOT}/build"

LDFLAGS="-s -w"

TARGETS_FILTER=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        -t) TARGETS_FILTER="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

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

    local size
    size=$(du -h "$out_path" | cut -f1)
    echo -e "${GREEN}  -> $out_name ($size)${NC}"
}

echo -e "${YELLOW}========================================"
echo -e "  VLGR Build Script"
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
        local key="${os}/${label}"
        # shellcheck disable=SC2076
        if [[ ! ",${TARGETS_FILTER}," =~ ",${key}," ]]; then
            continue
        fi
    fi

    echo -e "${YELLOW}--- $os ($label) ---${NC}"

    for app_def in "${APPS[@]}"; do
        local name="${app_def%%:*}"
        local src="${app_def##*:}"
        build_app "$os" "$goarch" "$label" "$ext" "$name" "$src"
    done
    echo ""
done

echo -e "${YELLOW}========================================"
echo -e "  ${GREEN}Build complete!${YELLOW}"
echo -e "  Output:${NC}"

for f in "$BUILD_DIR"/*; do
    if [ -f "$f" ]; then
        local name; name=$(basename "$f")
        local size; size=$(du -h "$f" | cut -f1)
        echo -e "    $name ($size)"
    fi
done | sort

echo -e "${YELLOW}========================================${NC}"
