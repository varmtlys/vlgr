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
	"vlgr/internal/version"
)

var (
	serverAddr = flag.String("server", "localhost:4443", "Tunnel server address")
	localPorts = flag.String("ports", "", "Local ports to expose, comma-separated (e.g. '8080,3000')")
	tcpForward = flag.String("tcp", "", "Expose local ports as raw TCP tunnels, comma-separated (e.g. '22' or '22:2222,5432')")
	token      = flag.String("token", "", "Authentication token")
	subdomains = flag.String("subdomain", "", "Request custom subdomains, comma-separated (order matches -ports)")
	useTLS     = flag.Bool("tls", false, "Use WSS (TLS) — required when connecting via Caddy/HTTPS")
	verbose    = flag.String("verbose", "info", "Log level: info or debug")
	addPorts   = flag.String("add", "", "Add a port with subdomain to a running instance: '<port> <subdomain>'")
	delPort    = flag.String("del", "", "Remove a port forward (and its subdomain) from a running instance: '<port>'")
	useTray    = flag.Bool("tray", false, "Show a system tray icon for this instance")
	inspect      = flag.String("inspect", "", "Enable the traffic inspector dashboard on this address (e.g. 127.0.0.1:4040)")
	inspectLimit = flag.Int("inspect-limit", 1000, "Max rows kept in the inspector (older dropped); capped at 100000")
	help         = flag.Bool("h", false, "Show help")
	showVer    = flag.Bool("version", false, "Show version")
)

func init() {
	flag.StringVar(serverAddr, "s", "localhost:4443", "Tunnel server address (shorthand)")
	flag.StringVar(localPorts, "p", "", "Local ports to expose (shorthand)")
	flag.StringVar(token, "t", "", "Authentication token (shorthand)")
	flag.StringVar(subdomains, "u", "", "Request custom subdomains (shorthand)")
	flag.StringVar(verbose, "V", "info", "Log level (shorthand)")
	flag.BoolVar(showVer, "v", false, "Show version (shorthand)")

	flag.Usage = printClientHelp
}

func printClientHelp() {
	fmt.Print(`vlgr-client — VLGR tunnel client
Version: ` + version.String() + `

Usage:
  vlgr-client --ports <ports> [flags]
  vlgr-client --add "<port> <subdomain>"
  vlgr-client --del <port>

Flags:
  --server, -s    VLGR server address                            (default localhost:4443)
  --ports, -p     Local port(s) to expose over HTTP, comma-separated
  --tcp           Local port(s) to expose as raw TCP, comma-separated (e.g. "22" or "22:2222")
  --token, -t     Authentication token                            (default empty)
  --subdomain, -u Request custom subdomain(s), comma-separated    (default auto)
  --tls           Use WSS (TLS) — required via Caddy/HTTPS        (default false)
  --verbose, -V   Log level: info (default) or debug
  --tray          Show a system tray icon for this instance
  --inspect       Traffic inspector dashboard address (e.g. 127.0.0.1:4040)
  --inspect-limit Max rows kept in the inspector (default 1000, max 100000)
  --add           Add a port with subdomain to a running instance (e.g. "5000 mysub")
  --del           Remove a port forward from a running instance   (e.g. 5000)
  --version, -v   Show version and exit
  --help, -h      Show this help

--add and --del cannot be combined with each other or with any other flag.
If several instances are running, a console menu asks which instance to
modify (Ctrl+C cancels).

Examples:
  vlgr-client -p 3000
  vlgr-client -s tunnel.domain.com:443 -p 3000 --tls -t mysecret --tray
  vlgr-client -s tunnel.domain.com:443 -p "8080,3000" -u "api,web" --tls -V debug
  vlgr-client --add "5000 mysub"
  vlgr-client --del 5000
`)
}

// Each instance writes its own control file so several clients can run in
// parallel; the pid in the name lets --add/--del enumerate them.
func ctlFilePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("vlgr-client-%d.ctl", os.Getpid()))
}

// instance is a running vlgr-client discovered via its control file.
type instance struct {
	pid      int
	addr     string
	ctlFile  string
	forwards []string // "port url" lines from LIST
}

// discoverInstances scans the temp dir for control files of running clients,
// queries each for its forwards and silently removes stale files.
func discoverInstances() []instance {
	pattern := filepath.Join(os.TempDir(), "vlgr-client-*.ctl")
	files, _ := filepath.Glob(pattern)
	// Legacy name used by older versions.
	if legacy := filepath.Join(os.TempDir(), "vlgr-client.ctl"); fileExists(legacy) {
		files = append(files, legacy)
	}

	var out []instance
	for _, f := range files {
		addrBytes, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		addr := strings.TrimSpace(string(addrBytes))
		forwards, err := queryForwards(addr)
		if err != nil {
			os.Remove(f) // stale control file — instance is gone
			continue
		}
		pid := 0
		base := filepath.Base(f)
		if n, err := fmt.Sscanf(base, "vlgr-client-%d.ctl", &pid); n != 1 || err != nil {
			pid = 0
		}
		out = append(out, instance{pid: pid, addr: addr, ctlFile: f, forwards: forwards})
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// queryForwards runs LIST against a control socket.
// Response: "OK <n>" followed by n lines "<port> <url>".
func queryForwards(addr string) ([]string, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := fmt.Fprint(conn, "LIST\n"); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) != 2 || parts[0] != "OK" {
		return nil, fmt.Errorf("bad LIST response: %q", line)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("bad LIST count: %q", parts[1])
	}
	forwards := make([]string, 0, n)
	for i := 0; i < n; i++ {
		fl, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		forwards = append(forwards, strings.TrimSpace(fl))
	}
	return forwards, nil
}

// selectInstance returns the target instance for --add/--del. With one
// instance it is used directly; with several a console menu is shown
// (Ctrl+C aborts the whole operation via default SIGINT handling).
func selectInstance() instance {
	instances := discoverInstances()
	if len(instances) == 0 {
		log.Fatal("no running vlgr-client instance found (start vlgr-client first)")
	}
	if len(instances) == 1 {
		return instances[0]
	}

	fmt.Println("Several vlgr-client instances are running:")
	for i, inst := range instances {
		label := fmt.Sprintf("pid %d", inst.pid)
		if inst.pid == 0 {
			label = inst.addr
		}
		desc := "no forwards"
		if len(inst.forwards) > 0 {
			var pairs []string
			for _, f := range inst.forwards {
				p := strings.SplitN(f, " ", 2)
				if len(p) == 2 {
					pairs = append(pairs, fmt.Sprintf("%s -> %s", p[0], p[1]))
				} else {
					pairs = append(pairs, f)
				}
			}
			desc = strings.Join(pairs, ", ")
		}
		fmt.Printf("  [%d] %s: %s\n", i+1, label, desc)
	}

	stdin := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Select instance [1-%d] (Ctrl+C to cancel): ", len(instances))
		line, err := stdin.ReadString('\n')
		if err != nil {
			log.Fatal("cancelled")
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && n >= 1 && n <= len(instances) {
			return instances[n-1]
		}
		fmt.Println("invalid choice")
	}
}

// sendCtlCommand sends one command to a control socket and returns the
// response line.
func sendCtlCommand(addr, cmd string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("cannot connect to running client at %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", fmt.Errorf("send command: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	line = strings.TrimSpace(line)
	respParts := strings.SplitN(line, " ", 2)
	msg := ""
	if len(respParts) > 1 {
		msg = respParts[1]
	}
	if respParts[0] != "OK" {
		if msg == "" {
			msg = "unknown error"
		}
		return "", fmt.Errorf("%s", msg)
	}
	return msg, nil
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

	inst := selectInstance()
	msg, err := sendCtlCommand(inst.addr, fmt.Sprintf("ADD %d %s", port, subdomain))
	if err != nil {
		log.Fatalf("add port failed: %v", err)
	}
	log.Printf("[client] added: %s -> localhost:%d", msg, port)
}

func runDelMode() {
	val := strings.TrimSpace(*delPort)
	port, err := strconv.Atoi(val)
	if err != nil || port < 1 || port > 65535 {
		log.Fatalf("invalid --del value %q: must be a port number", val)
	}

	inst := selectInstance()
	if _, err := sendCtlCommand(inst.addr, fmt.Sprintf("DEL %d", port)); err != nil {
		log.Fatalf("remove port failed: %v", err)
	}
	log.Printf("[client] removed forward for localhost:%d", port)
}

// checkExclusiveFlags enforces that --add/--del are used alone.
func checkExclusiveFlags() {
	visited := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	if !visited["add"] && !visited["del"] {
		return
	}
	for name := range visited {
		if name != "add" && name != "del" {
			log.Fatalf("--add/--del cannot be combined with other flags (got --%s)", name)
		}
	}
	if visited["add"] && visited["del"] {
		log.Fatal("--add and --del cannot be used together")
	}
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

// parseTCPForwards parses "local[:remote],..." into TCP forward specs.
func parseTCPForwards(s string) ([]client.TCPForward, error) {
	var out []client.TCPForward
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		localStr, remoteStr, hasRemote := strings.Cut(part, ":")
		local, err := strconv.Atoi(strings.TrimSpace(localStr))
		if err != nil || local <= 0 || local > 65535 {
			return nil, fmt.Errorf("invalid local port in %q", part)
		}
		remote := 0
		if hasRemote {
			remote, err = strconv.Atoi(strings.TrimSpace(remoteStr))
			if err != nil || remote < 0 || remote > 65535 {
				return nil, fmt.Errorf("invalid remote port in %q", part)
			}
		}
		out = append(out, client.TCPForward{LocalPort: uint16(local), RemotePort: uint16(remote)})
	}
	return out, nil
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

	if *showVer {
		fmt.Printf("vlgr-client %s\n", version.String())
		os.Exit(0)
	}

	checkExclusiveFlags()

	if *addPorts != "" {
		runAddMode()
		return
	}

	if *delPort != "" {
		runDelMode()
		return
	}

	if err := validateServerAddr(*serverAddr); err != nil {
		log.Fatalf("[client] %v", err)
	}

	if *localPorts == "" && *tcpForward == "" {
		log.Fatal("please specify --ports <port[,...]> and/or --tcp <port[,...]>")
	}

	var ports []uint16
	if *localPorts != "" {
		var err error
		ports, err = parsePorts(*localPorts)
		if err != nil || len(ports) == 0 {
			log.Fatalf("invalid port(s): %q", *localPorts)
		}
	}

	var tcpForwards []client.TCPForward
	if *tcpForward != "" {
		var err error
		tcpForwards, err = parseTCPForwards(*tcpForward)
		if err != nil || len(tcpForwards) == 0 {
			log.Fatalf("invalid --tcp value %q: %v", *tcpForward, err)
		}
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

	var dash *client.Dashboard
	if *inspect != "" {
		dash = client.NewDashboard(*inspect, *inspectLimit)
		if err := dash.Start(); err != nil {
			log.Fatalf("[client] inspector dashboard failed to start on %s: %v", *inspect, err)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	if *useTray {
		// systray must own the main goroutine; the tunnel loop runs beside
		// it, and Quit in the tray menu stops everything.
		tray := client.NewTray(fmt.Sprintf("vlgr — %s (pid %d)", *serverAddr, os.Getpid()), func() {
			select {
			case sigCh <- os.Interrupt:
			default:
			}
		})
		go func() {
			runLoop(ports, subs, tcpForwards, sigCh, tray, dash)
			tray.Stop()
		}()
		tray.Run()
		return
	}

	runLoop(ports, subs, tcpForwards, sigCh, nil, dash)
}

// runLoop is the connect/reconnect loop of a foreground client instance.
func runLoop(ports []uint16, subs []string, tcpForwards []client.TCPForward, sigCh chan os.Signal, tray *client.Tray, dash *client.Dashboard) {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	var tunnel *client.Tunnel
	for {
		if tunnel != nil {
			tunnel.Close()
		}
		os.Remove(ctlFilePath())

		tunnel = client.NewTunnel(*serverAddr, *token, ports, subs, *useTLS)
		tunnel.SetTCPForwards(tcpForwards)
		tunnel.SetDebug(*verbose == "debug")
		if dash != nil {
			tunnel.SetDashboard(dash)
		}
		if tray != nil {
			tunnel.SetOnChange(tray.Refresh)
		}

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

		if tray != nil {
			tray.SetTunnel(tunnel)
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
			if tray != nil {
				tray.SetTunnel(nil)
			}
		case <-sigCh:
			log.Println("[client] shutting down")
			tunnel.Close()
			os.Remove(ctlFilePath())
			return
		}

		time.Sleep(1 * time.Second)
	}
}
