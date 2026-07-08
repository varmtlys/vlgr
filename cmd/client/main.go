package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vlgr/internal/client"
)

var (
	serverAddr = flag.String("server", "localhost:4443", "Tunnel server address")
	localPorts = flag.String("ports", "", "Local ports to expose, comma-separated (e.g. '8080,3000')")
	token      = flag.String("token", "", "Authentication token")
	subdomains = flag.String("subdomain", "", "Request custom subdomains, comma-separated (order matches -ports)")
	useTLS     = flag.Bool("tls", false, "Use WSS (TLS) — required when connecting via Caddy/HTTPS")
	verbose    = flag.String("verbose", "info", "Log level: info or debug")
	help       = flag.Bool("h", false, "Show help")
)

func init() {
	flag.StringVar(serverAddr, "s", "localhost:4443", "Tunnel server address (shorthand)")
	flag.StringVar(localPorts, "p", "", "Local ports to expose (shorthand)")
	flag.StringVar(token, "t", "", "Authentication token (shorthand)")
	flag.StringVar(subdomains, "u", "", "Request custom subdomains (shorthand)")
	flag.StringVar(verbose, "v", "info", "Log level (shorthand)")

	flag.Usage = printClientHelp
}

func printClientHelp() {
	fmt.Print(`vlgr-client — VLGR tunnel client

Usage:
  vlgr-client --ports <ports> [flags]

Flags:
  --server, -s    VLGR server address                            (default localhost:4443)
  --ports, -p     Local port(s) to expose, comma-separated        (required)
  --token, -t     Authentication token                            (default empty)
  --subdomain, -u Request custom subdomain(s), comma-separated    (default auto)
  --tls           Use WSS (TLS) — required via Caddy/HTTPS        (default false)
  --verbose, -v   Log level: info (default) or debug
  --help, -h      Show this help

Examples:
  vlgr-client -p 3000
  vlgr-client -s tunnel.domain.com:443 -p 3000 --tls -t mysecret
  vlgr-client -s tunnel.domain.com:443 -p "8080,3000" -u "api,web" --tls -v debug
`)
}

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
			return nil, fmt.Errorf("port out of range: %d", p)
		}
		ports = append(ports, uint16(p))
	}
	return ports, nil
}

var serverAddrRe = regexp.MustCompile(`^[A-Za-z0-9._-]+:[0-9]{1,5}$`)

func validateServerAddr(s string) error {
	if s == "" {
		return fmt.Errorf("server address is empty")
	}
	if !serverAddrRe.MatchString(s) {
		return fmt.Errorf("invalid server address %q: must be host:port with no @, /, or scheme", s)
	}
	parts := strings.Split(s, ":")
	p, err := strconv.Atoi(parts[1])
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("invalid port in server address %q", s)
	}
	return nil
}

func main() {
	flag.Parse()

	if *help {
		printClientHelp()
		os.Exit(0)
	}

	if err := validateServerAddr(*serverAddr); err != nil {
		log.Fatalf("[client] %v", err)
	}

	if *localPorts == "" {
		log.Fatal("please specify -ports <port> or -ports <port1,port2,...>")
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
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	var tunnel *client.Tunnel
	for {
		if tunnel != nil {
			tunnel.Close()
		}
		tunnel = client.NewTunnel(*serverAddr, *token, ports, subs, *useTLS)
		tunnel.SetVerbose(*verbose)

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
