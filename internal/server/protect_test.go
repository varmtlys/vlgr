package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtector_Disabled(t *testing.T) {
	p, err := NewProtector("", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if !p.Allow(httptest.NewRecorder(), r, "sub") {
		t.Error("empty protector must allow everything")
	}
}

func TestProtector_BasicAuth(t *testing.T) {
	p, _ := NewProtector("bob:s3cret", "", 0)

	r := httptest.NewRequest("GET", "/", nil)
	if p.Allow(httptest.NewRecorder(), r, "sub") {
		t.Error("missing credentials must be rejected")
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("bob", "wrong")
	if p.Allow(httptest.NewRecorder(), r, "sub") {
		t.Error("wrong password must be rejected")
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("bob", "s3cret")
	w := httptest.NewRecorder()
	if !p.Allow(w, r, "sub") {
		t.Error("valid credentials must be allowed")
	}
}

func TestProtector_BasicAuthChallenge(t *testing.T) {
	p, _ := NewProtector("bob:s3cret", "", 0)
	w := httptest.NewRecorder()
	p.Allow(w, httptest.NewRequest("GET", "/", nil), "sub")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate challenge")
	}
}

// TestProtector_IPAllowlistDirect checks allowlisting off the direct peer
// (trustedHops = 0), which is the safe default.
func TestProtector_IPAllowlistDirect(t *testing.T) {
	p, err := NewProtector("", "10.0.0.0/8, 192.168.1.5", 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"10.1.2.3":    true,
		"192.168.1.5": true,
		"192.168.1.6": false,
		"8.8.8.8":     false,
	}
	for ip, want := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = ip + ":40000"
		// A spoofed X-Forwarded-For must be ignored at hops = 0.
		r.Header.Set("X-Forwarded-For", "10.0.0.1")
		if got := p.Allow(httptest.NewRecorder(), r, "sub"); got != want {
			t.Errorf("ip %s: allow=%v, want %v", ip, got, want)
		}
	}
}

// TestProtector_IPAllowlistSpoofBlocked is the regression test for the
// X-Forwarded-For spoofing bypass: with one trusted proxy, only the entry the
// proxy appended (rightmost) counts, so an attacker-supplied leftmost value is
// ignored.
func TestProtector_IPAllowlistSpoofBlocked(t *testing.T) {
	p, err := NewProtector("", "10.0.0.0/8", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Attacker (real 8.8.8.8) prepends an allowed IP; Caddy appends the real
	// peer. The rightmost entry (8.8.8.8) must be used → blocked.
	spoof := httptest.NewRequest("GET", "/", nil)
	spoof.RemoteAddr = "127.0.0.1:5000" // the Caddy hop
	spoof.Header.Set("X-Forwarded-For", "10.0.0.1, 8.8.8.8")
	if p.Allow(httptest.NewRecorder(), spoof, "sub") {
		t.Error("spoofed leftmost X-Forwarded-For must not bypass the allowlist")
	}

	// Legitimate internal client: Caddy appends the real 10.1.2.3 → allowed.
	ok := httptest.NewRequest("GET", "/", nil)
	ok.RemoteAddr = "127.0.0.1:5000"
	ok.Header.Set("X-Forwarded-For", "10.1.2.3")
	if !p.Allow(httptest.NewRecorder(), ok, "sub") {
		t.Error("legitimate proxied client must be allowed")
	}
}

func TestNewProtector_Invalid(t *testing.T) {
	if _, err := NewProtector("nopass", "", 0); err == nil {
		t.Error("basic auth without colon must error")
	}
	if _, err := NewProtector("", "not-an-ip", 0); err == nil {
		t.Error("invalid CIDR must error")
	}
	if _, err := NewProtector("", "", -1); err == nil {
		t.Error("negative trusted hops must error")
	}
}
