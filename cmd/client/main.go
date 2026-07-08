package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
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
	addPorts   = flag.String("add", "", "Add a port with subdomain to running instance: '<port> <subdomain>'")
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
  vlgr-client --add "<port> <subdomain>" [flags]

Flags:
  --server, -s    VLGR server address                            (default localhost:4443)
  --ports, -p     Local port(s) to expose, comma-separated        (required)
  --token, -t     Authentication token                            (default empty)
  --subdomain, -u Request custom subdomain(s), comma-separated    (default auto)
  --tls           Use WSS (TLS) — required via Caddy/HTTPS        (default false)
  --verbose, -v   Log level: info (default) or debug
  --add           Add a port with subdomain to running instance   (e.g. "5000 mysub")
  --help, -h      Show this help

Examples:
  vlgr-client -p 3000
  vlgr-client -s tunnel.domain.com:443 -p 3000 --tls -t mysecret
  vlgr-client -s tunnel.domain.com:443 -p "8080,3000" -u "api,web" --tls -v debug
  vlgr-client --add "5000 mysub"
`)
}

func ctlFilePath() string {
	return filepath.Join(os.TempDir(), "vlgr-client.ctl")
}

func runAddMode() {
	val := strings.TrimSpace(*addPorts)
	if val == "" {
		log.Fatal("usage: --add \"<port> <subdomain>\"")
	}

	parts := strings.Fields(val)
	if len(parts) != 2 {
		log.Fatalf("invalid --add value %q: must be \"<port> <subdomain>\"", val)
	}

	port, err := strconv.Atoi(parts[0])
	if err != nil || port < 1 || port > 65535 {
		log.Fatalf("invalid port in --add: %q", parts[0])
	}
	subdomain := parts[1]

	addrBytes, err := os.ReadFile(ctlFilePath())
	if err != nil {
		log.Fatalf("no running client found: %v (start vlgr-client first)", err)
	}
	ctlAddr := strings.TrimSpace(string(addrBytes))

	conn, err := net.DialTimeout("tcp", ctlAddr, 5*time.Second)
	if err != nil {
		log.Fatalf("cannot connect to running client at %s: %v", ctlAddr, err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	cmd := fmt.Sprintf("ADD %d %s\n", port, subdomain)
	if _, err := fmt.Fprint(conn, cmd); err != nil {
		log.Fatalf("send command: %v", err)
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		log.Fatalf("read response: %v", err)
	}
	line = strings.TrimSpace(line)

	respParts := strings.SplitN(line, " ", 2)
	if len(respParts) < 2 || respParts[0] != "OK" {
		msg := "unknown error"
		if len(respParts) > 1 {
			msg = respParts[1]
		}
		log.Fatalf("add port failed: %s", msg)
	}

	log.Printf("[client] added: %s -> localhost:%d", respParts[1], port)
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

	if *addPorts != "" {
		runAddMode()
		return
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
		os.Remove(ctlFilePath())

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

		if err := tunnel.StartControl(); err != nil {
			log.Printf("[client] control listener start failed: %v", err)
		} else {
			ctlAddr := tunnel.ControlAddr()
			os.WriteFile(ctlFilePath(), []byte(ctlAddr+"\n"), 0600)
			if *verbose == "debug" {
				log.Printf("[debug] control socket: %s", ctlAddr)
			}
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
			os.Remove(ctlFilePath())
			return
		}

		time.Sleep(1 * time.Second)
	}
}
