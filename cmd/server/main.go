package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/selfupdate"
	"vlgr/internal/server"
	"vlgr/internal/sshtun"
	"vlgr/internal/version"
)

var (
	wsAddr     = flag.String("addr", ":4443", "WebSocket listen address for tunnel clients")
	httpAddr   = flag.String("http", ":8080", "HTTP listen address for public traffic")
	domain     = flag.String("domain", "localhost:8080", "Base domain for tunnel URLs (e.g. tunnel.domain.com)")
	token      = flag.String("token", "", "Required authentication token for clients (empty = no auth)")
	verbose    = flag.String("verbose", "info", "Log level: info or debug")
	tcpPorts   = flag.String("tcp-ports", "", "Public TCP port range for raw TCP tunnels, e.g. '20000-20100' (empty = disabled)")
	basicAuth  = flag.String("basic-auth", "", "Protect all tunnels with HTTP Basic auth, 'user:pass' (empty = off)")
	allowIPs   = flag.String("allow-ips", "", "Comma-separated IP/CIDR allowlist for public traffic (empty = allow all)")
	adminAddr  = flag.String("admin", "", "REST API + dashboard listen address, e.g. 127.0.0.1:4041 (empty = disabled)")
	tlsPass    = flag.String("tls-passthrough", "", "Public TLS-passthrough (SNI) listen address, e.g. :443 (empty = disabled)")
	sshAddr    = flag.String("ssh", "", "Agentless SSH tunnel listen address, e.g. :2222 (empty = disabled)")
	sshPorts   = flag.String("ssh-ports", "", "Public TCP port range for SSH remote forwards, e.g. '20200-20300'")
	sshHostKey = flag.String("ssh-hostkey", "", "Path to persist the SSH host key (empty = ephemeral per start)")
	autoUpdate = flag.Bool("autoupdate", false, "Check for newer releases and self-update in the background")
	help       = flag.Bool("h", false, "Show help")
	showVer    = flag.Bool("version", false, "Show version")
)

func init() {
	flag.StringVar(wsAddr, "a", ":4443", "WebSocket listen address (shorthand)")
	flag.StringVar(httpAddr, "w", ":8080", "HTTP listen address (shorthand)")
	flag.StringVar(domain, "d", "localhost:8080", "Base domain (shorthand)")
	flag.StringVar(token, "t", "", "Authentication token (shorthand)")
	flag.StringVar(verbose, "V", "info", "Log level (shorthand)")
	flag.BoolVar(showVer, "v", false, "Show version (shorthand)")

	flag.Usage = printServerHelp
}

func printServerHelp() {
	fmt.Print(`vlgr-server — VLGR tunnel relay server
Version: ` + version.String() + `

Usage:
  vlgr-server [flags]

Flags:
  --addr, -a    WebSocket listen address for tunnel clients   (default :4443)
  --http, -w    HTTP listen address for public traffic        (default :8080)
  --domain, -d  Base domain for tunnel URLs                   (default localhost:8080)
  --token, -t   Auth token for clients (empty = no auth)
  --verbose, -V Log level: info (default) or debug
  --tcp-ports   Public TCP port range for raw TCP tunnels     (e.g. 20000-20100)
  --basic-auth  Protect all tunnels with HTTP Basic auth      (user:pass)
  --allow-ips   IP/CIDR allowlist for public traffic          (comma-separated)
  --admin       REST API + dashboard address                  (e.g. 127.0.0.1:4041)
  --tls-passthrough  Public TLS-passthrough (SNI) address     (e.g. :443)
  --ssh         Agentless SSH tunnel listen address           (e.g. :2222)
  --ssh-ports   Public TCP port range for SSH remote forwards (e.g. 20200-20300)
  --ssh-hostkey Path to persist the SSH host key              (empty = ephemeral)
  --autoupdate  Self-update from GitHub releases in the background (default false)
  --version, -v Show version and exit
  --help, -h    Show this help

Environment:
  VLGR_TOKEN    Auth token (overrides --token if set)

Examples:
  vlgr-server
  vlgr-server -a :4443 -w :8080 -d tunnel.domain.com -t mysecret
  vlgr-server --addr 127.0.0.1:4443 --http 127.0.0.1:8080 --domain tunnel.domain.com -V debug
`)
}

// hostOnly strips a trailing :port from a base domain, so TCP tunnel URLs
// use the bare hostname the client can reach directly.
func hostOnly(domain string) string {
	if h, _, err := net.SplitHostPort(domain); err == nil {
		return h
	}
	return domain
}

// parsePortRange parses "start-end" into inclusive bounds.
func parsePortRange(s string) (uint16, uint16, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected format start-end, got %q", s)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start < 1 || start > 65535 {
		return 0, 0, fmt.Errorf("invalid start port %q", parts[0])
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || end < 1 || end > 65535 {
		return 0, 0, fmt.Errorf("invalid end port %q", parts[1])
	}
	if end < start {
		return 0, 0, fmt.Errorf("end port %d is less than start port %d", end, start)
	}
	return uint16(start), uint16(end), nil
}

const maxConns = 1000

var connSem = make(chan struct{}, maxConns)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		// Exact host match: a prefix check would pass
		// https://host.domain.com.evil.com for host.domain.com.
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	},
}

func main() {
	flag.Parse()

	if *help {
		printServerHelp()
		os.Exit(0)
	}

	if *showVer {
		fmt.Printf("vlgr-server %s\n", version.String())
		os.Exit(0)
	}

	if *token == "" {
		*token = os.Getenv("VLGR_TOKEN")
	}

	debug := *verbose == "debug"
	baseDomain := *domain
	registry := server.NewRegistry()
	protect, err := server.NewProtector(*basicAuth, *allowIPs)
	if err != nil {
		log.Fatalf("[server] %v", err)
	}
	proxy := server.NewReverseProxy(registry, baseDomain, debug, protect)

	// Listeners to close before a self-update relaunch, so the replacement can
	// bind the same ports.
	var updateClosers []func()

	var tcpAlloc *server.PortAllocator
	tcpHost := hostOnly(baseDomain)
	if *tcpPorts != "" {
		start, end, err := parsePortRange(*tcpPorts)
		if err != nil {
			log.Fatalf("[server] invalid --tcp-ports: %v", err)
		}
		tcpAlloc = server.NewPortAllocator(start, end)
		log.Printf("[server] raw TCP tunnels enabled on ports %d-%d (host %s)", start, end, tcpHost)
	}

	if *adminAddr != "" {
		admin := server.NewAdminServer(*adminAddr, *token, registry)
		if err := admin.Start(); err != nil {
			log.Fatalf("[server] admin API failed to start on %s: %v", *adminAddr, err)
		}
		updateClosers = append(updateClosers, admin.Stop)
	}

	var tlsRegistry *server.Registry
	if *tlsPass != "" {
		tlsRegistry = server.NewRegistry()
		tp := server.NewTLSPassthrough(*tlsPass, baseDomain, tlsRegistry)
		if err := tp.Start(); err != nil {
			log.Fatalf("[server] TLS passthrough failed to start on %s: %v", *tlsPass, err)
		}
		updateClosers = append(updateClosers, tp.Stop)
	}

	if *sshAddr != "" {
		if *sshPorts == "" {
			log.Fatal("[server] --ssh requires --ssh-ports (public port range for remote forwards)")
		}
		start, end, err := parsePortRange(*sshPorts)
		if err != nil {
			log.Fatalf("[server] invalid --ssh-ports: %v", err)
		}
		sshServer, err := sshtun.New(*sshAddr, *token, *sshHostKey, server.NewPortAllocator(start, end))
		if err != nil {
			log.Fatalf("[server] SSH tunnel setup failed: %v", err)
		}
		if err := sshServer.Start(); err != nil {
			log.Fatalf("[server] SSH tunnel failed to start on %s: %v", *sshAddr, err)
		}
		updateClosers = append(updateClosers, sshServer.Stop)
		log.Printf("[server] agentless SSH forwards use public ports %d-%d", start, end)
	}

	tunnelMux := http.NewServeMux()
	tunnelMux.HandleFunc("/_tunnel", func(w http.ResponseWriter, r *http.Request) {
		select {
		case connSem <- struct{}{}:
			defer func() { <-connSem }()
		default:
			http.Error(w, "too many connections", http.StatusServiceUnavailable)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[server] WebSocket upgrade error: %v", err)
			return
		}

		handler := server.NewClientHandler(conn, registry, baseDomain, *token, debug, tcpAlloc, tcpHost, tlsRegistry)
		log.Printf("[server] new client connected")
		handler.Run()
	})

	publicSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}
	tunnelSrv := &http.Server{
		Addr:              *wsAddr,
		Handler:           tunnelMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if *autoUpdate {
		go selfupdate.Run(context.Background(), selfupdate.Config{
			Repo:    "varmtlys/vlgr",
			Asset:   "vlgr-server",
			Current: version.Version,
			Every:   time.Hour,
			// Close every listener so the replacement can bind the same ports;
			// connected clients reconnect once it is up.
			BeforeRestart: func() {
				publicSrv.Close()
				tunnelSrv.Close()
				for _, stop := range updateClosers {
					stop()
				}
			},
		})
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[server] shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publicSrv.Shutdown(ctx)
		tunnelSrv.Shutdown(ctx)
		// Tunnel WebSocket connections are hijacked and outlive Shutdown;
		// exiting closes them and clients reconnect on their own.
		os.Exit(0)
	}()

	go func() {
		log.Printf("[server] public HTTP listening on %s", *httpAddr)
		if err := publicSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[server] public HTTP error: %v", err)
		}
	}()

	log.Printf("[server] tunnel WebSocket listening on %s", *wsAddr)
	if err := tunnelSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[server] tunnel server error: %v", err)
	}
}
