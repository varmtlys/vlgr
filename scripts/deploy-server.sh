#!/usr/bin/env bash
#
# VLGR Server auto-deploy script
# Supported distros: Debian/Ubuntu, RHEL/CentOS/Fedora, Arch, Alpine, openSUSE, Void
#
# Usage:
#   curl -sL https://github.com/varmtlys/vlgr/raw/main/scripts/deploy-server.sh | sudo bash -s -- \
#     -d tunnel.domain.com -t mytoken
#
#   # Or locally:
#   sudo ./scripts/deploy-server.sh -d tunnel.domain.com -t mytoken
#
# Options:
#   -d <domain>     Base domain (required, e.g. tunnel.domain.com)
#   -t <token>      Auth token for clients (default: auto-generated)
#   -U <user>       System user to run the service (default: nobody)
#   -g <group>      System group (default: nogroup on Debian, nobody on RHEL)
#   -p <path>       Install path (default: /opt/vlgr)
#   -u              Uninstall VLGR server (removes service, binary, config)
#   --caddy         Install/configure Caddy with Cloudflare DNS plugin
#   --cf-token <t>  Cloudflare API token (requires --caddy)
#   --no-service    Skip systemd service creation
#   --no-build      Skip Go build, always build from local source
#   --release <v>   Download pre-built binary from GitHub release (e.g. "latest" or "v1.0")
#   --ref <ref>     Git ref to checkout when building from source (default: main)
#   --src-sha256 <h> Verify the cloned source tree matches this sha256 (optional, strong)
#   --help          Show this help
#
# By default the script tries to download the latest GitHub release binary.
# If no release exists or download fails, it builds from source.

set -euo pipefail

# ─── Config ──────────────────────────────────────────────────────────────────
DOMAIN=""
TOKEN=""
SERVICE_USER="nobody"
SERVICE_GROUP=""
INSTALL_PATH="/opt/vlgr"
INSTALL_CADDY=false
CF_TOKEN=""
NO_SERVICE=false
NO_BUILD=false
UNINSTALL=false
VLGR_RELEASE=""
GIT_REF="main"
SRC_SHA256=""
REPO_URL="https://github.com/varmtlys/vlgr.git"
RELEASES_URL="https://github.com/varmtlys/vlgr/releases"

# ─── Parse args ──────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    -d) DOMAIN="$2"; shift 2 ;;
    -t) TOKEN="$2"; shift 2 ;;
    -u|--uninstall) UNINSTALL=true; shift ;;
    -U) SERVICE_USER="$2"; shift 2 ;;
    -g) SERVICE_GROUP="$2"; shift 2 ;;
    -p) INSTALL_PATH="$2"; shift 2 ;;
    --caddy) INSTALL_CADDY=true; shift ;;
    --cf-token) CF_TOKEN="$2"; shift 2 ;;
    --no-service) NO_SERVICE=true; shift ;;
    --no-build) NO_BUILD=true; shift ;;
    --release) VLGR_RELEASE="$2"; shift 2 ;;
    --ref) GIT_REF="$2"; shift 2 ;;
    --src-sha256) SRC_SHA256="$2"; shift 2 ;;
    --help) sed -n '/^# Usage:/,/^$/p' "$0"; exit 0 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# ─── Helpers ──────────────────────────────────────────────────────────────────
log()  { echo -e "\033[1;32m[*]\033[0m $*"; }
warn() { echo -e "\033[1;33m[!]\033[0m $*"; }
err()  { echo -e "\033[1;31m[!]\033[0m $*"; }

# ─── Root check ───────────────────────────────────────────────────────────────
if [[ $EUID -ne 0 ]]; then
  err "This script must be run as root"
  exit 1
fi

# ─── Uninstall ─────────────────────────────────────────────────────────────────
uninstall() {
  echo ""
  echo -e "\033[1;33m========================================\033[0m"
  echo -e "\033[1;33m  VLGR Server Uninstall\033[0m"
  echo -e "\033[1;33m========================================\033[0m"
  echo ""

  log "Stopping and disabling service..."
  systemctl stop vlgr-server 2>/dev/null || true
  systemctl disable vlgr-server 2>/dev/null || true

  log "Removing systemd unit..."
  rm -f /etc/systemd/system/vlgr-server.service
  systemctl daemon-reload 2>/dev/null || true

  log "Removing binary and install path..."
  rm -rf "$INSTALL_PATH"

  log "Removing config..."
  rm -rf /etc/vlgr

  echo ""
  echo -e "\033[1;32m  VLGR Server uninstalled successfully!\033[0m"
  echo ""
}

# ─── Early exit for uninstall ─────────────────────────────────────────────────
if $UNINSTALL; then
  uninstall
  exit 0
fi

# ─── Domain required for deploy ───────────────────────────────────────────────
if [[ -z "$DOMAIN" ]]; then
  echo "Usage: $0 -d <domain> [-t <token>] [options]"
  echo "  -d <domain>  Base domain (required, e.g. tunnel.domain.com)"
  exit 1
fi

if [[ -z "$TOKEN" ]]; then
  TOKEN="vlgr-$(tr -dc a-f0-9 < /dev/urandom | head -c 16 2>/dev/null || openssl rand -hex 8)"
fi

# ─── Distro detection ────────────────────────────────────────────────────────
detect_distro() {
  DISTRO_ID=""
  DISTRO_LIKE=""
  if [[ -f /etc/os-release ]]; then
    . /etc/os-release
    DISTRO_ID="$ID"
    DISTRO_LIKE="${ID_LIKE:-}"
  elif [[ -f /etc/debian_version ]]; then
    DISTRO_ID="debian"
  elif [[ -f /etc/redhat-release ]]; then
    DISTRO_ID="rhel"
  elif [[ -f /etc/alpine-release ]]; then
    DISTRO_ID="alpine"
  else
    DISTRO_ID="unknown"
  fi
  DISTRO_ID="${DISTRO_ID,,}"
  DISTRO_LIKE="${DISTRO_LIKE,,}"
}

detect_distro

is_ubuntu()  { [[ "$DISTRO_ID" == "ubuntu" ]]; }
is_debian()  { [[ "$DISTRO_ID" == "debian" ]] || [[ "$DISTRO_LIKE" == *"debian"* ]]; }
is_rhel()    { [[ "$DISTRO_ID" == "rhel" ]] || [[ "$DISTRO_ID" == "centos" ]] || [[ "$DISTRO_ID" == "fedora" ]] || [[ "$DISTRO_LIKE" == *"rhel"* ]] || [[ "$DISTRO_LIKE" == *"fedora"* ]]; }
is_arch()    { [[ "$DISTRO_ID" == "arch" ]] || [[ "$DISTRO_LIKE" == *"arch"* ]]; }
is_alpine()  { [[ "$DISTRO_ID" == "alpine" ]]; }
is_suse()    { [[ "$DISTRO_ID" == "opensuse"* ]] || [[ "$DISTRO_ID" == "suse"* ]]; }
is_void()    { [[ "$DISTRO_ID" == "void" ]]; }

has_cmd() { command -v "$1" &>/dev/null; }

get_group() {
  if [[ -n "$SERVICE_GROUP" ]]; then
    echo "$SERVICE_GROUP"
  elif [[ "$DISTRO_ID" == "alpine" ]]; then
    echo "nobody"
  elif [[ "$DISTRO_ID" == "arch" ]]; then
    echo "${SERVICE_USER}"
  elif is_debian || is_ubuntu; then
    echo "nogroup"
  else
    echo "nobody"
  fi
}

# ─── Install packages ────────────────────────────────────────────────────────
install_packages() {
  local pkgs=("$@")
  if [[ ${#pkgs[@]} -eq 0 ]]; then return; fi

  if is_ubuntu || is_debian; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq "${pkgs[@]}"
  elif is_rhel; then
    if has_cmd dnf; then
      dnf install -y "${pkgs[@]}"
    else
      yum install -y "${pkgs[@]}"
    fi
  elif is_arch; then
    pacman -Syu --noconfirm "${pkgs[@]}"
  elif is_alpine; then
    apk add "${pkgs[@]}"
  elif is_suse; then
    zypper install -y "${pkgs[@]}"
  elif is_void; then
    xbps-install -Sy "${pkgs[@]}"
  else
    warn "Unknown distro. Install manually: ${pkgs[*]}"
  fi
}

# ─── Install Go ───────────────────────────────────────────────────────────────
ensure_go() {
  if has_cmd go && [[ "$(go version 2>/dev/null)" =~ go1\.2[2-9] ]]; then
    log "Go $(go version | grep -oP 'go\S+') already installed"
    return
  fi

  log "Installing Go 1.22+..."

  if is_alpine || is_arch || is_suse || is_void; then
    install_packages go
  else
    local go_ver="1.22.5"
    local go_tar="go${go_ver}.linux-amd64.tar.gz"
    local go_sha="9a6baf11ae4b72b70dfa8a863235ac3d6f4aa3c1d82b0f3cd87a07957b9715c6"
    install_packages wget
    wget -q "https://go.dev/dl/${go_tar}" -O "/tmp/${go_tar}"
    echo "${go_sha}  /tmp/${go_tar}" | sha256sum -c --status || { err "Go SHA256 mismatch"; exit 1; }
    tar -C /usr/local -xzf "/tmp/${go_tar}"
    rm "/tmp/${go_tar}"
    if [[ ! -f /etc/profile.d/go.sh ]]; then
      echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
    fi
    export PATH="$PATH:/usr/local/go/bin"
  fi

  if has_cmd go; then
    log "Go $(go version | grep -oP 'go\S+') installed"
  else
    err "Go installation failed. Install manually: https://go.dev/dl/"
    exit 1
  fi
}

# ─── Detect architecture ──────────────────────────────────────────────────────
detect_arch() {
  local arch
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *)             echo "amd64" ;; # fallback
  esac
}

# ─── Download pre-built binary ────────────────────────────────────────────────
download_binary() {
  local version="$1"
  local arch
  arch="$(detect_arch)"
  local binary_name="vlgr-server-linux-${arch}"
  local download_url

  if [[ "$version" == "latest" || -z "$version" ]]; then
    download_url="${RELEASES_URL}/latest/download/${binary_name}"
  else
    download_url="${RELEASES_URL}/download/${version}/${binary_name}"
  fi

  log "Trying to download pre-built binary from ${download_url} ..."
  mkdir -p "${INSTALL_PATH}/bin"

  if has_cmd curl; then
    curl -fsSL -o "${INSTALL_PATH}/bin/vlgr-server" "${download_url}" 2>/dev/null || true
  elif has_cmd wget; then
    wget -q -O "${INSTALL_PATH}/bin/vlgr-server" "${download_url}" 2>/dev/null || true
  fi

  if [[ -s "${INSTALL_PATH}/bin/vlgr-server" ]]; then
    chmod 755 "${INSTALL_PATH}/bin/vlgr-server"
    log "Downloaded pre-built binary: $(du -h "${INSTALL_PATH}/bin/vlgr-server" | cut -f1)"
    return 0
  fi

  warn "Pre-built binary not available at ${download_url}, will build from source"
  return 1
}

# ─── Build VLGR server ───────────────────────────────────────────────────────
build_server() {
  local build_dir
  build_dir="$(dirname "$(dirname "$(readlink -f "$0")")")"

  if [[ -f "${build_dir}/go.mod" ]] && grep -q 'module vlgr' "${build_dir}/go.mod" 2>/dev/null; then
    log "Building from local source at ${build_dir}..."
    cd "$build_dir"
  else
    if [[ -d "${INSTALL_PATH}/src" ]]; then
      log "Updating cached source at ${INSTALL_PATH}/src (ref=${GIT_REF})..."
      cd "${INSTALL_PATH}/src"
      if ! git fetch --depth 1 origin "${GIT_REF}" 2>/dev/null; then
        log "Shallow fetch failed, trying full fetch..."
        git fetch origin "${GIT_REF}"
      fi
      if ! git checkout --quiet "${GIT_REF}" 2>/dev/null; then
        log "Checkout of ${GIT_REF} failed, trying FETCH_HEAD..."
        git checkout --quiet FETCH_HEAD
      fi
    else
      log "Cloning repository (ref=${GIT_REF}) to ${INSTALL_PATH}/src..."
      git clone --depth 1 --branch "${GIT_REF}" "$REPO_URL" "${INSTALL_PATH}/src"
      cd "${INSTALL_PATH}/src"
    fi
  fi

  if [[ -n "$SRC_SHA256" ]]; then
    if ! has_cmd git; then
      err "--src-sha256 requested but git is not available"
      exit 1
    fi
    local got
    got="$(git rev-parse HEAD)"
    if [[ "$got" != "$SRC_SHA256" ]]; then
      err "Source SHA256 mismatch: expected ${SRC_SHA256}, got ${got}"
      exit 1
    fi
    log "Source verified: ${got}"
  fi

  if has_cmd git; then
    log "Source: $(git log --oneline -1 2>/dev/null || echo 'unknown')"
  fi

  log "Downloading Go dependencies..."
  go mod tidy

  log "Building vlgr-server..."
  mkdir -p "${INSTALL_PATH}/bin"
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${INSTALL_PATH}/bin/vlgr-server" ./cmd/server

  if [[ ! -f "${INSTALL_PATH}/bin/vlgr-server" ]]; then
    err "Build failed"
    exit 1
  fi

  log "Server binary: ${INSTALL_PATH}/bin/vlgr-server ($(du -h "${INSTALL_PATH}/bin/vlgr-server" | cut -f1))"
}

# ─── Create user ──────────────────────────────────────────────────────────────
ensure_user() {
  local user="$1"
  local group
  group="$(get_group)"

  if ! getent group "$group" &>/dev/null; then
    if is_alpine || is_arch; then
      addgroup -S "$group" 2>/dev/null || true
    else
      groupadd -r "$group" 2>/dev/null || true
    fi
  fi

  if ! getent passwd "$user" &>/dev/null; then
    log "Creating system user: $user:$group"
    if is_alpine; then
      adduser -S -D -H -G "$group" -s /sbin/nologin "$user" 2>/dev/null
    elif is_arch; then
      useradd -r -s /usr/bin/nologin -g "$group" "$user" 2>/dev/null
    else
      useradd -r -s /usr/sbin/nologin -g "$group" "$user" 2>/dev/null
    fi
  fi
}

# ─── Install binary ───────────────────────────────────────────────────────────
install_binary() {
  # If binary already obtained (downloaded or built), just verify and chmod.
  if [[ -f "${INSTALL_PATH}/bin/vlgr-server" ]]; then
    chmod 755 "${INSTALL_PATH}/bin/vlgr-server"
    log "Binary ready: ${INSTALL_PATH}/bin/vlgr-server ($(du -h "${INSTALL_PATH}/bin/vlgr-server" | cut -f1))"
    return
  fi

  # --no-build: use pre-built binary from local build/ directory.
  local arch
  arch="$(detect_arch)"
  local script_dir
  script_dir="$(dirname "$(dirname "$(readlink -f "$0")")")"
  local prebuilt="${script_dir}/build/vlgr-server-linux-${arch}"

  if [[ -f "$prebuilt" ]]; then
    log "Using pre-built binary: $prebuilt"
    mkdir -p "${INSTALL_PATH}/bin"
    cp "$prebuilt" "${INSTALL_PATH}/bin/vlgr-server"
    chmod 755 "${INSTALL_PATH}/bin/vlgr-server"
  else
    err "Pre-built binary not found at $prebuilt. Build it first: GOOS=linux GOARCH=${arch} go build -trimpath -ldflags='-s -w' -o build/vlgr-server-linux-${arch} ./cmd/server"
    exit 1
  fi
}

# ─── Setup directories ────────────────────────────────────────────────────────
setup_dirs() {
  local group
  group="$(get_group)"

  mkdir -p "${INSTALL_PATH}/bin"
  mkdir -p "${INSTALL_PATH}/logs"
  mkdir -p /etc/vlgr

  chown -R "root:$group" "$INSTALL_PATH"
  chmod 755 "$INSTALL_PATH"
  chmod 755 "${INSTALL_PATH}/bin"
  chmod 750 "${INSTALL_PATH}/logs"
}

# ─── Write config ─────────────────────────────────────────────────────────────
write_config() {
  local group
  group="$(get_group)"

  cat > /etc/vlgr/vlgr-server.conf <<EOF
# VLGR Server configuration
# Generated by deploy-server.sh on $(date -u +"%Y-%m-%dT%H:%M:%SZ")

VLGR_DOMAIN=${DOMAIN}
VLGR_WS_ADDR=127.0.0.1:4443
VLGR_HTTP_ADDR=127.0.0.1:8080

# Client auth token — share this with your VLGR clients
VLGR_TOKEN=${TOKEN}

# Binary path
VLGR_BIN=${INSTALL_PATH}/bin/vlgr-server
EOF
  chmod 640 /etc/vlgr/vlgr-server.conf
  chown "root:$group" /etc/vlgr/vlgr-server.conf
}

# ─── Systemd service ──────────────────────────────────────────────────────────
write_service() {
  local user="$1"
  local group
  group="$(get_group)"
  local svc="/etc/systemd/system/vlgr-server.service"

  cat > "$svc" <<SERVICE
[Unit]
Description=VLGR Tunnel Server
Documentation=https://github.com/varmtlys/vlgr
After=network.target
Wants=network.target

[Service]
Type=simple
User=${user}
Group=${group}
WorkingDirectory=${INSTALL_PATH}
EnvironmentFile=/etc/vlgr/vlgr-server.conf
# Token intentionally NOT passed as a flag: /proc/PID/cmdline is world-readable.
# The server reads VLGR_TOKEN from the environment (EnvironmentFile above).
ExecStart=${INSTALL_PATH}/bin/vlgr-server \\
    -addr \${VLGR_WS_ADDR} \\
    -http \${VLGR_HTTP_ADDR} \\
    -domain \${VLGR_DOMAIN}
Restart=always
RestartSec=5
TimeoutStopSec=10

# Security hardening
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=${INSTALL_PATH}/logs
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
SystemCallFilter=@network-io @system-service
[Install]
WantedBy=multi-user.target
SERVICE

  chmod 644 "$svc"
  log "Systemd unit created: $svc"
}

# ─── Install Caddy ────────────────────────────────────────────────────────────
install_caddy() {
  download_caddy() {
    local caddy_ver="2.8.4"
    install_packages curl
    curl -sL "https://github.com/caddyserver/caddy/releases/download/v${caddy_ver}/caddy_${caddy_ver}_linux_amd64.tar.gz" | tar -C /usr/local/bin -xz caddy
    chmod +x /usr/local/bin/caddy
  }

  if has_cmd caddy; then
    log "Caddy already installed: $(caddy version 2>/dev/null || true)"
  else
    log "Installing Caddy..."
    if is_arch || is_alpine; then
      install_packages caddy
    elif is_rhel; then
      dnf install -y caddy 2>/dev/null || download_caddy
    else
      download_caddy
    fi
  fi

  if has_cmd caddy; then
    log "Caddy installed: $(caddy version 2>/dev/null || true)"
  else
    warn "Caddy installation failed. Install manually: https://caddyserver.com/download"
    return 1
  fi

  # Add Cloudflare DNS plugin
  if ! caddy list-modules 2>/dev/null | grep -q cloudflare; then
    log "Adding Cloudflare DNS plugin..."
    if has_cmd xcaddy; then
      local caddy_path
      caddy_path="$(command -v caddy)"
      systemctl stop caddy 2>/dev/null || true
      xcaddy build --with github.com/caddy-dns/cloudflare --output "$caddy_path"
      chmod +x "$caddy_path"
      log "Caddy rebuilt with cloudflare plugin"
    else
      if has_cmd go; then
        go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
        local caddy_path
        caddy_path="$(command -v caddy)"
        systemctl stop caddy 2>/dev/null || true
        ~/go/bin/xcaddy build --with github.com/caddy-dns/cloudflare --output "$caddy_path"
        chmod +x "$caddy_path"
        log "Caddy rebuilt with cloudflare plugin"
      else
        warn "Go not available. Install xcaddy and rebuild Caddy manually:"
        warn "  go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest"
        warn "  sudo ~/go/bin/xcaddy build --with github.com/caddy-dns/cloudflare --output /usr/bin/caddy"
      fi
    fi
  else
    log "Cloudflare plugin already installed"
  fi
}

# ─── Caddy config ─────────────────────────────────────────────────────────────
write_caddy_config() {
  local caddyfile="/etc/caddy/Caddyfile"
  local env_file="/etc/caddy/.env"

  mkdir -p /etc/caddy

  if [[ ! -f "$caddyfile" ]] || ! grep -q "$DOMAIN" "$caddyfile" 2>/dev/null; then
    cat >> "$caddyfile" <<CADDY

# ============================
# VLGR Tunnel — added by deploy-server.sh
# ============================

${DOMAIN} {
    reverse_proxy 127.0.0.1:4443
}

*.${DOMAIN} {
    tls {
        dns cloudflare {env.CF_API_TOKEN}
    }
    reverse_proxy 127.0.0.1:8080
}
CADDY
    log "Caddy config appended to $caddyfile"
  else
    log "Caddy config for $DOMAIN already exists in $caddyfile"
  fi

  if [[ -n "$CF_TOKEN" ]] || [[ -n "${CF_API_TOKEN:-}" ]]; then
    local token="${CF_TOKEN:-${CF_API_TOKEN:-}}"
    echo "CF_API_TOKEN=${token}" > "$env_file"
    chmod 600 "$env_file"

    mkdir -p /etc/systemd/system/caddy.service.d
    cat > /etc/systemd/system/caddy.service.d/vlgr-override.conf <<OVERRIDE
[Service]
EnvironmentFile=${env_file}
OVERRIDE
    systemctl daemon-reload 2>/dev/null || true
    log "Cloudflare token configured for Caddy"
  fi
}

# ─── Main ─────────────────────────────────────────────────────────────────────
main() {
  echo ""
  echo -e "\033[1;36m========================================\033[0m"
  echo -e "\033[1;36m  VLGR Server Deploy\033[0m"
  echo -e "\033[1;36m  Distro: ${DISTRO_ID}\033[0m"
  echo -e "\033[1;36m  Domain: ${DOMAIN}\033[0m"
  echo -e "\033[1;36m  Token:  ${TOKEN:0:20}...\033[0m"
  echo -e "\033[1;36m  Install: ${INSTALL_PATH}\033[0m"
  echo -e "\033[1;36m========================================\033[0m"
  echo ""

  # Phase 1: Dependencies (same set on every distro)
  log "Phase 1/5: Installing dependencies..."
  install_packages curl git wget

  # Phase 2: System user
  log "Phase 2/5: Creating system user..."
  ensure_user "$SERVICE_USER"

  # Phase 3: Setup directories
  log "Phase 3/5: Setting up directories..."
  setup_dirs

  # Phase 4: Obtain binary (download or build)
  log "Phase 4/5: Obtaining server binary..."
  local binary_obtained=false

  if $NO_BUILD; then
    log "Forcing local build (--no-build)..."
  elif download_binary "${VLGR_RELEASE:-latest}"; then
    binary_obtained=true
  fi

  if ! $binary_obtained; then
    log "Building from source..."
    ensure_go
    build_server
  fi
  install_binary
  write_config

  # Phase 5: Service
  log "Phase 5/5: Configuring service..."
  if ! $NO_SERVICE; then
    write_service "$SERVICE_USER"
    systemctl daemon-reload
    systemctl enable vlgr-server
    systemctl restart vlgr-server
    sleep 2
    if systemctl is-active --quiet vlgr-server; then
      log "vlgr-server service is active"
    else
      warn "vlgr-server service failed to start. Check: journalctl -u vlgr-server -n 30 --no-pager"
      systemctl status vlgr-server --no-pager || true
    fi
  fi

  # Optional: Caddy
  if $INSTALL_CADDY; then
    log "Optional: Installing and configuring Caddy..."
    install_caddy || warn "Caddy setup incomplete"
    write_caddy_config

    if has_cmd caddy; then
      caddy fmt --overwrite /etc/caddy/Caddyfile 2>/dev/null || true
      systemctl enable caddy 2>/dev/null || true
      systemctl restart caddy 2>/dev/null || true
    fi
  fi

  # ─── Summary ────────────────────────────────────────────────────────────────
  echo ""
  echo -e "\033[1;36m========================================\033[0m"
  echo -e "\033[1;32m  VLGR Server deployed successfully!\033[0m"
  echo -e "\033[1;36m========================================\033[0m"
  echo ""
  echo -e "  Domain:          \033[1;33m${DOMAIN}\033[0m"
  echo -e "  Auth token:      \033[1;33m${TOKEN}\033[0m"
  echo -e "  Binary:          ${INSTALL_PATH}/bin/vlgr-server"
  echo -e "  Config:          /etc/vlgr/vlgr-server.conf"
  echo -e "  Service:         vlgr-server ($(systemctl is-active vlgr-server 2>/dev/null || echo "not installed"))"
  echo ""
  echo -e "  \033[1;32mConnect a client:\033[0m"
  echo -e "  ./vlgr-client -s ${DOMAIN}:443 -p 3000 --tls -t ${TOKEN}"
  echo ""

  if $INSTALL_CADDY && has_cmd caddy; then
    echo -e "  \033[1;32mCaddy:\033[0m"
    echo -e "  Config:  /etc/caddy/Caddyfile"
    echo -e "  Status:  $(systemctl is-active caddy 2>/dev/null || echo "not running")"
    echo ""
  fi

  echo -e "  \033[1;32mCheck service status:\033[0m"
  echo -e "  sudo journalctl -u vlgr-server -f"
  echo ""
}

main
