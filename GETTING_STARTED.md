# VLGR Production Deployment Guide

Deploy VLGR on a VPS with Caddy as reverse proxy, Cloudflare DNS, and Let's Encrypt wildcard certificates. Zero conflicts with other projects on the same VPS.

---

## Architecture

```
Internet
    │
    ▼
Cloudflare DNS (DNS-only, gray cloud)
    │  A  tunnel.domain.com       → VPS IP
    │  A  *.tunnel.domain.com     → VPS IP
    ▼
Caddy (VPS, ports 443/80)
    │  tunnel.domain.com          → HTTPS → 127.0.0.1:4443  (WebSocket)
    │  *.tunnel.domain.com        → HTTPS → 127.0.0.1:8080  (public HTTP)
    ▼
VLGR Server (VPS, loopback only)
    │  127.0.0.1:4443  — WebSocket for clients
    │  127.0.0.1:8080  — HTTP for external requests
    ▼
VLGR Client (your home machine)
    │  Connects via WSS → Caddy → VLGR → proxies to localhost:3000
```

VLGR binds only to `127.0.0.1`. All TLS termination and routing is handled by Caddy. No port conflicts with other projects — Caddy routes by `Host` header, not by port.

---

## Quick Deploy (Auto)

The [`deploy-server.sh`](scripts/deploy-server.sh) script automates the entire VPS setup — downloads a pre-built binary (or builds from source as fallback), creates a systemd service, and optionally installs Caddy with Cloudflare DNS plugin.

### One-liner (requires root)

```bash
curl -sL https://github.com/varmtlys/vlgr/raw/main/scripts/deploy-server.sh | sudo bash -s -- \
  -d tunnel.domain.com -t mysecret --release latest
```

### Locally

```bash
sudo ./scripts/deploy-server.sh -d tunnel.domain.com -t mysecret --release latest --caddy --cf-token <CF_API_TOKEN>
```

### Options

| Flag | Description |
|------|-------------|
| `-d <domain>` | Base domain (required, e.g. `tunnel.domain.com`) |
| `-t <token>` | Auth token for clients (default: auto-generated) |
| `-U <user>` | System user for the service (default: `nobody`) |
| `-g <group>` | System group (auto-detected per distro) |
| `-p <path>` | Install path (default: `/opt/vlgr`) |
| `--caddy` | Install & configure Caddy with Cloudflare DNS plugin |
| `--cf-token <t>` | Cloudflare API token (requires `--caddy`) |
| `--no-service` | Skip systemd service creation |
| `--no-build` | Skip the Go build — use a pre-built binary from the local `build/` directory |
| `--release <v>` | Download from GitHub release: `latest` (default) or `v1.0` |
| `--ref <ref>` | Git ref for source build (default: `main`) |
| `--src-sha256 <h>` | Verify the cloned source tree matches this sha256 (optional) |
| `-u, --uninstall` | Remove VLGR server, service, binary and config |

### What it does

1. Detects your Linux distro (Debian, Ubuntu, RHEL, Arch, Alpine, openSUSE, Void)
2. Installs dependencies (curl, wget, git)
3. Downloads the latest pre-built `vlgr-server` binary from GitHub Releases
4. Falls back to installing Go 1.26+ and building from source if no release binary found
5. Creates a dedicated system user (default: `nobody`)
6. Sets up directories (`/opt/vlgr/bin`, `/opt/vlgr/logs`, `/etc/vlgr`)
7. Writes config to `/etc/vlgr/vlgr-server.conf`
8. Optionally installs Caddy first (`--caddy`): adds the Cloudflare DNS plugin, appends reverse-proxy config, and creates a `caddy.service` unit when Caddy came from a plain binary download
9. Creates and enables a systemd service (`vlgr-server`); when `caddy.service` exists, the unit declares `Requires=caddy.service` + `After=caddy.service`, so systemd starts Caddy together with vlgr-server and refuses to start vlgr-server if Caddy cannot start

After the script finishes, the server is running and ready for clients.

Grab the pre-built client binary for your platform from [GitHub Releases](https://github.com/varmtlys/vlgr/releases) or build it locally:

```bash
# Build client: GOOS=<os> GOARCH=<arch> go build -trimpath -ldflags="-s -w" -o build/vlgr-client-<os>-<arch> ./cmd/client
./vlgr-client -s tunnel.domain.com:443 -p 3000 --tls -t <token>

# Multiple tunnels (ports match subdomains by position):
./vlgr-client -s tunnel.domain.com:443 -p "8080,3000,5000" -u "api,web,admin" --tls -t <token>

# With a system tray icon (Windows/Linux):
./vlgr-client -s tunnel.domain.com:443 -p 3000 --tls -t <token> --tray

# Add / remove forwards on an already running client:
./vlgr-client --add "5000 mysub"
./vlgr-client --del 5000
```

> The auto-deploy script automates Steps 3–6 (server-side setup). Steps 1–2 (Cloudflare DNS & API token) and Step 7 (client connection) must be done manually. For a manual setup or to understand each component, follow the detailed guide.

---

## Step 1: Cloudflare DNS

Go to Cloudflare Dashboard → DNS → Records. Add two A records:

| Type | Name | Value | Proxy status |
|---|---|---|---|
| A | `tunnel` | `<VPS IP>` | **DNS only** (gray cloud) |
| A | `*.tunnel` | `<VPS IP>` | **DNS only** (gray cloud) |

**Gray cloud is required.** Cloudflare's free plan does not proxy wildcard subdomains. Caddy will issue certificates directly via Let's Encrypt DNS-01 challenge.

---

## Step 2: Create Cloudflare API Token

1. Go to [Cloudflare Dashboard](https://dash.cloudflare.com) → profile icon (top right) → **My Profile**.
2. **API Tokens** tab → **Create Token**.
3. Choose **Edit zone DNS** template (or click **Create Custom Token**):

   | Field | Value |
   |---|---|
   | Token name | `caddy-vlgr` |
   | Permissions | `Zone` — `DNS` — `Edit` |
   | Zone Resources | `Include` — `Specific zone` — `domain.com` |

4. Click **Continue to summary** → **Create Token**.
5. **Copy the token immediately** — it is shown only once.

---

## Step 3: Rebuild Caddy with Cloudflare DNS Plugin

The `caddy-dns/cloudflare` module is required for wildcard certificate issuance via DNS-01 challenge. Caddy versions before 2.8 may not have `add-package`. Below are all methods.

### Check current Caddy version and installation

```bash
caddy version
which caddy
```

### Method A: Official package (Caddy ≥ 2.8)

```bash
sudo caddy add-package github.com/caddy-dns/cloudflare
sudo systemctl restart caddy
```

### Method B: Rebuild via xcaddy (any version)

```bash
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

sudo systemctl stop caddy

~/go/bin/xcaddy build \
    --with github.com/caddy-dns/cloudflare \
    --output /usr/bin/caddy

sudo chmod +x /usr/bin/caddy
```

### Method C: Rebuild via xcaddy to a different path

If Caddy is installed at `/usr/local/bin/caddy` or another location:

```bash
which caddy                                 # note the path
sudo systemctl stop caddy
~/go/bin/xcaddy build \
    --with github.com/caddy-dns/cloudflare \
    --output /path/to/caddy
```

### Verify the plugin is loaded

```bash
caddy list-modules 2>/dev/null | grep cloudflare
# Expected output: dns.providers.cloudflare
```

If you see nothing, the plugin is not loaded — repeat the rebuild.

---

## Step 4: Store Cloudflare Token

Create the environment file:

```bash
sudo mkdir -p /etc/caddy
sudo tee /etc/caddy/.env << 'EOF'
CF_API_TOKEN=your-api-token-here
EOF
sudo chmod 600 /etc/caddy/.env
```

Attach it to the Caddy systemd unit:

```bash
sudo mkdir -p /etc/systemd/system/caddy.service.d
sudo tee /etc/systemd/system/caddy.service.d/override.conf << 'EOF'
[Service]
EnvironmentFile=/etc/caddy/.env
EOF
```

Reload systemd:

```bash
sudo systemctl daemon-reload
```

---

## Step 5: Configure Caddy

Edit `/etc/caddy/Caddyfile`. Add the VLGR section alongside your existing projects:

```caddyfile
# ============================
# VLGR Tunnel
# ============================

tunnel.domain.com {
    reverse_proxy 127.0.0.1:4443
}

*.tunnel.domain.com {
    tls {
        dns cloudflare {env.CF_API_TOKEN}
    }
    reverse_proxy 127.0.0.1:8080
}

# ============================
# Your other projects (unchanged)
# ============================
# project1.domain.com { ... }
# project2.domain.com { ... }
```

Format and restart:

```bash
caddy fmt --overwrite /etc/caddy/Caddyfile
sudo systemctl restart caddy
sudo journalctl -u caddy --no-pager -n 20
```

You should see: `certificate obtained successfully` for `*.tunnel.domain.com`.

Verify the proxy works:

```bash
curl https://tunnel.domain.com/_tunnel
# Returns "Bad Request" (not a WebSocket call) — Caddy is proxying correctly
```

---

## Step 6: Deploy VLGR Server

### Download pre-built binary (recommended)

```bash
curl -fsSL -o vlgr-server https://github.com/varmtlys/vlgr/releases/latest/download/vlgr-server-linux-amd64
chmod +x vlgr-server
```

Or use the deploy script with `--release latest` (see Quick Deploy above).

### Build from source (fallback)

```bash
cd vlgr
git pull
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/vlgr-server-linux-amd64 ./cmd/server
```

### Manual launch (test first)

```bash
./build/vlgr-server-linux-amd64 \
    --addr 127.0.0.1:4443 \
    --http 127.0.0.1:8080 \
    --domain tunnel.domain.com
```

Verify the server binds to loopback only:

```bash
ss -tlnp | grep -E "4443|8080"
# Expected: 127.0.0.1:4443 and 127.0.0.1:8080
```

If you see `*:4443` or `*:8080`, the server is listening on all interfaces — fix the `-addr` and `-http` flags.

### Systemd unit (autostart)

Create `/etc/systemd/system/vlgr-server.service`:

```ini
[Unit]
Description=VLGR Tunnel Server
After=network.target caddy.service
# Start Caddy together with vlgr-server; refuse to start without it
Requires=caddy.service

[Service]
Type=simple
User=nobody
Group=nogroup
WorkingDirectory=/opt/vlgr
EnvironmentFile=/etc/vlgr/vlgr-server.conf
# Token is NOT passed as a flag (visible in /proc/PID/cmdline) —
# the server reads VLGR_TOKEN from the environment (EnvironmentFile above).
# Append optional flags as needed, e.g. --trusted-proxy-hops 1 (behind Caddy),
# --basic-auth / --allow-ips, --admin 127.0.0.1:4041, --tls-passthrough,
# --ssh / --ssh-ports, --autoupdate.
ExecStart=/opt/vlgr/vlgr-server --addr ${VLGR_WS_ADDR} --http ${VLGR_HTTP_ADDR} --domain ${VLGR_DOMAIN}
Restart=always
RestartSec=5
TimeoutStopSec=10

NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/opt/vlgr/logs
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
```

Create the config file `/etc/vlgr/vlgr-server.conf`:

```bash
sudo mkdir -p /etc/vlgr
sudo tee /etc/vlgr/vlgr-server.conf << 'EOF'
VLGR_DOMAIN="tunnel.domain.com"
VLGR_WS_ADDR="127.0.0.1:4443"
VLGR_HTTP_ADDR="127.0.0.1:8080"
VLGR_TOKEN="your-secret-token"
EOF
sudo chmod 600 /etc/vlgr/vlgr-server.conf
```

Deploy:

```bash
sudo mkdir -p /opt/vlgr
sudo cp build/vlgr-server-linux-amd64 /opt/vlgr/vlgr-server
sudo systemctl daemon-reload
sudo systemctl enable vlgr-server
sudo systemctl start vlgr-server
sudo systemctl status vlgr-server
```

---

## Step 7: Connect the Client

Download the pre-built client binary or build it:

```bash
# Download pre-built:
curl -fsSL -o vlgr-client https://github.com/varmtlys/vlgr/releases/latest/download/vlgr-client-linux-amd64
chmod +x vlgr-client

# Or build:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/vlgr-client-linux-amd64 ./cmd/client
```

Run it:

```bash
./build/vlgr-client-linux-amd64 \
    -s tunnel.domain.com:443 \
    -p 3000 \
    --tls
```

Expected output (single tunnel):

```
[client] authenticated
[client] tunnel ready: a3f8b2c1d4e5f6a7.tunnel.domain.com -> localhost:3000
========================================
  Tunnels: a3f8b2c1d4e5f6a7.tunnel.domain.com
  Local:   3000
========================================
```

Expected output (multiple tunnels):

```
[client] authenticated
[client] tunnel ready: api.tunnel.domain.com -> localhost:8080
[client] tunnel ready: web.tunnel.domain.com -> localhost:3000
========================================
  Tunnels: api.tunnel.domain.com, web.tunnel.domain.com
  Local:   8080,3000
========================================
```

`-tls` is required — the client must use `wss://` because Caddy terminates TLS on port 443.

### Managing forwards on a running client

A running instance can be modified without restarting it:

```bash
# Add a forward (port + subdomain):
./vlgr-client --add "5000 mysub"

# Remove a forward (frees its subdomain on the server):
./vlgr-client --del 5000
```

`--add`/`--del` must be used without any other flags. When several client
instances run in parallel, a console menu lists each instance with its
current forwards so you can pick the target (Ctrl+C cancels).

### System tray icon

On Windows and Linux, add `--tray` to show a tray icon for the instance
(one icon per running instance). The menu lets you view the active
forwards, open them in the browser, remove them, and add new ones via an
input dialog (`<port> [subdomain]`; on Linux this needs `zenity` or
`kdialog`). *Quit* stops the instance.

### Raw TCP tunnels

For non-HTTP services (SSH, databases, game servers), expose a local TCP
port instead of an HTTP forward. Enable a public port range on the server
and open it on the firewall (these ports bypass Caddy and are reached
directly):

```bash
# Server: allow public TCP ports 20000-20100
./vlgr-server --domain tunnel.domain.com --tcp-ports 20000-20100

# Client: expose local SSH on an auto-assigned public port (or pin it with 22:2222)
./vlgr-client -s tunnel.domain.com:443 --tls --tcp 22
```

The client prints the assigned public address, e.g. `tcp://tunnel.domain.com:20000`.

### Traffic inspector

Add `--inspect 127.0.0.1:4040` to record the HTTP requests passing through
the tunnel and browse them at `http://127.0.0.1:4040` — method, path,
status, timing, headers and bodies, with live updates and one-click replay
to the local app. `--inspect-limit N` caps how many requests are kept
(default 1000, max 100000; oldest drop off as new arrive). The dashboard can
export the captured traffic as **HAR** (`.har`, the standard HTTP Archive
format read by browser DevTools, Charles, Fiddler, Postman…) or as a plain
human-readable **text** dump. The dashboard is loopback-only — it rejects
non-loopback callers, checks the `Host` header to block DNS-rebinding, and
refuses cross-site replay — and is never exposed through the tunnel.

### Endpoint protection (server)

`--basic-auth user:pass` and `--allow-ips <cidr,...>` guard every public
request before it enters a tunnel. Because the server sits behind Caddy, set
`--trusted-proxy-hops 1` so the IP allowlist reads the address Caddy appended
to `X-Forwarded-For` rather than a client-supplied value:

```bash
./vlgr-server --domain tunnel.domain.com \
    --basic-auth ops:s3cret --allow-ips 10.0.0.0/8,203.0.113.4 --trusted-proxy-hops 1
```

The default `--trusted-proxy-hops 0` uses the direct peer IP and ignores
`X-Forwarded-For` entirely — safe when nothing is proxying in front. mTLS and
OAuth are intentionally left to Caddy, which terminates TLS.

### Admin API + dashboard (server)

`--admin 127.0.0.1:4041` exposes a read-only REST API (`/api/status`,
`/api/tunnels`) and a small live status page. Loopback callers are trusted, so
the local dashboard needs no token; a remote caller must send the token as a
`Bearer` token, and with no token set remote access is refused. The `Host`
check blocks DNS-rebinding. Reach it over an SSH tunnel or a protected Caddy
vhost — do not expose it publicly.

### TLS-passthrough tunnels (SNI)

To expose a local **TLS** service without the relay terminating TLS, run a
public SNI listener on the server and register the local port from the client:

```bash
# Server: one public TLS port multiplexed by SNI (bypasses Caddy — open it on the firewall)
./vlgr-server --domain tunnel.domain.com --tls-passthrough :8443

# Client: expose local TLS on 8443 (optionally request a subdomain)
./vlgr-client -s tunnel.domain.com:443 --tls --tls-tunnel 8443:mysub
```

Certificates stay entirely on the local service; the server routes by the SNI
hostname and relays raw bytes.

### Agentless SSH tunnels (`ssh -R`)

Users without a vlgr client can expose a local service with stock `ssh`. Enable
an SSH endpoint and a public port range on the server:

```bash
# Server (open --ssh and the port range on the firewall)
./vlgr-server --domain tunnel.domain.com --ssh :2222 --ssh-ports 20200-20300 \
    --ssh-hostkey /etc/vlgr/ssh_host_ed25519

# User (no vlgr client): forward local :3000 to an auto-assigned public port
ssh -N -R 0:localhost:3000 tunnel.domain.com -p 2222
```

When the server has a token it is required as the SSH password; without a token
the endpoint accepts anyone and logs a warning at startup, so set a token when
the port range is publicly reachable. `--ssh-hostkey <path>` persists the host
key so clients don't see key-changed warnings across restarts.

### Signed self-update

`--autoupdate` (server and client, default off) polls GitHub releases hourly
and, on a newer tag, downloads the matching binary, **verifies its detached
ed25519 signature**, then swaps it on disk and relaunches. This only works with
binaries built with a signing key (see the [signed releases](#signed-releases)
section); an unsigned build refuses to update (fail-safe). Development builds
are skipped. On relaunch listeners are closed so the replacement can rebind the
same ports; connected clients reconnect on their own.

---

## Step 8: End-to-End Test

From any device on the internet:

```bash
curl https://a3f8b2c1d4e5f6a7.tunnel.domain.com/
# Returns your localhost:3000 content
```

---

## Signed releases

`--autoupdate` only installs binaries whose detached ed25519 signature verifies
against a public key compiled into the binary. To produce such binaries you
sign at build time.

Generate a signing key once and keep the private seed secret (e.g. a CI
secret — never commit it):

```powershell
go run .\scripts\sign genkey
# private (seed, keep secret): <64 hex chars>   → store as $VLGR_SIGNING_KEY
# public  (compile in):        <64 hex chars>   → embedded automatically
```

Build with the key set. `build.ps1` derives the public key, compiles it into
every binary via `-ldflags "-X vlgr/internal/selfupdate.SigningPublicKey=..."`,
and writes a `<binary>.sig` next to each artifact:

```powershell
$env:VLGR_SIGNING_KEY = "<private-seed-hex>"
.\scripts\build.ps1
```

Upload both the binary and its `.sig` to the GitHub release. The updater fetches
`<asset>` and `<asset>.sig`, verifies the signature, and only then installs.
Without a key, `build.ps1` produces unsigned binaries and self-update refuses to
run — this is intentional (fail-safe). Keep the private seed stable across
releases: changing it breaks auto-update for already-deployed nodes, which then
need a manual reinstall.

---

## Cloudflare-Specific Notes

### Gray cloud vs orange cloud

| Mode | Description | Wildcard support (free plan) |
|---|---|---|
| Gray (DNS only) | DNS resolves directly to VPS IP. Caddy handles TLS. | Yes |
| Orange (proxied) | Cloudflare proxies traffic. Hidden VPS IP. | No (requires Business plan) |

For VLGR, **use gray cloud** for both `tunnel` and `*.tunnel` records. Caddy issues certificates directly via Let's Encrypt. Cloudflare's proxy is unnecessary — Caddy already terminates TLS.

### Why not orange cloud?

- Wildcard proxying requires Cloudflare Business or Enterprise plan.
- Cloudflare would insert itself between the external user and Caddy, adding latency.
- No benefit: Caddy already handles TLS, rate limiting can be done at the Caddy or VLGR level.

### DNS propagation

After adding DNS records, wait 1–5 minutes for propagation. Verify:

```bash
dig tunnel.domain.com +short
# Should return your VPS IP

dig abc123.tunnel.domain.com +short
# Should also return your VPS IP (wildcard)
```

---

## Troubleshooting

### Caddy fails to start after config change

```bash
caddy validate --config /etc/caddy/Caddyfile
sudo journalctl -u caddy --no-pager -n 30
```

Common causes:
- `dns cloudflare` — plugin not installed → rebuild with xcaddy (Step 3, Method B).
- `API token '' appears invalid` — `CF_API_TOKEN` not set → check `/etc/caddy/.env` and the override file (Step 4).
- TLS provisioning error — Cloudflare API token lacks DNS:Edit permission (Step 2).

### Client cannot connect

```bash
# On VPS — is the server listening on loopback?
ss -tlnp | grep -E "4443|8080"
# Must show 127.0.0.1, NOT * or 0.0.0.0

# Is Caddy proxying to the right ports?
curl -v https://tunnel.domain.com/_tunnel 2>&1 | head -20

# From home machine — can you reach the VPS at all?
curl -v https://tunnel.domain.com/ 2>&1 | head -20
```

### "Bad Gateway" on external requests

- VLGR server is not running: `sudo systemctl status vlgr-server`.
- Port mismatch: Caddy proxies to 8080, but VLGR listens on a different port.
- Subdomain extraction fails: check that `extractSubdomain` works with your domain format. For `abc.tunnel.domain.com`, the subdomain is `abc`. Debug by checking server logs: `sudo journalctl -u vlgr-server -f`.

### Existing Caddy projects broken after adding VLGR

Should not happen — Caddy routes by `Host` header. Your existing `project1.domain.com` block is unaffected by adding `tunnel.domain.com` and `*.tunnel.domain.com` blocks. If something broke, check for syntax errors in the Caddyfile:

```bash
caddy validate --config /etc/caddy/Caddyfile
```
