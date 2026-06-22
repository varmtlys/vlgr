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
│                  │                            │  :8080  HTTP        │
└──────────────────┘                            └──────────┬──────────┘
                                                           │
                                                    HTTPS request
                                                           │
                                                 ┌─────────┴─────────┐
                                                 │  External user    │
                                                 │  (browser, curl)  │
                                                 └───────────────────┘
```

The client opens a persistent WebSocket to the server. The server assigns a unique public URL (e.g., `a3f8b2c1.tunnel.domain.com`). When an external user hits that URL, the server serializes the HTTP request into a binary frame, sends it to the client over WebSocket. The client forwards it to `localhost:3000`, serializes the response, and sends it back. The server returns the response to the external user — who sees a normal HTTP exchange.

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
│   └── client/main.go              # Client entry point
│
├── internal/
│   ├── protocol/protocol.go        # Binary protocol: framing, HTTP serialization
│   ├── server/
│   │   ├── registry.go             # Tunnel registry (subdomain → Tunnel)
│   │   ├── handler.go              # Client WebSocket handler
│   │   └── proxy.go                # Reverse proxy for incoming HTTP
│   └── client/
│       └── tunnel.go               # Client logic: connect, register, proxy
│
├── scripts/
│   ├── build.ps1                   # Build for Windows + Linux (PowerShell)
│   └── build.sh                    # Build for Linux + Windows (Bash)
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
| `0x04` | `MsgRegister` | Client → Server | Register tunnel (port + optional subdomain) |
| `0x05` | `MsgRegisterOK` | Server → Client | Tunnel created (public URL + tunnelID) |
| `0x06` | `MsgRegisterErr` | Server → Client | Registration error |
| `0x07` | `MsgHTTPReq` | Server → Client | Proxied HTTP request |
| `0x08` | `MsgHTTPRes` | Client → Server | HTTP response from local server |
| `0x09` | `MsgCloseTunnel` | Both | Close tunnel request |
| `0x0A` | `MsgError` | Both | Error message |

### HTTP request payload (`MsgHTTPReq`)

```
[methodLen:2][method][pathLen:2][path][headerCount:4]([keyLen:2][key][valueLen:2][value])*[bodyLen:4][body]
```

### HTTP response payload (`MsgHTTPRes`)

```
[statusCode:2][headerCount:4]([keyLen:2][key][valueLen:2][value])*[bodyLen:4][body]
```

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
    LocalPort uint16
    Handler   *ClientHandler
    CreatedAt time.Time
}

type Registry struct {
    mu      sync.RWMutex
    tunnels map[string]*Tunnel
    nextID  uint64
}
```

### 2. ClientHandler (`internal/server/handler.go`)

Handles one client WebSocket connection. Each connected client gets its own instance.

**Lifecycle:**

1. `NewClientHandler` — stores connection and registry reference.
2. `Run()` — main read loop:
   - Sets read deadlines, registers Pong handler.
   - Starts `pingLoop` goroutine (sends WebSocket Ping every 30s).
   - Reads binary messages, decodes frames, dispatches by type.
3. `handleAuth` — accepts any token, replies `MsgAuthOK`.
4. `handleRegister` — reads port from payload, registers tunnel, replies with public URL.
5. `handleHTTPRes` — receives response from client, routes to awaiting `ForwardHTTP` via channel.

**Request multiplexing:**

Multiple HTTP requests flow over a single WebSocket connection simultaneously:

```go
type pendingReq struct {
    response chan protocol.HTTPResponse
    done     chan struct{}
}
// pending map[uint64]*pendingReq  — RequestID → awaiting request
```

`ForwardHTTP` generates a unique `requestID`, inserts a `pendingReq` into the map, writes `MsgHTTPReq` to WebSocket, then blocks on the response channel (30s timeout). When `MsgHTTPRes` arrives, `handleHTTPRes` finds the entry and delivers the response.

### 3. ReverseProxy (`internal/server/proxy.go`)

HTTP handler for external traffic. Implements `http.Handler`.

**Algorithm:**

1. Extract subdomain from `Host` header using `extractSubdomain(host, baseDomain)`.
2. Look up tunnel in registry → 404 if missing.
3. Read request body (32MB limit).
4. Serialize into `protocol.HTTPRequest`.
5. Call `tunnel.Handler.ForwardHTTP(req)` — blocks until response.
6. Write HTTP response back to external user.

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
2. Send `MsgAuth` with token.
3. Send `MsgRegister` with local port and optional subdomain.
4. Parse public URL and tunnelID from response.

**Run():**
- Reads messages in a loop.
- `MsgHTTPReq` → spawns `handleHTTPReq` goroutine (concurrent handling).
- `MsgCloseTunnel` → exits loop.

**handleHTTPReq():**
1. Deserialize HTTP request from payload.
2. Build `http.Request` targeting `http://localhost:<port><path>`.
3. Execute request via `http.Client` (30s timeout).
4. Read response body (32MB limit).
5. Serialize response, send `MsgHTTPRes` back.

**Error handling:** Returns HTTP 502 to the server on failure.

### 2. Client entry point (`cmd/client/main.go`)

Reconnection loop with exponential backoff: 1s → 2s → 4s → ... → 30s max. Resets on successful connection. Handles SIGINT for graceful shutdown.

---

## Installation & Quick Start

### Requirements

- Go 1.22+
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
go run ./cmd/client -local 3000

# Terminal 4 — test
curl -H "Host: <hex-from-output>.localhost:8080" http://localhost:8080/
```

### Building binaries

```bash
# Linux
./scripts/build.sh --linux-only

# Windows (PowerShell)
.\scripts\build.ps1 -WindowsOnly
```

Output in `build/linux/` and `build/windows/`.

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
13. **Proxy wakes up** — `ForwardHTTP` returns `HTTPResponse`
14. **Proxy writes to external user** — HTTP 200 + JSON body
15. **External user gets response** — `{"status": "ok", "uptime": 12345}`

---

## CLI Flags

### Server (`cmd/server`)

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:4443` | WebSocket listen address for tunnel clients |
| `-http` | `:8080` | HTTP listen address for public traffic |
| `-domain` | `localhost:8080` | Base domain for tunnel URLs (e.g. `tunnel.example.com`) |

### Client (`cmd/client`)

| Flag | Default | Description |
|---|---|---|
| `-server` | `localhost:4443` | VLGR server address |
| `-local` | **required** | Local port to expose |
| `-token` | `vlgr-token` | Authentication token |
| `-subdomain` | auto | Request specific subdomain |
| `-tls` | `false` | Use WSS (TLS) — required when connecting via Caddy/HTTPS |

---

## Production Deployment

See [GETTING_STARTED.md](GETTING_STARTED.md) for the full production guide covering:

- Cloudflare DNS configuration
- Caddy setup with wildcard TLS (Let's Encrypt DNS-01)
- Cloudflare API token creation
- Caddy rebuild with Cloudflare DNS plugin
- VLGR server systemd unit
- Client connection via WSS

### Quick production commands

```bash
# Server (VPS)
./vlgr-server -addr 127.0.0.1:4443 -http 127.0.0.1:8080 -domain tunnel.domain.com

# Client (your machine)
./vlgr-client -server tunnel.domain.com:443 -local 3000 -tls
```

---

## Arduino / IoT

VLGR runs on your computer and exposes any local HTTP server to the internet — including IoT devices on your local network (ESP32, ESP8266, Arduino with Ethernet shield, etc.).

### Typical setup

```
┌──────────────────┐   Wi-Fi / Ethernet    ┌──────────────────┐   WebSocket    ┌──────────────────┐
│   ESP32 / IoT    │──────────────────────►│   VLGR Client    │◄──────────────►│   VLGR Server    │
│   192.168.1.42   │     HTTP on LAN       │   (your laptop)  │                │   (VPS, public)  │
│   :80  web UI    │                       │                  │                │                  │
└──────────────────┘                       └──────────────────┘                └────────┬─────────┘
                                                                                        │
                                                                                 public URL
                                                                                        │
                                                                              ┌─────────┴─────────┐
                                                                              │  External user    │
                                                                              └───────────────────┘
```

### Step 1 — Arduino/ESP web server

Example sketch for ESP32 (Arduino IDE):

```cpp
#include <WiFi.h>

const char* ssid = "YOUR_WIFI";
const char* password = "YOUR_PASS";

WiFiServer server(80);

void setup() {
  Serial.begin(115200);
  WiFi.begin(ssid, password);
  while (WiFi.status() != WL_CONNECTED) { delay(500); }
  Serial.print("IP: "); Serial.println(WiFi.localIP());
  server.begin();
}

void loop() {
  WiFiClient client = server.accept();
  if (!client) return;

  String request = client.readStringUntil('\r');
  client.println("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n");
  client.println("{\"device\":\"ESP32\",\"temp\":23.5,\"uptime\":" + String(millis() / 1000) + "}");

  client.stop();
}
```

### Step 2 — Expose with VLGR

```bash
# Run VLGR client pointing to ESP32's IP and port
./vlgr-client -server tunnel.domain.com:443 -local 192.168.1.42:80 -tls
```

The ESP32 web server is now reachable at `https://<subdomain>.tunnel.domain.com` from anywhere.
