package server

import (
	"crypto/rand"
	"fmt"
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
	nextID  uint64
}

func NewRegistry() *Registry {
	return &Registry{
		tunnels: make(map[string]*Tunnel),
		nextID:  1,
	}
}

func (r *Registry) Register(subdomain string, localPort uint16, handler *ClientHandler) (*Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tunnels[subdomain]; exists {
		return nil, fmt.Errorf("subdomain %q already taken", subdomain)
	}

	t := &Tunnel{
		ID:        r.nextID,
		Subdomain: subdomain,
		LocalPort: localPort,
		Handler:   handler,
		CreatedAt: time.Now(),
	}
	r.nextID++
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
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
