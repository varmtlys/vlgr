package server

import (
	"testing"
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
		{"single label base", "sub.example.com", "example.com", "sub"},
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
