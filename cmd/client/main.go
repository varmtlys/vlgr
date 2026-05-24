package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"vlgr/internal/client"
)

var (
	serverAddr = flag.String("server", "localhost:4443", "Tunnel server address")
	localPort  = flag.Int("local", 0, "Local port to expose (required)")
	token      = flag.String("token", "vlgr-token", "Authentication token")
	subdomain  = flag.String("subdomain", "", "Request custom subdomain (optional)")
	useTLS     = flag.Bool("tls", false, "Use WSS (TLS) — required when connecting via Caddy/HTTPS")
)

func main() {
	flag.Parse()

	if *localPort == 0 {
		log.Fatal("please specify -local <port>")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		tunnel := client.NewTunnel(*serverAddr, *token, uint16(*localPort), *subdomain, *useTLS)

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
		log.Printf("  Tunnel: %s", tunnel.PublicURL())
		log.Printf("  Local:  http://localhost:%d", *localPort)
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
