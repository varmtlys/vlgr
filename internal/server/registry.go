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
	LocalPort uint16
	Handler   *ClientHandler
	CreatedAt time.Time
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

func (r *Registry) Register(subdomain string, localPort uint16, handler *ClientHandler) (*Tunnel, error) {
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
		LocalPort: localPort,
		Handler:   handler,
		CreatedAt: time.Now(),
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

func generateSubdomain() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		log.Printf("[registry] CRITICAL: crypto/rand failed, using timestamp fallback: %v", err)
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x", now)
	}
	return fmt.Sprintf("%x", b)
}
