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

The client opens a persistent WebSocket to the server. The server assigns a unique public URL for each port (e.g., `a3f8b2c1.tunnel.domain.com`). A single client connection can expose multiple local ports — each gets its own subdomain and TunnelID. When an external user hits a public URL, the server serializes the HTTP request into a binary frame (tagged with the corresponding TunnelID), sends it to the client over WebSocket. The client looks up the local port by TunnelID, forwards the request to the correct `localhost:<port>`, serializes the response, and sends it back. The server returns the response to the external user — who sees a normal HTTP exchange.

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
│   ├── build.sh                    # Build for Linux + Windows (Bash)
│   └── deploy-server.sh            # One-command auto-deploy for VPS (multi-distro)
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
| `0x0B` | `MsgStreamData` | Both | WebSocket stream data chunk |
| `0x0C` | `MsgStreamClose` | Both | WebSocket stream close |

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
3. For each port in the comma-separated `-local` list, send `MsgRegister` with port and optional subdomain (matched by position in `-subdomain`).
4. Parse public URLs and tunnelIDs from responses. Store `tunnelID → port` mapping for routing.

**Run():**
- Reads messages in a loop.
- `MsgHTTPReq` → spawns `handleHTTPReq` goroutine (concurrent handling), routes to correct `localhost:<port>` by `frame.TunnelID`.
- `MsgCloseTunnel` → exits loop.

**Multi-tunnel routing:**
When the client registers multiple ports, the server assigns a unique `TunnelID` to each. Incoming `MsgHTTPReq` frames carry the target `TunnelID`. The client looks up the corresponding local port via `mappings[TunnelID]` and forwards the request to the correct `localhost:<port>`.

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
| `-domain` | `localhost:8080` | Base domain for tunnel URLs (e.g. `tunnel.domain.com`) |

### Client (`cmd/client`)

| Flag | Default | Description |
|---|---|---|
| `-server` | `localhost:4443` | VLGR server address |
| `-local` | **required** | Local port(s) to expose, comma-separated (e.g. `3000` or `8080,3000`) |
| `-token` | `vlgr-token` | Authentication token |
| `-subdomain` | auto | Request custom subdomain(s), comma-separated — order matches `-local` |
| `-tls` | `false` | Use WSS (TLS) — required when connecting via Caddy/HTTPS |
| `-debug` | `false` | Enable verbose debug logging |

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

The [`scripts/deploy-server.sh`](scripts/deploy-server.sh) script automates the entire VPS setup — installs Go, builds the server, creates a systemd service, and optionally installs Caddy with Cloudflare DNS plugin. Supports Debian/Ubuntu, RHEL/Fedora, Arch, Alpine, openSUSE, and Void.

```bash
# From the web (as root):
curl -sL https://github.com/varmtlys/vlgr/raw/main/scripts/deploy-server.sh | sudo bash -s -- \
  -d tunnel.domain.com -t my-token

# Or locally with Caddy + Cloudflare:
sudo ./scripts/deploy-server.sh -d tunnel.domain.com -t my-token \
  --caddy --cf-token <CF_API_TOKEN>
```

### Quick production commands

```bash
# Server (VPS)
./vlgr-server -addr 127.0.0.1:4443 -http 127.0.0.1:8080 -domain tunnel.domain.com

# Client (your machine) — single tunnel
./vlgr-client -server tunnel.domain.com:443 -local 3000 -tls

# Client — multiple tunnels
./vlgr-client -server tunnel.domain.com:443 -local "8080,3000,5000" -subdomain "api,web,admin" -tls
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
    sendFrame(0x01, 0, 0, (uint8_t*)authToken, strlen(authToken));
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
