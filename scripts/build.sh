#!/usr/bin/env bash
#
# Build script for Linux (Bash)
# Cross-compiles VLGR server and client for Linux and Windows.
#
# Usage:
#   ./scripts/build.sh              # Build both platforms
#   ./scripts/build.sh --linux-only # Linux only
#   ./scripts/build.sh --win-only   # Windows only
#
# Requires: Go 1.22+ installed and in PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BUILD_DIR="${PROJECT_ROOT}/build"
WIN_DIR="${BUILD_DIR}/windows"
LINUX_DIR="${BUILD_DIR}/linux"

LDFLAGS="-s -w"  # strip debug info, shrink binary
BUILD_TIME="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

WINDOWS_ONLY=false
LINUX_ONLY=false

for arg in "$@"; do
    case "$arg" in
        --win-only)   WINDOWS_ONLY=true ;;
        --linux-only) LINUX_ONLY=true ;;
    esac
done

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
GRAY='\033[0;37m'
NC='\033[0m'

build_target() {
    local os="$1"
    local arch="$2"
    local out_dir="$3"
    local ext="$4"

    export GOOS="$os"
    export GOARCH="$arch"
    export CGO_ENABLED=0

    mkdir -p "$out_dir"

    local apps=("vlgr-server:./cmd/server" "vlgr-client:./cmd/client")

    for app_def in "${apps[@]}"; do
        local name="${app_def%%:*}"
        local src="${app_def##*:}"
        local out_name="${name}${ext}"
        local out_path="${out_dir}/${out_name}"

        echo -e "${CYAN}[$os/$arch] Building $name...${NC}"

        go build \
            -trimpath \
            -ldflags "$LDFLAGS" \
            -o "$out_path" \
            "$src" || {
            echo -e "${RED}  ERROR: Build failed for $name ($os/$arch)${NC}"
            exit 1
        }

        local size
        size=$(du -h "$out_path" | cut -f1)
        echo -e "${GREEN}  -> $out_name ($size)${NC}"
    done
}

echo -e "${YELLOW}========================================"
echo -e "  VLGR Build Script"
echo -e "  Build time: $BUILD_TIME"
echo -e "========================================${NC}"
echo ""

cd "$PROJECT_ROOT"

echo -e "${CYAN}Downloading dependencies...${NC}"
go mod tidy
go mod download
echo ""

go_ver=$(go version 2>&1 || true)
echo -e "${GRAY}Go version: $go_ver${NC}"
echo ""

if ! $LINUX_ONLY; then
    echo -e "${YELLOW}--- Windows (amd64) ---${NC}"
    build_target "windows" "amd64" "$WIN_DIR" ".exe"
    echo ""
fi

if ! $WINDOWS_ONLY; then
    echo -e "${YELLOW}--- Linux (amd64) ---${NC}"
    build_target "linux" "amd64" "$LINUX_DIR" ""
    echo ""
fi

echo -e "${YELLOW}========================================"
echo -e "  ${GREEN}Build complete!${YELLOW}"
echo -e "  Output:${NC}"

for d in "$WIN_DIR" "$LINUX_DIR"; do
    if [ -d "$d" ]; then
        echo -e "${GRAY}    $d${NC}"
        for f in "$d"/*; do
            if [ -f "$f" ]; then
                name=$(basename "$f")
                size=$(du -h "$f" | cut -f1)
                echo -e "      $name ($size)"
            fi
        done
    fi
done

echo -e "${YELLOW}========================================${NC}"
