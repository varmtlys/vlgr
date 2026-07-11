package server

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// captureClientHello returns the raw bytes of a real TLS ClientHello carrying
// the given SNI, produced by the stdlib TLS client.
func captureClientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	c, s := net.Pipe()
	defer s.Close()

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		s.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := s.Read(buf)
		got <- buf[:n]
	}()

	client := tls.Client(c, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	// Handshake will stall on the pipe; we only need the ClientHello to be sent.
	go client.Handshake()

	select {
	case b := <-got:
		c.Close()
		return b
	case <-time.After(2 * time.Second):
		c.Close()
		t.Fatal("timed out capturing ClientHello")
		return nil
	}
}

func TestExtractSNI(t *testing.T) {
	hello := captureClientHello(t, "app.tunnel.example.com")
	name, ok := extractSNI(hello)
	if !ok {
		t.Fatal("expected to extract SNI")
	}
	if name != "app.tunnel.example.com" {
		t.Errorf("SNI: want app.tunnel.example.com, got %q", name)
	}
}

func TestExtractSNI_NotHandshake(t *testing.T) {
	if _, ok := extractSNI([]byte{0x17, 0x03, 0x03, 0x00, 0x01, 0x00}); ok {
		t.Error("non-handshake record must not yield SNI")
	}
	if _, ok := extractSNI(nil); ok {
		t.Error("empty input must not yield SNI")
	}
}

func TestExtractSNI_Incomplete(t *testing.T) {
	hello := captureClientHello(t, "app.tunnel.example.com")
	if _, ok := extractSNI(hello[:20]); ok {
		t.Error("truncated ClientHello must not yield SNI")
	}
}
