package server

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"time"
)

type Tunnel struct {
	ID        uint64
	Subdomain string
	Handler   *ClientHandler
}

type Registry struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel
}

func NewRegistry() *Registry {
	return &Registry{
		tunnels: make(map[string]*Tunnel),
	}
}

func (r *Registry) Register(subdomain string, handler *ClientHandler) (*Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tunnels[subdomain]; exists {
		return nil, fmt.Errorf("subdomain %q already taken", subdomain)
	}

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		log.Printf("[registry] CRITICAL: crypto/rand failed: %v", err)
		return nil, fmt.Errorf("crypto/rand failed: %w", err)
	}
	t := &Tunnel{
		ID:        binary.BigEndian.Uint64(idBytes),
		Subdomain: subdomain,
		Handler:   handler,
	}
	r.tunnels[subdomain] = t
	return t, nil
}

func (r *Registry) Unregister(subdomain string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tunnels, subdomain)
}

func (r *Registry) Get(subdomain string) *Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[subdomain]
}

// generateTunnelID returns a random 64-bit tunnel identifier. Used for TCP
// tunnels, which have no subdomain but still need a unique ID for routing.
func generateTunnelID() uint64 {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		log.Printf("[registry] CRITICAL: crypto/rand failed, using timestamp fallback: %v", err)
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b)
}

func generateSubdomain() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		log.Printf("[registry] CRITICAL: crypto/rand failed, using timestamp fallback: %v", err)
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x", now)
	}
	return fmt.Sprintf("%x", b)
}
