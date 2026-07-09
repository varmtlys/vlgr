package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/server"
	"vlgr/internal/version"
)

var (
	wsAddr   = flag.String("addr", ":4443", "WebSocket listen address for tunnel clients")
	httpAddr = flag.String("http", ":8080", "HTTP listen address for public traffic")
	domain   = flag.String("domain", "localhost:8080", "Base domain for tunnel URLs (e.g. tunnel.domain.com)")
	token    = flag.String("token", "", "Required authentication token for clients (empty = no auth)")
	verbose  = flag.String("verbose", "info", "Log level: info or debug")
	help     = flag.Bool("h", false, "Show help")
	showVer  = flag.Bool("version", false, "Show version")
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
  --version, -v Show version and exit
  --help, -h    Show this help

Environment:
  VLGR_TOKEN    Auth token (overrides --token if set)

Examples:
  vlgr-server
  vlgr-server -a :4443 -w :8080 -d tunnel.domain.com -t mysecret
  vlgr-server --addr 127.0.0.1:4443 --http 127.0.0.1:8080 --domain tunnel.example.com -V debug
`)
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
		// https://host.example.com.evil.com for host.example.com.
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
	proxy := server.NewReverseProxy(registry, baseDomain, debug)

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

		handler := server.NewClientHandler(conn, registry, baseDomain, *token, debug)
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
