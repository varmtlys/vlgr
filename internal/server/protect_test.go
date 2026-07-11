package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtector_Disabled(t *testing.T) {
	p, err := NewProtector("", "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if !p.Allow(httptest.NewRecorder(), r, "sub") {
		t.Error("empty protector must allow everything")
	}
}

func TestProtector_BasicAuth(t *testing.T) {
	p, _ := NewProtector("bob:s3cret", "")

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
	p, _ := NewProtector("bob:s3cret", "")
	w := httptest.NewRecorder()
	p.Allow(w, httptest.NewRequest("GET", "/", nil), "sub")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate challenge")
	}
}

func TestProtector_IPAllowlist(t *testing.T) {
	p, err := NewProtector("", "10.0.0.0/8, 192.168.1.5")
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
		r.Header.Set("X-Forwarded-For", ip)
		if got := p.Allow(httptest.NewRecorder(), r, "sub"); got != want {
			t.Errorf("ip %s: allow=%v, want %v", ip, got, want)
		}
	}
}

func TestNewProtector_Invalid(t *testing.T) {
	if _, err := NewProtector("nopass", ""); err == nil {
		t.Error("basic auth without colon must error")
	}
	if _, err := NewProtector("", "not-an-ip"); err == nil {
		t.Error("invalid CIDR must error")
	}
}
