//go:build windows || linux

package client

import "testing"

func TestTraySlotHref(t *testing.T) {
	cases := []struct {
		name string
		slot *traySlot
		want string
	}{
		{"http forward", &traySlot{active: true, url: "web.example.com", scheme: "http"}, "http://web.example.com"},
		{"tls forward", &traySlot{active: true, url: "web.example.com", scheme: "https"}, "https://web.example.com"},
		{"raw tcp tunnel", &traySlot{active: true, url: "tcp://example.com:20001", scheme: "http"}, ""},
		{"tls passthrough tunnel", &traySlot{active: true, url: "tls://sub.example.com", scheme: "http"}, ""},
		{"inactive slot", &traySlot{url: "web.example.com", scheme: "http"}, ""},
	}
	for _, tc := range cases {
		if got := tc.slot.href(); got != tc.want {
			t.Errorf("%s: href() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
