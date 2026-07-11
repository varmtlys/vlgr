package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/client"
	"vlgr/internal/protocol"
	"vlgr/internal/server"
)

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type testBackend struct {
	addr string
	srv  *http.Server
}

func startBackend(t *testing.T) *testBackend {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "vlgr-test")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "hello from %s %s", r.Method, r.URL.Path)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		for k, v := range r.Header {
			if k != "Content-Length" {
				for _, vv := range v {
					w.Header().Add("X-Echo-"+k, vv)
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("im a teapot"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			resp := []byte(fmt.Sprintf("echo: %s", msg))
			if err := conn.WriteMessage(mt, resp); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("slow response"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 1024*100)
		for i := range body {
			body[i] = 'A' + byte(i%26)
		}
		w.Write(body)
	})

	port := freePort()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	time.Sleep(50 * time.Millisecond)
	return &testBackend{addr: addr, srv: srv}
}

func (b *testBackend) port() uint16 {
	var p uint16
	fmt.Sscanf(strings.Split(b.addr, ":")[1], "%d", &p)
	return p
}

func (b *testBackend) close() { b.srv.Close() }

type testVLGRServer struct {
	wsAddr   string
	httpAddr string
	tcpStart uint16
	tcpEnd   uint16
	wsSrv    *http.Server
	httpSrv  *http.Server
}

func startVLGRServer(t *testing.T, baseDomain string, token string) *testVLGRServer {
	t.Helper()
	wsPort := freePort()
	httpPort := freePort()
	wsAddr := fmt.Sprintf("127.0.0.1:%d", wsPort)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	// A small public TCP port range for raw TCP tunnel tests.
	tcpStart := uint16(freePort())
	tcpEnd := tcpStart + 4
	tcpAlloc := server.NewPortAllocator(tcpStart, tcpEnd)

	registry := server.NewRegistry()
	proxy := server.NewReverseProxy(registry, baseDomain, false, nil)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	wsMux := http.NewServeMux()
	wsMux.HandleFunc("/_tunnel", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handler := server.NewClientHandler(conn, registry, baseDomain, token, false, tcpAlloc, "127.0.0.1")
		handler.Run()
	})
	httpMux := http.NewServeMux()
	httpMux.Handle("/", proxy)

	wsSrv := &http.Server{Addr: wsAddr, Handler: wsMux}
	httpSrv := &http.Server{Addr: httpAddr, Handler: httpMux}
	go wsSrv.ListenAndServe()
	go httpSrv.ListenAndServe()
	time.Sleep(50 * time.Millisecond)

	return &testVLGRServer{wsAddr: wsAddr, httpAddr: httpAddr, tcpStart: tcpStart, tcpEnd: tcpEnd, wsSrv: wsSrv, httpSrv: httpSrv}
}

func (s *testVLGRServer) close() {
	s.wsSrv.Close()
	s.httpSrv.Close()
}

func extractSubdomain(publicURL string, baseDomain string) string {
	suffix := "." + baseDomain
	idx := strings.Index(publicURL, suffix)
	if idx < 0 {
		return ""
	}
	return publicURL[:idx]
}

func connectTunnel(t *testing.T, wsAddr, token string, ports []uint16, subs []string) *client.Tunnel {
	t.Helper()
	tun := client.NewTunnel(wsAddr, token, ports, subs, false)
	if err := tun.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { tun.Close() })
	go tun.Run()
	time.Sleep(100 * time.Millisecond)
	return tun
}

func httpDo(t *testing.T, proxyAddr, host, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	url := fmt.Sprintf("http://%s%s", proxyAddr, path)
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxBodySize))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return resp, respBody
}

func httpGet(t *testing.T, proxyAddr, host, path string) (*http.Response, []byte) {
	return httpDo(t, proxyAddr, host, "GET", path, nil, nil)
}

func httpPost(t *testing.T, proxyAddr, host, path string, body []byte, ct string) (*http.Response, []byte) {
	headers := map[string]string{}
	if ct != "" {
		headers["Content-Type"] = ct
	}
	return httpDo(t, proxyAddr, host, "POST", path, body, headers)
}

// ─── Basic tunnel tests ──────────────────────────────────────────────────────

func TestBasicHTTPTunnel(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	resp, body := httpGet(t, srv.httpAddr, sub+".test.local", "/")
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "hello from GET /") {
		t.Errorf("unexpected body: %s", body)
	}
	if resp.Header.Get("X-Backend") != "vlgr-test" {
		t.Errorf("missing X-Backend header")
	}
}

func TestHTTPTunnel_CustomStatus(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	resp, body := httpGet(t, srv.httpAddr, sub+".test.local", "/status")
	if resp.StatusCode != 418 {
		t.Errorf("status: want 418, got %d", resp.StatusCode)
	}
	if string(body) != "im a teapot" {
		t.Errorf("body: want 'im a teapot', got %q", body)
	}
}

func TestHTTPTunnel_MultipleRequests(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")
	host := sub + ".test.local"

	for _, path := range []string{"/", "/echo", "/status", "/", "/status", "/echo"} {
		resp, _ := httpGet(t, srv.httpAddr, host, path)
		if resp.StatusCode < 200 || resp.StatusCode >= 600 {
			t.Errorf("path %s: unexpected status %d", path, resp.StatusCode)
		}
	}
}

func TestHTTPTunnel_ConcurrentRequests(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")
	host := sub + ".test.local"

	const n = 30
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := httpGet(t, srv.httpAddr, host, "/")
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// ─── Auth tests ──────────────────────────────────────────────────────────────

func TestHTTPTunnel_AuthRejected(t *testing.T) {
	srv := startVLGRServer(t, "test.local", "secret-token")
	defer srv.close()

	tun := client.NewTunnel(srv.wsAddr, "wrong-token", []uint16{3000}, nil, false)
	err := tun.Connect()
	if err == nil {
		t.Error("expected auth error with wrong token")
	}
	if !strings.Contains(err.Error(), "authentication rejected") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHTTPTunnel_NoAuth(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	resp, _ := httpGet(t, srv.httpAddr, sub+".test.local", "/")
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

// ─── Error response tests ────────────────────────────────────────────────────

func TestHTTPTunnel_NoTunnel(t *testing.T) {
	srv := startVLGRServer(t, "test.local", "")
	defer srv.close()

	resp, _ := httpGet(t, srv.httpAddr, "nonexistent.test.local", "/")
	if resp.StatusCode != 404 {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

func TestHTTPTunnel_NoSubdomain(t *testing.T) {
	srv := startVLGRServer(t, "test.local", "")
	defer srv.close()

	resp, _ := httpGet(t, srv.httpAddr, "test.local", "/")
	if resp.StatusCode != 400 {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestHTTPTunnel_WrongDomain(t *testing.T) {
	srv := startVLGRServer(t, "test.local", "")
	defer srv.close()

	resp, _ := httpGet(t, srv.httpAddr, "sub.other.com", "/")
	if resp.StatusCode != 400 {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

// ─── POST and body propagation tests ─────────────────────────────────────────

func TestHTTPTunnel_PostEcho(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	payload := []byte(`{"message":"hello tunnel"}`)
	resp, body := httpPost(t, srv.httpAddr, sub+".test.local", "/echo", payload, "application/json")

	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("body mismatch: want %q, got %q", payload, body)
	}
}

func TestHTTPTunnel_HeadersPropagation(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	resp, _ := httpDo(t, srv.httpAddr, sub+".test.local", "POST", "/echo",
		[]byte("data"), map[string]string{"X-Custom": "custom-val", "Authorization": "Bearer abc"})

	if resp.Header.Get("X-Echo-X-Custom") != "custom-val" {
		t.Errorf("X-Custom not propagated, got %q", resp.Header.Get("X-Echo-X-Custom"))
	}
}

// ─── Large body test ─────────────────────────────────────────────────────────

func TestHTTPTunnel_LargeResponse(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	resp, body := httpGet(t, srv.httpAddr, sub+".test.local", "/big")
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if len(body) != 1024*100 {
		t.Errorf("body length: want %d, got %d", 1024*100, len(body))
	}
}

// ─── Slow response test ─────────────────────────────────────────────────────

func TestHTTPTunnel_SlowResponse(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	resp, body := httpGet(t, srv.httpAddr, sub+".test.local", "/slow")
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if string(body) != "slow response" {
		t.Errorf("body: want 'slow response', got %q", body)
	}
}

// ─── Multi-port tunnel tests ─────────────────────────────────────────────────

func TestHTTPTunnel_MultiPort(t *testing.T) {
	backend1 := startBackend(t)
	defer backend1.close()
	backend2 := startBackend(t)
	defer backend2.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend1.port(), backend2.port()}, nil)

	urls := strings.Split(tun.PublicURL(), ", ")
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d: %v", len(urls), urls)
	}

	sub1 := extractSubdomain(urls[0], "test.local")
	sub2 := extractSubdomain(urls[1], "test.local")

	resp1, body1 := httpGet(t, srv.httpAddr, sub1+".test.local", "/")
	resp2, body2 := httpGet(t, srv.httpAddr, sub2+".test.local", "/")

	if resp1.StatusCode != 200 || resp2.StatusCode != 200 {
		t.Errorf("statuses: %d, %d", resp1.StatusCode, resp2.StatusCode)
	}
	if !strings.Contains(string(body1), "hello from GET /") {
		t.Errorf("body1: %s", body1)
	}
	if !strings.Contains(string(body2), "hello from GET /") {
		t.Errorf("body2: %s", body2)
	}
}

// ─── Custom subdomain tests ──────────────────────────────────────────────────

func TestHTTPTunnel_CustomSubdomain(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, []string{"myapp"})
	if tun.PublicURL() != "myapp.test.local" {
		t.Errorf("PublicURL: want myapp.test.local, got %q", tun.PublicURL())
	}

	resp, body := httpGet(t, srv.httpAddr, "myapp.test.local", "/")
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "hello from GET /") {
		t.Errorf("body: %s", body)
	}
}

func TestHTTPTunnel_DuplicateSubdomain(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun1 := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, []string{"duplicate"})
	_ = tun1

	tun2 := client.NewTunnel(srv.wsAddr, "mytoken", []uint16{backend.port()}, []string{"duplicate"}, false)
	err := tun2.Connect()
	if err == nil {
		t.Error("expected duplicate subdomain error")
	}
	if !strings.Contains(err.Error(), "already taken") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ─── Client disconnect tests ─────────────────────────────────────────────────

func TestHTTPTunnel_ClientDisconnect(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	resp, _ := httpGet(t, srv.httpAddr, sub+".test.local", "/")
	if resp.StatusCode != 200 {
		t.Fatalf("initial request failed: %d", resp.StatusCode)
	}

	tun.Close()
	time.Sleep(100 * time.Millisecond)

	resp, _ = httpGet(t, srv.httpAddr, sub+".test.local", "/")
	if resp.StatusCode != 404 {
		t.Errorf("after disconnect: want 404, got %d", resp.StatusCode)
	}
}

// ─── WebSocket relay tests ───────────────────────────────────────────────────

func TestWebSocketRelay_Echo(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	wsURL := fmt.Sprintf("ws://%s/ws", srv.httpAddr)
	dialer := websocket.Dialer{}
	header := http.Header{}
	header.Set("Host", sub+".test.local")
	conn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer conn.Close()

	messages := []string{"hello", "world", "vlgr"}
	for _, msg := range messages {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, resp, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		expected := "echo: " + msg
		if string(resp) != expected {
			t.Errorf("echo: want %q, got %q", expected, resp)
		}
	}
}

func TestWebSocketRelay_Bidirectional(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	wsURL := fmt.Sprintf("ws://%s/ws", srv.httpAddr)
	dialer := websocket.Dialer{}
	header := http.Header{}
	header.Set("Host", sub+".test.local")
	conn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer conn.Close()

	const rounds = 10
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			msg := fmt.Sprintf("ping-%d", i)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		received := 0
		for received < rounds {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			received++
		}
		if received != rounds {
			t.Errorf("received %d of %d messages", received, rounds)
		}
	}()

	wg.Wait()
}

func TestWebSocketRelay_ExternalClose(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	wsURL := fmt.Sprintf("ws://%s/ws", srv.httpAddr)
	dialer := websocket.Dialer{}
	header := http.Header{}
	header.Set("Host", sub+".test.local")
	conn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}

	// send one message to verify relay is working
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read: %v", err)
	}

	// external client closes
	conn.Close()
	time.Sleep(100 * time.Millisecond)

	// tunnel should still be alive for new WS connections
	conn2, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("second WS dial after close: %v", err)
	}
	defer conn2.Close()

	if err := conn2.WriteMessage(websocket.TextMessage, []byte("after-close")); err != nil {
		t.Fatalf("write after close: %v", err)
	}
	if _, _, err := conn2.ReadMessage(); err != nil {
		t.Fatalf("read after close: %v", err)
	}
}

// ─── Subdomain format tests ──────────────────────────────────────────────────

func TestHTTPTunnel_SubdomainWithHyphen(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, []string{"my-app"})
	resp, _ := httpGet(t, srv.httpAddr, "my-app.test.local", "/")
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

// ─── Multiple clients test ───────────────────────────────────────────────────

func TestHTTPTunnel_MultipleClients(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, []string{"client1"})
	connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, []string{"client2"})

	resp1, _ := httpGet(t, srv.httpAddr, "client1.test.local", "/")
	if resp1.StatusCode != 200 {
		t.Errorf("client1: %d", resp1.StatusCode)
	}
	resp2, _ := httpGet(t, srv.httpAddr, "client2.test.local", "/")
	if resp2.StatusCode != 200 {
		t.Errorf("client2: %d", resp2.StatusCode)
	}
}

// ─── Streamed body tests ─────────────────────────────────────────────────────

func TestHTTPTunnel_StreamedBodies(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	// 10MB body: above InlineBodyLimit, so both the upload and the echoed
	// response travel as MsgStreamData frames instead of inline payloads.
	payload := make([]byte, 10<<20)
	for i := range payload {
		payload[i] = byte(i * 31)
	}

	resp, body := httpPost(t, srv.httpAddr, sub+".test.local", "/echo", payload, "application/octet-stream")
	if resp.StatusCode != 200 {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if len(body) != len(payload) {
		t.Fatalf("echoed body length: want %d, got %d", len(payload), len(body))
	}
	if !bytes.Equal(body, payload) {
		t.Error("echoed body does not match uploaded payload")
	}
}

func TestHTTPTunnel_InlineBodyStillInline(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	tun := connectTunnel(t, srv.wsAddr, "mytoken", []uint16{backend.port()}, nil)
	sub := extractSubdomain(tun.PublicURL(), "test.local")

	// Just under the inline limit — must work exactly as before.
	payload := bytes.Repeat([]byte("x"), int(protocol.InlineBodyLimit)-1)
	resp, body := httpPost(t, srv.httpAddr, sub+".test.local", "/echo", payload, "text/plain")
	if resp.StatusCode != 200 {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if !bytes.Equal(body, payload) {
		t.Error("echoed inline body does not match")
	}
}

// ─── Raw TCP tunnel tests ────────────────────────────────────────────────────

// startTCPEcho starts a local TCP echo server and returns its port.
func startTCPEcho(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	var p uint16
	fmt.Sscanf(strings.Split(ln.Addr().String(), ":")[1], "%d", &p)
	return p
}

// connectTCPTunnel connects a client that exposes localPort as a raw TCP
// tunnel and returns the assigned public port.
func connectTCPTunnel(t *testing.T, wsAddr, token string, localPort uint16) uint16 {
	t.Helper()
	tun := client.NewTunnel(wsAddr, token, nil, nil, false)
	tun.SetTCPForwards([]client.TCPForward{{LocalPort: localPort}})
	if err := tun.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { tun.Close() })
	go tun.Run()
	time.Sleep(100 * time.Millisecond)

	// PublicURL is "tcp://127.0.0.1:<port>".
	url := tun.PublicURL()
	var p uint16
	parts := strings.Split(url, ":")
	fmt.Sscanf(parts[len(parts)-1], "%d", &p)
	if p == 0 {
		t.Fatalf("no public TCP port assigned, got %q", url)
	}
	return p
}

// TestRawTCPTunnel exercises one TCP tunnel (echo backend) across several
// scenarios: a small echo, a large multi-frame transfer, and a second
// connection reusing the same tunnel.
func TestRawTCPTunnel(t *testing.T) {
	echoPort := startTCPEcho(t)

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	pubPort := connectTCPTunnel(t, srv.wsAddr, "mytoken", echoPort)
	addr := fmt.Sprintf("127.0.0.1:%d", pubPort)

	// roundtrip opens a fresh public connection, writes payload and asserts
	// the echo backend returns it unchanged.
	roundtrip := func(t *testing.T, payload []byte, timeout time.Duration) {
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial public TCP port: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(timeout))

		done := make(chan error, 1)
		go func() {
			buf := make([]byte, len(payload))
			_, err := io.ReadFull(conn, buf)
			if err == nil && !bytes.Equal(buf, payload) {
				err = fmt.Errorf("payload mismatch")
			}
			done <- err
		}()
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("roundtrip: %v", err)
		}
	}

	t.Run("echo", func(t *testing.T) {
		roundtrip(t, []byte("hello raw tcp tunnel"), 3*time.Second)
	})
	t.Run("large multi-frame transfer", func(t *testing.T) {
		roundtrip(t, bytes.Repeat([]byte("abcdefgh"), 200*1024), 10*time.Second) // 1.6 MB
	})
	t.Run("second connection on same tunnel", func(t *testing.T) {
		roundtrip(t, []byte("second connection over the same tunnel"), 3*time.Second)
	})
}

// ─── Traffic inspector dashboard tests ───────────────────────────────────────

func dashList(t *testing.T, addr string) []map[string]any {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/api/requests")
	if err != nil {
		t.Fatalf("dashboard list: %v", err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode dashboard list: %v", err)
	}
	return out
}

func TestInspectorDashboard_RecordAndReplay(t *testing.T) {
	backend := startBackend(t)
	defer backend.close()

	srv := startVLGRServer(t, "test.local", "mytoken")
	defer srv.close()

	dashAddr := fmt.Sprintf("127.0.0.1:%d", freePort())
	dash := client.NewDashboard(dashAddr, 1000)
	if err := dash.Start(); err != nil {
		t.Fatalf("dashboard start: %v", err)
	}
	t.Cleanup(dash.Stop)

	tun := client.NewTunnel(srv.wsAddr, "mytoken", []uint16{backend.port()}, nil, false)
	tun.SetDashboard(dash)
	if err := tun.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { tun.Close() })
	go tun.Run()
	time.Sleep(100 * time.Millisecond)

	host := extractSubdomain(tun.PublicURL(), "test.local") + ".test.local"

	// Drive a request through the tunnel; the inspector must record it.
	httpPost(t, srv.httpAddr, host, "/echo", []byte("recorded-body"), "text/plain")
	time.Sleep(100 * time.Millisecond)

	entries := dashList(t, dashAddr)
	if len(entries) < 1 {
		t.Fatalf("expected >=1 recorded request, got %d", len(entries))
	}
	if entries[0]["method"] != "POST" || entries[0]["path"] != "/echo" {
		t.Errorf("unexpected recorded entry: %+v", entries[0])
	}
	id := int(entries[0]["id"].(float64))

	// Replay re-issues the request to the local app and records a new entry.
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/api/replay/%d", dashAddr, id), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("replay status: want 204, got %d", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)

	entries = dashList(t, dashAddr)
	if len(entries) < 2 {
		t.Fatalf("expected replay to add an entry, got %d", len(entries))
	}
	if entries[0]["replayed"] != true {
		t.Errorf("newest entry should be a replay: %+v", entries[0])
	}
}
