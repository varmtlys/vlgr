# VLGR — ngrok alternative in Go

VLGR is an HTTP tunnel that exposes your local server to the internet through a public relay server. A full [ngrok](https://ngrok.com) clone written in Go.

---

## Table of Contents

1. [How It Works](#how-it-works)
2. [Project Structure](#project-structure)
3. [Protocol](#protocol)
4. [Server — Deep Dive](#server--deep-dive)
5. [Client — Deep Dive](#client--deep-dive)
6. [Installation & Quick Start](#installation--quick-start)
7. [Request Flow (Step by Step)](#request-flow-step-by-step)
8. [CLI Flags](#cli-flags)
9. [Production Deployment](#production-deployment)

---

## How It Works

You run a web server on `localhost:3000`. No public IP, behind NAT. VLGR exposes it to the internet through a relay server.

```
┌──────────────────┐         WebSocket          ┌─────────────────────┐
│   VLGR Client    │◄══════════════════════════►│   VLGR Server       │
│   (your laptop)  │     control channel        │   (VPS, public IP)  │
│                  │                            │                     │
│  localhost:3000  │                            │  :4443  WebSocket   │
└──────────────────┘                            │  :8080  HTTP        │
                                                └──────────┬──────────┘
                                                           │
                                                    HTTPS request
                                                           │
                                                 ┌─────────┴─────────┐
                                                 │  External user    │
                                                 │  (browser, curl)  │
                                                 └───────────────────┘
```

The client opens a persistent WebSocket to the server. The server assigns a unique public URL for each port (e.g., `a3f8b2c1d4e5f6a7.tunnel.domain.com`). A single client connection can expose multiple local ports — each gets its own subdomain and TunnelID. When an external user hits a public URL, the server serializes the HTTP request into a binary frame (tagged with the corresponding TunnelID), sends it to the client over WebSocket. The client looks up the local port by TunnelID, forwards the request to the correct `localhost:<port>`, serializes the response, and sends it back. The server returns the response to the external user — who sees a normal HTTP exchange.

WebSocket upgrades are fully proxied via TCP hijacking and `MsgStreamData`/`MsgStreamClose` frames, enabling bidirectional real-time communication.

Large HTTP bodies are streamed too: bodies up to 4 MB travel inline in a single frame, anything bigger is relayed in 32 KB `MsgStreamData` chunks — so uploads/downloads have no size limit, responses are flushed as they arrive (SSE works), and memory per transfer stays bounded.

Forwards can be managed at runtime: `--add`/`--del` talk to a running instance over a local control socket (with a console menu when several instances run in parallel), and `--tray` puts a per-instance icon in the system tray (Windows/Linux) for viewing, adding and removing forwards.

---

## Project Structure

```
vlgr/
├── go.mod
├── .gitignore
├── README.md
├── GETTING_STARTED.md
│
├── cmd/
│   ├── server/main.go              # Server entry point
│   └── client/
│       ├── main.go                 # Client entry point
│       └── main_test.go            # 1 test with 18 address validation cases
│
├── internal/
│   ├── protocol/
│   │   ├── protocol.go             # Binary protocol: framing, HTTP serialization, streamed bodies
│   │   └── protocol_test.go        # 49 unit tests: frames, HTTP serde, register/auth codecs, WS detection
│   ├── version/
│   │   ├── version.go              # Version string for the handshake mismatch warning
│   │   └── version_test.go
│   ├── server/
│   │   ├── registry.go             # Tunnel registry (subdomain → Tunnel) + Snapshot for the admin API
│   │   ├── registry_test.go        # register, get, unregister, concurrency
│   │   ├── handler.go              # Client WebSocket handler (HTTP + raw TCP + TLS-passthrough tunnels)
│   │   ├── handler_test.go         # auth, registration, unregister, frame dispatch, stream relay
│   │   ├── tcp.go                  # Public TCP port allocator for raw TCP / SSH tunnels
│   │   ├── tcp_test.go             # Port allocator: auto/preferred allocation, release, exhaustion
│   │   ├── proxy.go                # Reverse proxy for incoming HTTP, streamed transfers
│   │   ├── proxy_test.go           # subdomain extraction, response writing, header injection
│   │   ├── protect.go              # Endpoint protection: basic auth + IP allowlist (proxy guard)
│   │   ├── protect_test.go         # basic auth, allowlist, X-Forwarded-For hop resolution
│   │   ├── admin.go                # Admin REST API + status dashboard (read-only, Bearer-guarded)
│   │   ├── admin_test.go           # tunnel listing, token guard
│   │   ├── tlspass.go              # TLS-passthrough listener multiplexing tunnels by SNI
│   │   ├── sni.go                  # ClientHello server_name parser (read-only, no TLS termination)
│   │   └── sni_test.go             # SNI extraction from a real ClientHello
│   ├── sshtun/
│   │   ├── server.go               # Agentless SSH tunnels via ssh -R remote forwarding
│   │   └── server_test.go          # end-to-end remote forward + auth failure
│   ├── selfupdate/
│   │   ├── selfupdate.go           # Signed self-update: poll releases, verify ed25519 sig, swap, relaunch
│   │   └── selfupdate_test.go      # version compare, asset name, signature verification
│   ├── client/
│   │   ├── tunnel.go               # Client logic: connect, register, proxy, streamed bodies, TCP relay, control socket
│   │   ├── tunnel_test.go          # routing, WS relay, body streams, error paths
│   │   ├── tray.go                 # System tray icon and menu (Windows/Linux, fyne.io/systray)
│   │   ├── tray_stub.go            # No-op Tray for platforms without tray support (keeps darwin builds cgo-free)
│   │   ├── tray_input_windows.go   # Native input box + browser open (Windows)
│   │   ├── tray_input_linux.go     # Input dialog via zenity/kdialog + xdg-open (Linux)
│   │   ├── dashboard.go            # Traffic inspector: ring buffer + local web UI + replay
│   │   ├── dashboard_html.go       # Self-contained inspector single-page UI
│   │   └── dashboard_test.go       # ring buffer, field capture, listing, body cap
│   └── integration/
│       └── integration_test.go     # E2E: live server/client/backend, streamed bodies, raw TCP, inspector
│
├── scripts/
│   ├── build.ps1                   # Cross-compile for all platforms (PowerShell); optional release signing
│   ├── build.sh                    # Cross-compile for all platforms (Bash)
│   ├── deploy-server.sh            # One-command auto-deploy for VPS (multi-distro)
│   └── sign/main.go                # ed25519 keygen/sign helper for signed self-update releases
│
└── docs/
    └── Caddyfile                   # Caddy config reference
```

---

## Protocol

### Binary frame

Every message is wrapped in a fixed binary frame:

```
┌──────────┬──────────┬────────────┬──────────┬──────────────┐
│  Type    │ TunnelID │  RequestID │  Length  │   Payload    │
│  1 byte  │  8 bytes │   8 bytes  │  4 bytes │   N bytes    │
│  uint8   │  uint64  │   uint64   │  uint32  │              │
└──────────┴──────────┴────────────┴──────────┴──────────────┘
```

**Header is always 21 bytes** (1 + 8 + 8 + 4). Payload follows.

| Field | Size | Purpose |
|---|---|---|
| `Type` | 1 B | Message type |
| `TunnelID` | 8 B | Tunnel identifier for multiplexing |
| `RequestID` | 8 B | Unique request ID for matching request↔response |
| `PayloadLen` | 4 B | Payload length in bytes |
| `Payload` | N B | Serialized data (type-dependent) |

### Message types

| Code | Constant | Direction | Purpose |
|---|---|---|---|
| `0x01` | `MsgAuth` | Client → Server | Authentication token |
| `0x02` | `MsgAuthOK` | Server → Client | Auth success |
| `0x03` | `MsgAuthErr` | Server → Client | Auth error |
| `0x04` | `MsgRegister` | Client → Server | Register tunnel (port + optional subdomain). Multiple registrations per connection allowed — each creates an independent tunnel |
| `0x05` | `MsgRegisterOK` | Server → Client | Tunnel created (public URL + tunnelID) |
| `0x06` | `MsgRegisterErr` | Server → Client | Registration error |
| `0x07` | `MsgHTTPReq` | Server → Client | Proxied HTTP request (routed by TunnelID) |
| `0x08` | `MsgHTTPRes` | Client → Server | HTTP response from local server |
| `0x09` | `MsgCloseTunnel` | Both | Close tunnel request |
| `0x0A` | `MsgError` | Both | Error message |
| `0x0B` | `MsgStreamData` | Both | Stream data chunk (WebSocket relay or streamed HTTP body) |
| `0x0C` | `MsgStreamClose` | Both | Stream close / body EOF |
| `0x0D` | `MsgUnregister` | Client → Server | Remove one tunnel (identified by frame `TunnelID`) from a live connection |
| `0x0E` | `MsgUnregisterOK` | Server → Client | Tunnel removed |
| `0x0F` | `MsgUnregisterErr` | Server → Client | Unregister error (unknown tunnel) |
| `0x10` | `MsgRegisterTCP` | Client → Server | Register a raw TCP tunnel (local port + requested public port) |
| `0x11` | `MsgTCPOpen` | Both | Server → Client: new public TCP connection to relay. Client → Server: local connection is up (readiness ack) |
| `0x12` | `MsgRegisterTLS` | Client → Server | Register a TLS-passthrough tunnel (local port + optional subdomain), routed publicly by SNI |

### Auth payload (`MsgAuth` / `MsgAuthOK`)

```
MsgAuth:   [tokenLen:2][token][versionLen:2][version]
MsgAuthOK: [versionLen:2][serverVersion]
```

The version field enables a mismatch warning during the handshake. A missing
version field is tolerated for older clients.

### Register payload (`MsgRegister` / `MsgRegisterOK`)

```
MsgRegister:    [port:2][subdomainLen:1][subdomain]     (subdomain optional)
MsgRegisterTCP: [localPort:2][remotePort:2]             (remotePort 0 = server picks)
MsgRegisterOK:  [urlLen:1][publicURL][tunnelID:8]        (publicURL is "host:port" for TCP)
```

### HTTP request payload (`MsgHTTPReq`)

```
[methodLen:2][method][pathLen:2][path][headerCount:4]([keyLen:2][key][valueLen:2][value])*[bodyLen:4][body]
```

### HTTP response payload (`MsgHTTPRes`)

```
[statusCode:2][headerCount:4]([keyLen:2][key][valueLen:2][value])*[bodyLen:4][body]
```

### Streamed bodies

Bodies ≤ 4 MB (`InlineBodyLimit`) are inlined in `bodyLen`/`body` as shown
above. Larger bodies set `bodyLen = 0xFFFFFFFF` (`StreamedBodyLen`) with no
inline body; the body then follows as `MsgStreamData` frames with the same
`RequestID`, terminated by `MsgStreamClose`. Request bodies stream
server → client, response bodies client → server; both may stream at once.

### Raw TCP tunnels

Besides HTTP, the client can expose a local port as a raw TCP tunnel
(`MsgRegisterTCP`). The server allocates a public TCP port from a configured
range (`--tcp-ports`) and opens a listener on it. For each incoming
connection it sends `MsgTCPOpen` (carrying the `TunnelID` and a fresh
`RequestID`); the client dials the local port, then sends `MsgTCPOpen` back
as a readiness ack so the server doesn't forward bytes before the relay is
wired up. Data then flows both ways as `MsgStreamData` frames terminated by
`MsgStreamClose` — the same relay machinery as WebSocket upgrades. Raw TCP
bypasses Caddy, so its ports must be reachable directly on the server.

### TLS passthrough (SNI)

The client can expose a local TLS service without the relay terminating TLS
(`MsgRegisterTLS`, same payload as `MsgRegister`). The server runs a single
public TLS listener (`--tls-passthrough`) that peeks the ClientHello, reads the
`server_name` (SNI), maps it to a registered TLS tunnel by subdomain, and
relays the raw bytes — including the peeked ClientHello — to the client's local
port over the same `MsgTCPOpen`/`MsgStreamData` machinery as raw TCP. Many TLS
hostnames therefore share one public port, and certificates stay entirely on
the local service. Like raw TCP, this port bypasses Caddy and must be reachable
directly.

### Agentless SSH tunnels

The relay can also expose local services to users who have no vlgr client at
all — just a stock `ssh`. With `--ssh :2222` (and `--ssh-ports` for the public
range) the server runs an SSH endpoint that accepts standard remote forwarding:

```bash
ssh -N -R 0:localhost:3000 tunnel.domain.com -p 2222
```

The server allocates a public TCP port, prints it back (per the `tcpip-forward`
reply), and relays every connection to the user's local port over the SSH
`forwarded-tcpip` channel. When the server has `--token` set it is required as
the SSH password; without a token the endpoint accepts anyone and logs a
warning at startup, so set a token whenever the port range is publicly
reachable. `--ssh-hostkey <path>` persists the host key across restarts.

### Keep-alive

WebSocket control frames (Ping/Pong) are used. Server pings every 30s. Client responds automatically via gorilla/websocket. If no Pong within 60s, the connection times out.

---

## Server — Deep Dive

### 1. Registry (`internal/server/registry.go`)

Thread-safe map `subdomain → *Tunnel`. The central routing table: when an external HTTP request arrives, the proxy extracts the subdomain from the `Host` header and looks it up here.

```go
type Tunnel struct {
    ID        uint64
    Subdomain string
    Handler   *ClientHandler
}

type Registry struct {
    mu      sync.RWMutex
    tunnels map[string]*Tunnel
}
```

Tunnel IDs are cryptographically random (`crypto/rand`), not sequential. Subdomains use 8 random bytes (16 hex chars) for 2^64 namespace.

### 2. ClientHandler (`internal/server/handler.go`)

Handles one client WebSocket connection. Each connected client gets its own instance.

**Lifecycle:**

1. `NewClientHandler` — stores connection and registry reference.
2. `Run()` — main read loop:
   - Sets read deadlines, registers Pong handler.
   - Starts `pingLoop` goroutine (sends WebSocket Ping every 30s).
   - Reads binary messages, decodes frames, dispatches by type.
3. `handleAuth` — validates token via constant-time comparison (`crypto/subtle`). An invalid token costs a flat 1 s delay, gets `MsgAuthErr`, and the connection is closed. Connections that don't authenticate within 10 s are dropped.
4. `handleRegister` — decodes port/subdomain (`protocol.DecodeRegister`), registers tunnel, replies with public URL.
5. `handleHTTPRes` — receives response from client, routes to awaiting forward call via channel.

**Request multiplexing:**

Multiple HTTP requests flow over a single WebSocket connection simultaneously:

```go
type pendingReq struct {
    response   chan protocol.HTTPResponse
    streamData chan []byte
    done       chan struct{}
}
// pending map[uint64]*pendingReq  — RequestID → awaiting request
```

`startForward` generates a unique `requestID`, inserts a `pendingReq` into the map and writes `MsgHTTPReq`; `awaitResponse` blocks on the response channel (30 s timeout, 5 min for streamed uploads). When `MsgHTTPRes` arrives, `handleHTTPRes` finds the entry and delivers the response. The `streamData` channel carries WebSocket relay data or a streamed response body.

### 3. ReverseProxy (`internal/server/proxy.go`)

HTTP handler for external traffic. Implements `http.Handler`.

**Algorithm:**

1. Extract subdomain from `Host` header using `extractSubdomain(host, baseDomain)`.
2. Look up tunnel in registry → 404 if missing.
3. Apply endpoint protection (`Protector.Allow`): IP allowlist and/or Basic auth → 403/401 if blocked (no-op when neither is configured).
4. Read request body up to 4 MB; a larger body is pumped concurrently as `MsgStreamData` chunks (no size limit).
5. Serialize into `protocol.HTTPRequest`, adding `X-Forwarded-Host/Proto/For`.
6. Forward through the tunnel — blocks until the response header arrives.
7. Write the response: inline body directly, streamed body chunk-by-chunk with a `Flush` after each chunk (SSE-friendly).

**Subdomain extraction:**

```
host = "abc123.tunnel.domain.com", baseDomain = "tunnel.domain.com"
→ suffix = ".tunnel.domain.com"
→ prefix = "abc123"
→ return "abc123"
```

---

## Client — Deep Dive

### 1. Tunnel (`internal/client/tunnel.go`)

**Connect():**
1. Dial WebSocket (`ws://` or `wss://` depending on `-tls` flag).
2. Send `MsgAuth` with token and client version; warn if the server version differs.
3. For each port in the comma-separated `--ports` list, send `MsgRegister` with port and optional subdomain (matched by position in `--subdomain`).
4. Parse public URLs and tunnelIDs from responses. Each forward (`tunnelID`, local port, public URL) is stored in an ordered `forwards` list used for routing, `LIST`/`DEL` control commands and the tray menu.

**Run():**
- Reads messages in a loop.
- `MsgHTTPReq` → spawns `handleHTTPReq` goroutine (concurrent handling), routes to correct `localhost:<port>` by `frame.TunnelID`. A streamed request body is fed to the local app through an `io.Reader` backed by incoming `MsgStreamData` chunks; a response body over 4 MB is pumped back the same way.
- `MsgStreamData`/`MsgStreamClose` → dispatched to the WebSocket relay, a raw TCP relay, a streamed request body, or a streamed-response cancel signal.
- `MsgTCPOpen` → dials the tunnel's local port for a new public TCP connection and relays bytes both ways (raw TCP tunnels).
- `MsgCloseTunnel` → exits loop.
- `MsgRegisterOK`/`MsgRegisterErr` and `MsgUnregisterOK`/`MsgUnregisterErr` arriving mid-session resolve a pending `AddPort`/`RemovePort` call.

When `--inspect` is set, each forwarded HTTP exchange is also recorded to the traffic inspector dashboard (see [CLI Flags](#cli-flags)).

**Control socket:**
Each running instance listens on `127.0.0.1:<random port>` and writes the address to `%TEMP%/vlgr-client-<pid>.ctl` (removed on shutdown). Line-based text commands:

| Command | Response | Purpose |
|---|---|---|
| `ADD <port> [subdomain]` | `OK <url>` / `ERR <msg>` | Register a new forward on the live connection |
| `DEL <port>` | `OK removed port <port>` / `ERR <msg>` | Unregister a forward (sends `MsgUnregister`, frees the subdomain) |
| `LIST` | `OK <n>` + `n` lines `<port> <url>` | Enumerate active forwards |

**Limits:** 100 concurrent local requests, 200 concurrent long-lived streams (WebSocket relays + streamed transfers), 20 tunnels per client, 1000 client connections per server.

**Multi-tunnel routing:**
When the client registers multiple ports, the server assigns a unique `TunnelID` to each. Incoming `MsgHTTPReq` frames carry the target `TunnelID`. The client looks up the corresponding local port in the `forwards` list and forwards the request to the correct `localhost:<port>`.

### 2. Client entry point (`cmd/client/main.go`)

Reconnection loop with exponential backoff: 1s → 2s → 4s → ... → 30s max. Resets on successful connection. Handles SIGINT/SIGTERM for graceful shutdown (the server does too).

**`--add` / `--del` modes:** instead of starting a tunnel, the process discovers running instances by scanning `vlgr-client-*.ctl` files in the temp dir (stale files from dead instances are removed), then sends `ADD`/`DEL` over the control socket. With one instance the command applies directly; with several, a console menu lists each instance (pid + its current forwards from `LIST`) to choose the target — Ctrl+C cancels. These flags must be used alone: combining `--add`/`--del` with each other or any other flag is an error.

**`--tray` mode:** the systray loop owns the main goroutine while the reconnect loop runs beside it. Each instance gets its own tray icon (a base64-embedded diagonal arrow — PNG on Linux, ICO on Windows). The menu shows the active forwards (each with *Open in browser* and *Remove forward*), an *Add forward…* item that opens a native input dialog (`<port> [subdomain]`), and *Quit*. The menu refreshes automatically whenever the set of forwards changes, including changes made via `--add`/`--del` from another terminal. Supported on Windows and Linux (zenity or kdialog needed for the add dialog); on macOS builds the flag exits with an error to keep cross-compilation cgo-free.

---

## Installation & Quick Start

### Requirements

- Go 1.26+
- VPS with public IP (for server) — or localhost for testing

### Setup

```bash
cd vlgr
go mod tidy
```

### Local test (all on one machine)

```bash
# Terminal 1 — server
go run ./cmd/server

# Terminal 2 — your local web server
python -m http.server 3000

# Terminal 3 — VLGR client
go run ./cmd/client -p 3000

# Terminal 4 — test
curl -H "Host: <hex-from-output>.localhost:8080" http://localhost:8080/
```

### Building binaries

```bash
# Pre-built binaries are available on GitHub Releases:
# https://github.com/varmtlys/vlgr/releases

# Or build locally for your platform:
# Linux (amd64 / arm64):
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/vlgr-server-linux-amd64 ./cmd/server
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/vlgr-client-linux-amd64 ./cmd/client

# Windows (amd64):
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/vlgr-server-windows-amd64.exe ./cmd/server
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/vlgr-client-windows-amd64.exe ./cmd/client

# macOS (amd64 / arm64):
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/vlgr-server-darwin-amd64 ./cmd/server
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o build/vlgr-client-darwin-arm64 ./cmd/client
```

Output in `build/` as flat files: `vlgr-server-<os>-<arch>`.

---

## Request Flow (Step by Step)

External user requests `GET https://abc123.tunnel.domain.com/api/status`:

1. **External request hits server** — HTTPS → Caddy (on VPS) → localhost:8080
2. **Proxy extracts subdomain** — `extractSubdomain("abc123.tunnel.domain.com:443", "tunnel.domain.com")` → `"abc123"`
3. **Proxy looks up tunnel** — `registry.Get("abc123")` → found!
4. **Proxy serializes HTTP request** — `EncodeHTTPRequest(...)` → binary payload
5. **Proxy sends frame to client** — `MsgHTTPReq` with `RequestID: 42` over WebSocket
6. **Client receives frame** — `Run()` reads message, spawns handler goroutine
7. **Client deserializes HTTP request** — `DecodeHTTPRequest(payload)`
8. **Client calls localhost** — `GET http://localhost:3000/api/status`
9. **Client gets response** — `{"status": "ok", "uptime": 12345}`
10. **Client serializes response** — `EncodeHTTPResponse(...)`
11. **Client sends response** — `MsgHTTPRes` with same `RequestID: 42`
12. **Server receives response** — `handleHTTPRes` routes to pending request channel
13. **Proxy wakes up** — `awaitResponse` returns `HTTPResponse`
14. **Proxy writes to external user** — HTTP 200 + JSON body
15. **External user gets response** — `{"status": "ok", "uptime": 12345}`

---

## CLI Flags

### Server (`cmd/server`)

| Flag | Short | Default | Description |
|---|---|---|---|
| `--addr` | `-a` | `:4443` | WebSocket listen address for tunnel clients |
| `--http` | `-w` | `:8080` | HTTP listen address for public traffic |
| `--domain` | `-d` | `localhost:8080` | Base domain for tunnel URLs (e.g. `tunnel.domain.com`) |
| `--token` | `-t` | `""` | Auth token for clients (empty = no auth, or set `VLGR_TOKEN` env) |
| `--verbose` | `-V` | `info` | Log level: `info` (default) or `debug` |
| `--tcp-ports` | | | Public TCP port range for raw TCP tunnels, e.g. `20000-20100` (empty = disabled) |
| `--basic-auth` | | | Protect all tunnels with HTTP Basic auth, `user:pass` (empty = off) |
| `--allow-ips` | | | Comma-separated IP/CIDR allowlist for public traffic (empty = allow all) |
| `--trusted-proxy-hops` | | `0` | Trusted reverse proxies in front (set `1` behind Caddy); `0` uses the direct peer IP |
| `--admin` | | | REST API + dashboard listen address, e.g. `127.0.0.1:4041` (empty = disabled) |
| `--tls-passthrough` | | | Public TLS-passthrough (SNI) listen address, e.g. `:443` (empty = disabled) |
| `--ssh` | | | Agentless SSH tunnel listen address, e.g. `:2222` (empty = disabled) |
| `--ssh-ports` | | | Public TCP port range for SSH remote forwards, e.g. `20200-20300` |
| `--ssh-hostkey` | | | Path to persist the SSH host key (empty = ephemeral per start) |
| `--autoupdate` | | `false` | Poll GitHub releases and self-update in the background |
| `--version` | `-v` | | Show version and exit |
| `--help` | `-h` | | Show help with usage examples |

**Self-update.** With `--autoupdate` the server polls GitHub releases hourly
and, on a newer tag, downloads the matching binary, **verifies its detached
ed25519 signature** (compiled-in public key + per-release `<asset>.sig`), swaps
it on disk and relaunches. An unsigned or mismatched binary is refused, and a
build without a compiled-in key never self-updates (fail-safe). Before
relaunching it closes every listener so the replacement can bind the same
ports; hijacked client tunnels drop and reconnect on their own. Development
builds are skipped. Disabled by default.

**Admin API + dashboard.** `--admin <addr>` starts a read-only REST API and a
status page. `GET /api/status` returns version, uptime and tunnel count;
`GET /api/tunnels` lists the active HTTP tunnels; `/` serves a small live
dashboard. Loopback callers reaching it by a loopback `Host` are trusted, so
the local dashboard works without a token; any remote caller must present the
`--token` value as a `Bearer` token, and remote access is refused outright when
no token is set. The `Host` check also blocks DNS-rebinding. Bind it to
loopback and reach it over an SSH tunnel or a protected Caddy vhost.

**Endpoint protection.** `--basic-auth` and `--allow-ips` guard every public
request before it is forwarded into a tunnel. The IP allowlist is checked
against the originating client IP, resolved with `--trusted-proxy-hops`: the
default `0` uses the direct connection (`RemoteAddr`) and ignores
`X-Forwarded-For` entirely, so a client cannot spoof its source. Behind one
reverse proxy (Caddy) set `--trusted-proxy-hops 1` — the server then reads the
address that proxy appended (the rightmost `X-Forwarded-For` entry), so
attacker-supplied leftmost entries are ignored. Both guards are enforced
server-wide. TLS client certificates (mTLS) and OAuth are intentionally left to
the fronting proxy (Caddy), which already terminates TLS and can enforce them
per host.

### Client (`cmd/client`)

| Flag | Short | Default | Description |
|---|---|---|---|
| `--server` | `-s` | `localhost:4443` | VLGR server address |
| `--ports` | `-p` | | Local port(s) to expose over HTTP, comma-separated (e.g. `3000` or `8080,3000`) |
| `--tcp` | | | Local port(s) to expose as raw TCP, comma-separated: `<local[:remote]>` (e.g. `22` or `22:2222,5432`) |
| `--tls-tunnel` | | | Local TLS port(s) to expose via SNI passthrough: `<local[:subdomain]>` (e.g. `8443` or `8443:mysub`) |
| `--token` | `-t` | `""` | Authentication token (required when server has `--token` set) |
| `--subdomain` | `-u` | auto | Request custom subdomain(s), comma-separated — order matches `--ports` |
| `--tls` | | `false` | Use WSS (TLS) — required when connecting via Caddy/HTTPS |
| `--verbose` | `-V` | `info` | Log level: `info` (default) or `debug` |
| `--tray` | | `false` | Show a system tray icon for this instance (Windows/Linux) with a menu to view, add and remove forwards |
| `--inspect` | | | Traffic inspector dashboard address, e.g. `127.0.0.1:4040` (empty = disabled) |
| `--inspect-limit` | | `1000` | Max rows kept in the inspector (older dropped, new on top); capped at `100000` |
| `--autoupdate` | | `false` | Poll GitHub releases and self-update in the background |
| `--add` | | | Add a port with subdomain to a running instance: `"<port> <subdomain>"` — must be used alone |
| `--del` | | | Remove a port forward (and its subdomain) from a running instance: `<port>` — must be used alone |
| `--version` | `-v` | | Show version and exit |
| `--help` | `-h` | | Show help with usage examples |

At least one of `--ports` or `--tcp` is required.

`--add` and `--del` talk to an already running vlgr-client over its local
control socket. When several instances run in parallel, a console menu lists
each instance with its current forwards to pick the target (Ctrl+C cancels);
with a single instance the command applies immediately.

### Raw TCP tunnels

Enable a public port range on the server, then expose any local TCP service
(SSH, Postgres, a game server) through it:

```bash
# Server: allow public TCP ports 20000-20100
./vlgr-server --domain tunnel.domain.com --tcp-ports 20000-20100

# Client: expose local SSH (22) on an auto-assigned public port
./vlgr-client -s tunnel.domain.com:443 --tls --tcp 22

# Client: pin the public port (22 -> tunnel.domain.com:2222)
./vlgr-client -s tunnel.domain.com:443 --tls --tcp 22:2222
```

Raw TCP bypasses Caddy, so the chosen port range must be open on the server's
firewall and reachable directly (not behind the HTTPS reverse proxy).

### Traffic inspector

`--inspect <addr>` starts a local web dashboard (à la ngrok's `localhost:4040`)
that records the HTTP requests flowing through the tunnel — method, path,
status, timing, headers and bodies — with live updates and one-click replay
to the local app:

```bash
./vlgr-client -s tunnel.domain.com:443 -p 3000 --tls --inspect 127.0.0.1:4040
# open http://127.0.0.1:4040
```

`--inspect-limit` sets how many requests are retained (default `1000`, capped
at `100000`); older rows drop off as new ones arrive on top. The dashboard has
**Export HAR** and **Export text** buttons: HAR (`.har`) is the standard HTTP
Archive format importable into browser DevTools, Charles, Fiddler, Postman,
etc.; the text dump is a plain human-readable request/response log. Both are
also available directly at `/api/export.har` and `/api/export.txt`.

The dashboard is loopback-only: it rejects non-loopback callers, checks the
`Host` header to block DNS-rebinding, and refuses cross-site replay requests. It
is never exposed through the tunnel.

### Self-update

With `--autoupdate` the client polls the GitHub releases API hourly and, when a
newer tag ships, downloads the matching binary for its OS/arch, **verifies its
detached ed25519 signature**, then swaps it on disk and relaunches with the
same arguments. The public key is compiled in at build time
(`-ldflags "-X vlgr/internal/selfupdate.SigningPublicKey=<hex>"`) and each
release ships a `<asset>.sig` file; if the signature is missing or does not
match, the update is refused and the running binary is left untouched. A build
with no key compiled in never self-updates (fail-safe). Development builds
(`version = dev`) are skipped. The relaunch drops the tunnel briefly; the new
process reconnects on its own, so an open forward recovers within the usual
reconnect backoff. Disabled by default.

---

## Production Deployment

See [GETTING_STARTED.md](GETTING_STARTED.md) for the full production guide covering:

- Cloudflare DNS configuration
- Caddy setup with wildcard TLS (Let's Encrypt DNS-01)
- Cloudflare API token creation
- Caddy rebuild with Cloudflare DNS plugin
- VLGR server systemd unit
- Client connection via WSS

### Auto-deploy (one command)

The [`scripts/deploy-server.sh`](scripts/deploy-server.sh) script automates the entire VPS setup — downloads a pre-built binary (or builds from source as fallback), creates a systemd service, and optionally installs Caddy with Cloudflare DNS plugin. Supports Debian/Ubuntu, RHEL/Fedora, Arch, Alpine, openSUSE, and Void.

```bash
# From the web (as root) — downloads latest pre-built binary:
curl -sL https://github.com/varmtlys/vlgr/raw/main/scripts/deploy-server.sh | sudo bash -s -- \
  -d tunnel.domain.com -t my-token --release latest

# Or locally with Caddy + Cloudflare:
sudo ./scripts/deploy-server.sh -d tunnel.domain.com -t my-token \
  --release latest --caddy --cf-token <CF_API_TOKEN>
```

### Quick production commands

```bash
# Server (VPS)
./vlgr-server --addr 127.0.0.1:4443 --http 127.0.0.1:8080 --domain tunnel.domain.com

# Client (your machine) — single tunnel
./vlgr-client -s tunnel.domain.com:443 -p 3000 --tls

# Client — multiple tunnels
./vlgr-client -s tunnel.domain.com:443 -p "8080,3000,5000" -u "api,web,admin" --tls

# Add / remove ports on a running client
./vlgr-client --add "5000 mysub"
./vlgr-client --del 5000

# Client with a system tray icon (Windows/Linux)
./vlgr-client -s tunnel.domain.com:443 -p 3000 --tls --tray

# Raw TCP tunnel (server needs --tcp-ports) + local traffic inspector
./vlgr-client -s tunnel.domain.com:443 --tls --tcp 22:2222
./vlgr-client -s tunnel.domain.com:443 -p 3000 --tls --inspect 127.0.0.1:4040
```

---

## Arduino / IoT

VLGR's protocol is simple enough to implement directly on microcontrollers. The ESP32 below connects to WiFi behind NAT, runs the full VLGR tunnel client, and serves HTTP responses — no separate computer needed.

### Architecture

```
┌───────────────────────────────────┐   WebSocket (WSS)   ┌────────────────────┐
│  ESP32 (behind NAT)               │◄═══════════════════►│  VLGR Server       │
│                                   │                     │  (VPS, public IP)  │
│  • WiFi connection                │                     │                    │
│  • VLGR binary protocol over WS   │                     │  :4443  WebSocket  │
│  • Handles HTTP requests inline   │                     │  :8080  HTTP       │
└───────────────────────────────────┘                     └──────────┬─────────┘
                                                                     │
                                                               HTTPS request
                                                                     │
                                                          ┌──────────┴─────────┐
                                                          │  External user     │
                                                          │  (browser, curl)   │
                                                          └────────────────────┘
```

### Complete ESP32 sketch (minimal)

The sketch implements the full VLGR client protocol directly on ESP32. It connects to WiFi, authenticates with the VLGR server, registers a tunnel, and responds to HTTP requests with device telemetry.

**Required library:** [WebSockets by Markus Sattler](https://github.com/Links2004/arduinoWebSockets)

```cpp
#include <WiFi.h>
#include <WebSocketsClient.h>

const char* ssid = "YOUR_WIFI", *password = "YOUR_PASS";
const char* serverHost = "tunnel.domain.com";
const uint16_t serverPort = 443;
const bool useTLS = true;
const char* authToken = "vlgr-token";

WebSocketsClient ws;
uint64_t tunnelID = 0;

void w16(uint8_t* p, uint16_t v) { p[0]=v>>8; p[1]=v; }
void w32(uint8_t* p, uint32_t v) { p[0]=v>>24; p[1]=v>>16; p[2]=v>>8; p[3]=v; }
void w64(uint8_t* p, uint64_t v) { for(int i=7;i>=0;i--){p[i]=v&0xFF;v>>=8;} }
uint64_t r64(const uint8_t* p) { uint64_t v=0; for(int i=0;i<8;i++) v=(v<<8)|p[i]; return v; }
uint32_t r32(const uint8_t* p) { return ((uint32_t)p[0]<<24)|((uint32_t)p[1]<<16)|((uint32_t)p[2]<<8)|p[3]; }
uint16_t r16(const uint8_t* p) { return (p[0]<<8)|p[1]; }

void sendFrame(uint8_t type, uint64_t tunID, uint64_t reqID,
               const uint8_t* payload, uint32_t len) {
  uint32_t total = 21 + len;
  uint8_t* msg = new uint8_t[total];
  msg[0] = type;
  w64(&msg[1], tunID); w64(&msg[9], reqID); w32(&msg[17], len);
  if (len) memcpy(&msg[21], payload, len);
  ws.sendBIN(msg, total);
  delete[] msg;
}

void webSocketEvent(WStype_t t, uint8_t* d, size_t l) {
  if (t == WStype_CONNECTED) {
    Serial.println("> auth");
    // MsgAuth payload: [tokenLen:2][token][versionLen:2][version]
    uint16_t tl = strlen(authToken);
    uint8_t ap[132];
    w16(&ap[0], tl); memcpy(&ap[2], authToken, tl); w16(&ap[2+tl], 0);
    sendFrame(0x01, 0, 0, ap, 4 + tl);
    return;
  }
  if (t != WStype_BIN || l < 21) return;

  uint8_t mt = d[0];
  uint32_t pl = r32(&d[17]);
  uint8_t* fp = (l >= 21+pl) ? &d[21] : nullptr;

  if (mt == 0x02) {
    Serial.println("> register");
    uint8_t rp[2] = {0,80};
    sendFrame(0x04, 0, 0, rp, 2);
  } else if (mt == 0x05 && fp && pl >= 9) {
    tunnelID = r64(&fp[1+fp[0]]);
    char url[128]; memcpy(url, &fp[1], fp[0]); url[fp[0]]=0;
    Serial.printf("> ready: %s\n", url);
  } else if (mt == 0x07 && fp) {
    uint16_t mlen = r16(&fp[0]);
    uint16_t plen = r16(&fp[2+mlen]);
    uint16_t cp = plen < 63 ? plen : 63; char path[64]; memcpy(path, &fp[4+mlen], cp); path[cp]=0;
    char body[256]; snprintf(body, sizeof(body),
      "{\"d\":\"ESP32\",\"p\":\"%s\",\"u\":%lu}", path, millis()/1000);
    uint16_t blen = strlen(body);
    uint32_t rlen = 42 + blen;
    uint8_t* rp = new uint8_t[rlen];
    uint32_t o=0;
    w16(&rp[o],200); o+=2;
    w32(&rp[o],1); o+=4;
    w16(&rp[o],12); memcpy(&rp[o+2],"Content-Type",12); o+=14;
    w16(&rp[o],16); memcpy(&rp[o+2],"application/json",16); o+=18;
    w32(&rp[o],blen); memcpy(&rp[o+4],body,blen);
    sendFrame(0x08, tunnelID, r64(&d[9]), rp, rlen);
    delete[] rp;
  }
}

void setup() {
  Serial.begin(115200);
  WiFi.begin(ssid, password);
  while (WiFi.status() != WL_CONNECTED) { delay(500); Serial.print("."); }
  Serial.printf("\nIP: %s\n", WiFi.localIP().toString().c_str());
  if (useTLS) ws.beginSSL(serverHost, serverPort, "/_tunnel");
  else ws.begin(serverHost, serverPort, "/_tunnel");
  ws.onEvent(webSocketEvent);
}

void loop() { ws.loop(); }
```

### How to use

1. Install the [WebSockets library](https://github.com/Links2004/arduinoWebSockets) in Arduino IDE.
2. Set `ssid`, `password`, `serverHost`, and `authToken`.
3. Flash to ESP32. Open Serial Monitor — you'll see `ready: <url>`.
4. Access device from anywhere at `https://<url>`.
