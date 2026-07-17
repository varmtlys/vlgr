package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdmin_Tunnels(t *testing.T) {
	reg := NewRegistry()
	reg.Register("alpha", nil)
	reg.Register("beta", nil)
	a := NewAdminServer("127.0.0.1:0", "", reg)

	r := httptest.NewRequest("GET", "/api/tunnels", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Host = "127.0.0.1:4041"
	w := httptest.NewRecorder()
	a.guard(a.handleTunnels)(w, r)

	var got []TunnelInfo
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tunnels, got %d", len(got))
	}
}

func TestAdmin_TokenGuard(t *testing.T) {
	a := NewAdminServer("127.0.0.1:0", "sekret", NewRegistry())

	// Remote caller (non-loopback peer) without a token is rejected.
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr = "203.0.113.7:4444"
	w := httptest.NewRecorder()
	a.guard(a.handleStatus)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no token: want 401, got %d", w.Code)
	}

	r = httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr = "203.0.113.7:4444"
	r.Header.Set("Authorization", "Bearer sekret")
	w = httptest.NewRecorder()
	a.guard(a.handleStatus)(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("valid token: want 200, got %d", w.Code)
	}
}

func TestAdmin_LoopbackGuard(t *testing.T) {
	a := NewAdminServer("127.0.0.1:0", "", NewRegistry()) // no token

	// Loopback peer reaching a loopback Host is trusted (local dashboard).
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Host = "localhost:4041"
	w := httptest.NewRecorder()
	a.guard(a.handleStatus)(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("loopback: want 200, got %d", w.Code)
	}

	// Loopback peer but a foreign Host (DNS-rebinding) is refused.
	r = httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Host = "evil.example.com"
	w = httptest.NewRecorder()
	a.guard(a.handleStatus)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("rebinding: want 401, got %d", w.Code)
	}
}
