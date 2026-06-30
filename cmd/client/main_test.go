package main

import "testing"

func TestValidateServerAddr(t *testing.T) {
	good := []string{"localhost:4443", "domain.com:443", "127.0.0.1:8080", "sub.domain.com:443", "a:1"}
	for _, s := range good {
		if err := validateServerAddr(s); err != nil {
			t.Errorf("validateServerAddr(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{
		"",
		"domain.com:443@evil.com",
		"https://domain.com:443",
		"domain.com",
		"domain.com:abc",
		"domain.com:0",
		"domain.com:99999",
		"domain.com:-1",
		"exam ple.com:443",
		"domain.com:443/extra",
		"domain.com:443?x=1",
		"http://domain.com:443",
		"wss://domain.com:443",
	}
	for _, s := range bad {
		if err := validateServerAddr(s); err == nil {
			t.Errorf("validateServerAddr(%q) = nil, want error", s)
		}
	}
}
