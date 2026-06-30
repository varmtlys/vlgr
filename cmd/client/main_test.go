package main

import "testing"

func TestValidateServerAddr(t *testing.T) {
	good := []string{"localhost:4443", "example.com:443", "127.0.0.1:8080", "sub.example.com:443", "a:1"}
	for _, s := range good {
		if err := validateServerAddr(s); err != nil {
			t.Errorf("validateServerAddr(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{
		"",
		"example.com:443@evil.com",
		"https://example.com:443",
		"example.com",
		"example.com:abc",
		"example.com:0",
		"example.com:99999",
		"example.com:-1",
		"exam ple.com:443",
		"example.com:443/extra",
		"example.com:443?x=1",
		"http://example.com:443",
		"wss://example.com:443",
	}
	for _, s := range bad {
		if err := validateServerAddr(s); err == nil {
			t.Errorf("validateServerAddr(%q) = nil, want error", s)
		}
	}
}
