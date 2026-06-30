package server

import (
	"sync"
	"testing"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	h := &ClientHandler{}

	tunnel, err := r.Register("testsub", 3000, h)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if tunnel.Subdomain != "testsub" {
		t.Errorf("Subdomain: want testsub, got %q", tunnel.Subdomain)
	}
	if tunnel.LocalPort != 3000 {
		t.Errorf("LocalPort: want 3000, got %d", tunnel.LocalPort)
	}
	if tunnel.Handler != h {
		t.Error("Handler mismatch")
	}
	if tunnel.ID == 0 {
		t.Error("Tunnel ID should not be 0")
	}

	got := r.Get("testsub")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.ID != tunnel.ID {
		t.Errorf("Get returned wrong tunnel: ID %d vs %d", got.ID, tunnel.ID)
	}
}

func TestRegistry_GetNotFound(t *testing.T) {
	r := NewRegistry()
	got := r.Get("nonexistent")
	if got != nil {
		t.Error("expected nil for nonexistent subdomain")
	}
}

func TestRegistry_RegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	h := &ClientHandler{}

	_, err := r.Register("dup", 3000, h)
	if err != nil {
		t.Fatalf("first Register failed: %v", err)
	}
	_, err = r.Register("dup", 4000, h)
	if err == nil {
		t.Error("expected error for duplicate subdomain")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	h := &ClientHandler{}

	r.Register("temp", 3000, h)
	r.Unregister("temp")

	if got := r.Get("temp"); got != nil {
		t.Error("expected nil after unregister")
	}
}

func TestRegistry_UnregisterNonexistent(t *testing.T) {
	r := NewRegistry()
	r.Unregister("nonexistent")
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	h := &ClientHandler{}
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sub := generateSubdomain()
			r.Register(sub, uint16(n+1000), h)
			r.Get(sub)
			r.Unregister(sub)
		}(i)
	}
	wg.Wait()
	if len(r.tunnels) != 0 {
		t.Errorf("expected empty registry, got %d entries", len(r.tunnels))
	}
}

func TestRegistry_MultipleTunnels(t *testing.T) {
	r := NewRegistry()
	h := &ClientHandler{}

	subs := []string{"api", "web", "admin", "db", "cache"}
	ports := []uint16{3000, 3001, 3002, 5432, 6379}

	for i, sub := range subs {
		tun, err := r.Register(sub, ports[i], h)
		if err != nil {
			t.Fatalf("Register %q failed: %v", sub, err)
		}
		if tun.Subdomain != sub {
			t.Errorf("wrong subdomain: want %q, got %q", sub, tun.Subdomain)
		}
	}

	for _, sub := range subs {
		if got := r.Get(sub); got == nil {
			t.Errorf("Get %q returned nil", sub)
		}
	}
}

func TestGenerateSubdomain_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		s := generateSubdomain()
		if seen[s] {
			t.Errorf("duplicate subdomain generated: %s", s)
		}
		seen[s] = true
	}
}

func TestGenerateSubdomain_Format(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := generateSubdomain()
		if len(s) != 16 {
			t.Errorf("subdomain length: want 16, got %d (%q)", len(s), s)
		}
		for _, c := range s {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("non-hex character in subdomain %q: %c", s, c)
			}
		}
	}
}

func TestTunnel_CreatedAt(t *testing.T) {
	r := NewRegistry()
	h := &ClientHandler{}
	tun, _ := r.Register("timed", 3000, h)
	if tun.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}
