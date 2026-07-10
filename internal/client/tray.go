//go:build windows || linux

package client

import (
	"encoding/base64"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"

	"fyne.io/systray"
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

// Tray shows a system tray icon for one client instance with a menu to
// view, add and remove port forwards.
type Tray struct {
	label  string
	onQuit func()

	mu     sync.Mutex
	tunnel *Tunnel
	stop   chan struct{} // closed on every menu rebuild to release old click watchers
	ready  bool
}

func NewTray(label string, onQuit func()) *Tray {
	return &Tray{label: label, onQuit: onQuit}
}

// Run blocks until Stop is called or Quit is chosen in the menu. It must be
// called from the main goroutine.
func (tr *Tray) Run() {
	systray.Run(func() {
		if icon := trayIcon(); icon != nil {
			systray.SetIcon(icon)
		}
		systray.SetTitle("vlgr")
		systray.SetTooltip(tr.label)
		tr.mu.Lock()
		tr.ready = true
		tr.mu.Unlock()
		tr.Refresh()
	}, nil)
}

// Stop terminates the tray loop (Run returns).
func (tr *Tray) Stop() {
	systray.Quit()
}

// SetTunnel switches the tray to a (re)connected tunnel; nil marks the
// instance as disconnected.
func (tr *Tray) SetTunnel(t *Tunnel) {
	tr.mu.Lock()
	tr.tunnel = t
	tr.mu.Unlock()
	tr.Refresh()
}

// Refresh rebuilds the menu from the current forwards.
func (tr *Tray) Refresh() {
	tr.mu.Lock()
	if !tr.ready {
		tr.mu.Unlock()
		return
	}
	if tr.stop != nil {
		close(tr.stop)
	}
	stop := make(chan struct{})
	tr.stop = stop
	t := tr.tunnel
	tr.mu.Unlock()

	systray.ResetMenu()

	title := systray.AddMenuItem(tr.label, "")
	title.Disable()
	systray.AddSeparator()

	var forwards []Forward
	if t != nil {
		forwards = t.Forwards()
	}
	if t == nil {
		item := systray.AddMenuItem("disconnected…", "")
		item.Disable()
	} else if len(forwards) == 0 {
		item := systray.AddMenuItem("no forwards", "")
		item.Disable()
	}

	scheme := "http"
	if t != nil && t.useTLS {
		scheme = "https"
	}

	for _, f := range forwards {
		f := f
		item := systray.AddMenuItem(fmt.Sprintf("%d → %s", f.Port, f.URL), "")
		open := item.AddSubMenuItem("Open in browser", "")
		del := item.AddSubMenuItem("Remove forward", "")
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-open.ClickedCh:
					openBrowser(scheme + "://" + f.URL)
				case <-del.ClickedCh:
					if err := t.RemovePort(f.Port); err != nil {
						log.Printf("[tray] remove port %d: %v", f.Port, err)
					}
				}
			}
		}()
	}

	systray.AddSeparator()
	add := systray.AddMenuItem("Add forward…", "Add a port forward")
	if t == nil {
		add.Disable()
	}
	quit := systray.AddMenuItem("Quit", "Stop this vlgr-client instance")

	go func() {
		for {
			select {
			case <-stop:
				return
			case <-add.ClickedCh:
				go tr.promptAdd(t)
			case <-quit.ClickedCh:
				if tr.onQuit != nil {
					tr.onQuit()
				}
				return
			}
		}
	}()
}

// promptAdd asks the user for "<port> [subdomain]" via a native input box
// and registers the forward.
func (tr *Tray) promptAdd(t *Tunnel) {
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
	var p int
	if _, err := fmt.Sscanf(s, "%d", &p); err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port: %q", s)
	}
	return uint16(p), nil
}
