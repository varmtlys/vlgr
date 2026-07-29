//go:build windows || linux

package client

import (
	"encoding/base64"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"vlgr/internal/protocol"
	"vlgr/internal/version"
)

// Tray icon: a diagonal arrow with an arrowhead and tail feathers,
// generated as a 32x32 image and embedded as base64. Windows requires ICO,
// Linux (StatusNotifier) takes PNG.
const (
	trayIconPNG = "iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAABH0lEQVR4nOyWYW4CIRCF35Aep+0Vuj3M9hql1+geRrzC6nnEjJGIBHBYWPnhTkLiLprv+WaGQaFzbAJeQ8DnZH95xfZobfjHZHcEDNdHPY/05++/rQV+/7dfRNAePBrNHPAs1rF9C5jDSN/h+8UOSP9hDo6aIjz+0F76XWvjrqBFCoIii8Y8UpJT3YZsLVu89PfPOAeS9jcRIElBLqpqIIS7VPjvcvlHjQMxONdDaU0kBXCf8yqBu2cnQiIkKoDBimB4hSIewe9EZPo/K0DRDeB/lsJdSA4rcQ2UwqUhErAWHMJhpP0+aglHaRu2hqN0HBNg3Nw/WZiSidhEgH+uK7q061ArovutOHlOp26xLlqloHt0T8EmoLuAcwAAAP//6QaEDOakp5oAAAAASUVORK5CYII="
	trayIconICO = "AAABAAEAICAAAAEAIABYAQAAFgAAAIlQTkcNChoKAAAADUlIRFIAAAAgAAAAIAgGAAAAc3p69AAAAR9JREFUeJzslmFuAiEQhd+QHqftFbo9zPYapdfoHka8wup5xIyRiARwWFj54U5C4i6a7/lmhkGhc2wCXkPA52R/ecX2aG34x2R3BAzXRz2P9Ofvv60Ffv+3X0TQHjwazRzwLNaxfQuYw0jf4fvFDkj/YQ6OmiI8/tBe+l1r466gRQqCIovGPFKSU92GbC1bvPT3zzgHkvY3ESBJQS6qaiCEu1T473L5R40DMTjXQ2lNJAVwn/MqgbtnJ0IiJCqAwYpgeIUiHsHvRGT6PytA0Q3gf5bCXUgOK3ENlMKlIRKwFhzCYaT9PmoJR2kbtoajdBwTYNzcP1mYkonYRIB/riu6tOtQK6L7rTh5TqdusS5apaB7dE/BJqC7gHMAAAD//+kGhAzmpKeaAAAAAElFTkSuQmCC"
)

func trayIcon() []byte {
	b64 := trayIconPNG
	if runtime.GOOS == "windows" {
		b64 = trayIconICO
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		log.Printf("[tray] decode icon: %v", err)
		return nil
	}
	return data
}

// traySlot is one reusable menu row representing a forward. The whole pool
// is created once at startup and only titles/visibility change afterwards,
// so click channels stay valid for the life of the process.
type traySlot struct {
	item *systray.MenuItem
	open *systray.MenuItem
	del  *systray.MenuItem

	mu     sync.Mutex
	active bool
	port   uint16
	url    string
	scheme string
}

func (s *traySlot) set(port uint16, url, scheme string) {
	s.mu.Lock()
	s.active = true
	s.port = port
	s.url = url
	s.scheme = scheme
	s.mu.Unlock()
	s.item.SetTitle(fmt.Sprintf("%d → %s", port, url))
	// Raw TCP/TLS forwards carry their own scheme and are not browsable.
	if strings.Contains(url, "://") {
		s.open.Disable()
	} else {
		s.open.Enable()
	}
	s.item.Show()
}

// href returns the browser URL of the forward, or "" when there is nothing
// a browser can open (inactive slot, or a raw TCP/TLS tunnel).
func (s *traySlot) href() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.url == "" || strings.Contains(s.url, "://") {
		return ""
	}
	return s.scheme + "://" + s.url
}

func (s *traySlot) clear() {
	s.mu.Lock()
	s.active = false
	s.port = 0
	s.url = ""
	s.mu.Unlock()
	s.item.Hide()
}

// target returns the forward this row currently stands for.
func (s *traySlot) target() (active bool, port uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.port
}

// Tray shows a system tray icon for one client instance with a menu to
// view, add and remove port forwards.
type Tray struct {
	label  string
	onQuit func()

	mu     sync.Mutex
	tunnel *Tunnel
	ready  bool

	status *systray.MenuItem
	slots  []*traySlot
	add    *systray.MenuItem
	about  *systray.MenuItem
	quit   *systray.MenuItem

	stopCh   chan struct{} // closed once on shutdown to release click watchers
	stopOnce sync.Once
}

func NewTray(label string, onQuit func()) *Tray {
	return &Tray{label: label, onQuit: onQuit, stopCh: make(chan struct{})}
}

// Run blocks until Stop is called or Quit is chosen in the menu. It must be
// called from the main goroutine.
func (tr *Tray) Run() {
	systray.Run(tr.onReady, nil)
}

// onReady builds the static menu once. Click watchers are wired here and
// live for the whole process; Refresh only updates titles and visibility.
func (tr *Tray) onReady() {
	if icon := trayIcon(); icon != nil {
		systray.SetIcon(icon)
	}
	systray.SetTitle("vlgr")
	systray.SetTooltip(tr.label)

	title := systray.AddMenuItem(tr.label, "")
	title.Disable()
	systray.AddSeparator()

	tr.status = systray.AddMenuItem("connecting…", "")
	tr.status.Disable()

	// Fixed pool of forward rows. Visibility is NOT touched here: on Windows
	// the native menu items are created asynchronously after onReady returns,
	// so calling Hide()/Show() now fails with "menu item not found". The
	// deferred Refresh below runs once the backend has settled.
	tr.slots = make([]*traySlot, protocol.MaxTunnelsPerClient)
	for i := range tr.slots {
		item := systray.AddMenuItem("", "")
		s := &traySlot{
			item: item,
			open: item.AddSubMenuItem("Open in browser", ""),
			del:  item.AddSubMenuItem("Remove forward", ""),
		}
		tr.slots[i] = s
		go tr.watchSlot(s)
	}

	systray.AddSeparator()
	tr.add = systray.AddMenuItem("Add forward…", "Add a port forward")
	tr.about = systray.AddMenuItem("About "+version.Name+"…", "Show version and application info")
	tr.quit = systray.AddMenuItem("Quit", "Stop this vlgr-client instance")

	go tr.watchControls()

	// Defer the first Refresh until the native menu exists. A later Refresh
	// from SetTunnel/onChange will correct the state again anyway.
	go func() {
		time.Sleep(300 * time.Millisecond)
		tr.mu.Lock()
		tr.ready = true
		tr.mu.Unlock()
		tr.Refresh()
	}()
}

func (tr *Tray) watchSlot(s *traySlot) {
	for {
		select {
		case <-tr.stopCh:
			return
		case <-s.open.ClickedCh:
			if href := s.href(); href != "" {
				openBrowser(href)
			}
		case <-s.del.ClickedCh:
			active, port := s.target()
			t := tr.currentTunnel()
			if active && t != nil {
				go func() {
					if err := t.RemovePort(port); err != nil {
						log.Printf("[tray] remove port %d: %v", port, err)
					}
				}()
			}
		}
	}
}

func (tr *Tray) watchControls() {
	for {
		select {
		case <-tr.stopCh:
			return
		case <-tr.add.ClickedCh:
			go tr.promptAdd()
		case <-tr.about.ClickedCh:
			go showMessage(version.Name, version.About("vlgr-client"))
		case <-tr.quit.ClickedCh:
			if tr.onQuit != nil {
				tr.onQuit()
			}
			return
		}
	}
}

// Stop terminates the tray loop (Run returns) and releases the click watchers.
func (tr *Tray) Stop() {
	tr.stopOnce.Do(func() { close(tr.stopCh) })
	systray.Quit()
}

func (tr *Tray) currentTunnel() *Tunnel {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.tunnel
}

// SetTunnel switches the tray to a (re)connected tunnel; nil marks the
// instance as disconnected.
func (tr *Tray) SetTunnel(t *Tunnel) {
	tr.mu.Lock()
	tr.tunnel = t
	tr.mu.Unlock()
	tr.Refresh()
}

// Refresh updates the existing menu items from the current forwards. Held
// under tr.mu so concurrent callers (tunnel change events, SetTunnel) cannot
// interleave menu mutations.
func (tr *Tray) Refresh() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.ready {
		return
	}
	t := tr.tunnel

	var forwards []Forward
	scheme := "http"
	if t != nil {
		forwards = t.Forwards()
		if t.useTLS {
			scheme = "https"
		}
	}

	for i, s := range tr.slots {
		if i < len(forwards) {
			s.set(forwards[i].Port, forwards[i].URL, scheme)
		} else {
			s.clear()
		}
	}

	switch {
	case t == nil:
		tr.status.SetTitle("disconnected…")
		tr.status.Show()
		tr.add.Disable()
	case len(forwards) == 0:
		tr.status.SetTitle("no forwards")
		tr.status.Show()
		tr.add.Enable()
	default:
		tr.status.Hide()
		tr.add.Enable()
	}
}

// promptAdd asks the user for "<port> [subdomain]" via a native input box
// and registers the forward.
func (tr *Tray) promptAdd() {
	t := tr.currentTunnel()
	if t == nil {
		return
	}
	input, err := promptInput("vlgr — add forward", "Enter: <port> [subdomain]")
	if err != nil {
		log.Printf("[tray] input dialog: %v", err)
		return
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return // cancelled
	}
	parts := strings.Fields(input)
	port, err := parsePort(parts[0])
	if err != nil {
		log.Printf("[tray] %v", err)
		return
	}
	subdomain := ""
	if len(parts) > 1 {
		subdomain = parts[1]
	}
	if url, err := t.AddPort(port, subdomain); err != nil {
		log.Printf("[tray] add port %d: %v", port, err)
	} else {
		log.Printf("[client] added: %s -> localhost:%d", url, port)
	}
}

func parsePort(s string) (uint16, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port: %q", s)
	}
	return uint16(p), nil
}
