package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"

	"vlgr/internal/server"
)

var (
	wsAddr   = flag.String("addr", ":4443", "WebSocket listen address for tunnel clients")
	httpAddr = flag.String("http", ":8080", "HTTP listen address for public traffic")
	domain   = flag.String("domain", "localhost:8080", "Base domain for tunnel URLs (e.g. tunnel.domain.com)")
	token    = flag.String("token", "", "Required authentication token for clients (empty = no auth)")
	debug    = flag.Bool("debug", false, "Enable verbose debug logging")
)

const maxConns = 1000

var connSem = make(chan struct{}, maxConns)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return strings.HasPrefix(origin, "https://"+r.Host) ||
			strings.HasPrefix(origin, "http://"+r.Host)
	},
}

func main() {
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("VLGR_TOKEN")
	}

	baseDomain := *domain
	registry := server.NewRegistry()
	proxy := server.NewReverseProxy(registry, baseDomain, *debug)

	http.HandleFunc("/_tunnel", func(w http.ResponseWriter, r *http.Request) {
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

		handler := server.NewClientHandler(conn, registry, baseDomain, *token, *debug)
		log.Printf("[server] new client connected")
		handler.Run()
	})

	go func() {
		log.Printf("[server] public HTTP listening on %s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, proxy); err != nil {
			log.Fatalf("[server] public HTTP error: %v", err)
		}
	}()

	log.Printf("[server] tunnel WebSocket listening on %s", *wsAddr)
	if err := http.ListenAndServe(*wsAddr, nil); err != nil {
		log.Fatalf("[server] tunnel server error: %v", err)
	}
}
