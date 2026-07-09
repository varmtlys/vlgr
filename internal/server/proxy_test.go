package server

import (
	"net/http/httptest"

	"testing"
	"vlgr/internal/protocol"
)

func TestExtractSubdomain(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		baseDomain string
		expected   string
	}{
		{"basic", "abc123.tunnel.domain.com", "tunnel.domain.com", "abc123"},
		{"with port", "abc123.tunnel.domain.com:443", "tunnel.domain.com", "abc123"},
		{"base with port", "abc123.tunnel.domain.com", "tunnel.domain.com:8080", "abc123"},
		{"both ports", "abc123.tunnel.domain.com:443", "tunnel.domain.com:8080", "abc123"},
		{"localhost", "a1b2.localhost:8080", "localhost:8080", "a1b2"},
		{"single label base", "sub.domain.com", "domain.com", "sub"},
		{"no subdomain", "tunnel.domain.com", "tunnel.domain.com", ""},
		{"wrong domain", "abc.other.com", "tunnel.domain.com", ""},
		{"multi-level subdomain", "deep.sub.tunnel.domain.com", "tunnel.domain.com", ""},
		{"empty host", "", "tunnel.domain.com", ""},
		{"empty base", "abc.tunnel.com", "", ""},
		{"host shorter than base", "a.b", "a.b.c", ""},
		{"host equals base with dot suffix", "tunnel.domain.com.", "tunnel.domain.com", ""},
		{"single char subdomain", "x.tunnel.domain.com", "tunnel.domain.com", "x"},
		{"numeric subdomain", "123.tunnel.domain.com", "tunnel.domain.com", "123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSubdomain(tt.host, tt.baseDomain)
			if got != tt.expected {
				t.Errorf("extractSubdomain(%q, %q): want %q, got %q",
					tt.host, tt.baseDomain, tt.expected, got)
			}
		})
	}
}

func TestExtractSubdomain_PortOnly(t *testing.T) {
	got := extractSubdomain(":443", "tunnel.domain.com")
	if got != "" {
		t.Errorf("expected empty for port-only host, got %q", got)
	}
}

func TestWriteNormalResponse_SecurityDefaults(t *testing.T) {
	rec := httptest.NewRecorder()
	writeNormalResponse(rec, protocol.HTTPResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"text/html"}},
		Body:       []byte("ok"),
	})
	if rec.Code != 200 {
		t.Errorf("status: want 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff default should be applied when app didn't set it")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("X-Frame-Options default should be applied when app didn't set it")
	}
}

func TestWriteNormalResponse_AppHeadersWin(t *testing.T) {
	rec := httptest.NewRecorder()
	writeNormalResponse(rec, protocol.HTTPResponse{
		StatusCode: 200,
		Headers: map[string][]string{
			"X-Frame-Options":        {"SAMEORIGIN"},
			"X-Content-Type-Options": {"nosniff"},
		},
	})
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("app's X-Frame-Options must win, got %q", got)
	}
}

func TestWriteNormalResponse_DropsCRLFHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeNormalResponse(rec, protocol.HTTPResponse{
		StatusCode: 200,
		Headers: map[string][]string{
			"X-Evil":  {"a\r\nInjected: yes"},
			"Bad Key": {"v"},
			"X-Good":  {"fine"},
		},
	})
	if rec.Header().Get("X-Evil") != "" {
		t.Error("CRLF header value must be dropped")
	}
	if rec.Header().Get("Injected") != "" {
		t.Error("injected header must not appear")
	}
	if rec.Header().Get("X-Good") != "fine" {
		t.Error("valid header must pass through")
	}
}
