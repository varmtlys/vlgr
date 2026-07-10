//go:build !windows && !linux

package client

import "log"

// Tray is a stub for platforms without system tray support in this build
// (the systray backend for darwin requires cgo, which would break
// cross-compilation).
type Tray struct{}

func NewTray(label string, onQuit func()) *Tray { return &Tray{} }

func (tr *Tray) Run() {
	log.Fatal("--tray is not supported in this build")
}

func (tr *Tray) Stop()               {}
func (tr *Tray) SetTunnel(t *Tunnel) {}
func (tr *Tray) Refresh()            {}
