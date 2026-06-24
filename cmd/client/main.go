package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"vlgr/internal/client"
)

var (
	serverAddr = flag.String("server", "localhost:4443", "Tunnel server address")
	localPorts = flag.String("local", "", "Local ports to expose, comma-separated (e.g. '8080,3000')")
	token      = flag.String("token", "vlgr-token", "Authentication token")
	subdomains = flag.String("subdomain", "", "Request custom subdomains, comma-separated (order matches -local)")
	useTLS     = flag.Bool("tls", false, "Use WSS (TLS) — required when connecting via Caddy/HTTPS")
	debug      = flag.Bool("debug", false, "Enable verbose debug logging")
)

func parsePorts(s string) ([]uint16, error) {
	parts := strings.Split(s, ",")
	var ports []uint16
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		if p <= 0 || p > 65535 {
			return nil, err
		}
		ports = append(ports, uint16(p))
	}
	return ports, nil
}

func main() {
	flag.Parse()

	if *localPorts == "" {
		log.Fatal("please specify -local <port> or -local <port1,port2,...>")
	}

	ports, err := parsePorts(*localPorts)
	if err != nil || len(ports) == 0 {
		log.Fatalf("invalid port(s): %q", *localPorts)
	}

	var subs []string
	if *subdomains != "" {
		for _, s := range strings.Split(*subdomains, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				subs = append(subs, s)
			}
		}
	}
	if len(subs) > 0 && len(subs) != len(ports) {
		log.Fatalf("number of subdomains (%d) must match number of ports (%d)", len(subs), len(ports))
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		tunnel := client.NewTunnel(*serverAddr, *token, ports, subs, *useTLS)
		tunnel.SetDebug(*debug)

		if err := tunnel.Connect(); err != nil {
			log.Printf("[client] connection failed: %v, retrying in %v...", err, backoff)
			select {
			case <-sigCh:
				log.Println("[client] shutting down")
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = 1 * time.Second
		log.Printf("========================================")
		log.Printf("  Tunnels: %s", tunnel.PublicURL())
		log.Printf("  Local:   %s", *localPorts)
		log.Printf("========================================")

		done := make(chan struct{})
		go func() {
			tunnel.Run()
			close(done)
		}()

		select {
		case <-done:
			log.Printf("[client] disconnected, reconnecting...")
		case <-sigCh:
			log.Println("[client] shutting down")
			tunnel.Close()
			return
		}

		time.Sleep(1 * time.Second)
	}
}
