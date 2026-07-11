package sshtun

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeAlloc is a trivial PortAllocator that lets the OS pick ports by always
// binding :0 — but the SSH server binds by number, so we hand out a real free
// port grabbed up front.
type fakeAlloc struct {
	mu   sync.Mutex
	port uint16
}

func (f *fakeAlloc) Allocate(preferred uint16) (uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.port == 0 {
		return 0, fmt.Errorf("exhausted")
	}
	p := f.port
	f.port = 0
	return p, nil
}
func (f *fakeAlloc) Release(uint16) {}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func TestSSH_RemoteForward(t *testing.T) {
	pubPort := freePort(t)
	srv, err := New("127.0.0.1:0", "s3cret", "", &fakeAlloc{port: pubPort})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	// Dial the SSH server as a stock client and request a remote forward.
	cfg := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("s3cret")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	client, err := ssh.Dial("tcp", srv.ln.Addr().String(), cfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer client.Close()

	// ssh -R <pubPort>:... — the client listens; the library dials back to it.
	rl, err := client.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", pubPort))
	if err != nil {
		t.Fatalf("remote listen: %v", err)
	}
	defer rl.Close()

	go func() {
		conn, err := rl.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn) // echo
	}()

	// Hit the public port; bytes should round-trip through the SSH channel.
	pc, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", pubPort))
	if err != nil {
		t.Fatalf("dial public port: %v", err)
	}
	defer pc.Close()
	pc.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := pc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(pc, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo: want ping, got %q", buf)
	}
}

func TestSSH_WrongPassword(t *testing.T) {
	srv, err := New("127.0.0.1:0", "s3cret", "", &fakeAlloc{port: freePort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	cfg := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("wrong")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	if _, err := ssh.Dial("tcp", srv.ln.Addr().String(), cfg); err == nil {
		t.Error("expected auth failure with wrong password")
	}
}
