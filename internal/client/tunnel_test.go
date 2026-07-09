package client

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"vlgr/internal/protocol"
)

func TestNewTunnel(t *testing.T) {
	tun := NewTunnel("domain.com:4443", "token123", []uint16{3000}, nil, true)
	if tun.serverAddr != "domain.com:4443" {
		t.Errorf("serverAddr: want domain.com:4443, got %q", tun.serverAddr)
	}
	if tun.token != "token123" {
		t.Errorf("token: want token123, got %q", tun.token)
	}
	if tun.useTLS != true {
		t.Error("useTLS should be true")
	}
	if len(tun.ports) != 1 || tun.ports[0] != 3000 {
		t.Errorf("ports: want [3000], got %v", tun.ports)
	}
	if tun.PublicURL() != "" {
		t.Errorf("PublicURL should be empty initially, got %q", tun.PublicURL())
	}
}

func TestTunnel_SetDebug(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	if tun.debug {
		t.Error("debug should be off by default")
	}
	tun.SetDebug(true)
	if !tun.debug {
		t.Error("debug should be on after SetDebug(true)")
	}
	tun.SetDebug(false)
	if tun.debug {
		t.Error("debug should be off after SetDebug(false)")
	}
}

func TestPortFor_Found(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	tun.mappings[42] = 3000
	port, ok := tun.portFor(42)
	if !ok {
		t.Error("portFor should return true for known ID")
	}
	if port != 3000 {
		t.Errorf("port: want 3000, got %d", port)
	}
}

func TestPortFor_NotFound(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	_, ok := tun.portFor(999)
	if ok {
		t.Error("portFor should return false for unknown ID")
	}
}

func TestSendHTTPError_NoConnection(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when writing without connection")
		}
	}()
	tun.sendHTTPError(1, 100, 502, "test error")
}

func TestWriteResponse_NoConnection(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when writing without connection")
		}
	}()
	resp := protocol.HTTPResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"X-Test": {"value"}},
		Body:       []byte("ok"),
	}
	tun.writeResponse(1, 100, resp)
}

func TestHandleStreamData_NoRelay(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	tun.handleStreamData(protocol.Frame{RequestID: 999, Payload: []byte("data")})
}

func TestHandleStreamClose_NoRelay(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	tun.handleStreamClose(protocol.Frame{RequestID: 999})
}

func TestHandleHTTPReq_DecodeError(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	tun.mappings[1] = 3000
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic when writing without connection")
			}
			close(done)
		}()
		tun.handleHTTPReq(protocol.Frame{
			TunnelID:  1,
			RequestID: 1,
			Payload:   []byte{0x00, 0x01},
		})
	}()
	<-done
}

func TestHTTPClient_Configuration(t *testing.T) {
	// No global Timeout — it would abort streamed transfers — but response
	// headers must still be bounded.
	if httpClient.Timeout != 0 {
		t.Error("httpClient must not have a global timeout (breaks streamed bodies)")
	}
	tr, ok := httpClient.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout == 0 {
		t.Error("httpClient transport should bound response header time")
	}
}

func TestConnect_InvalidServer(t *testing.T) {
	tun := NewTunnel("255.255.255.255:9999", "token", []uint16{3000}, nil, false)
	err := tun.Connect()
	if err == nil {
		t.Error("expected error connecting to invalid server")
	}
}

func TestTunnel_Close(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	closed := make(chan struct{})
	go func() {
		<-tun.done
		close(closed)
	}()
	tun.Close()
	<-closed
}

func TestHandleNormalHTTPReq_UnknownTunnel(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	req := protocol.HTTPRequest{
		Method:  "GET",
		Path:    "/",
		Headers: map[string][]string{},
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic when writing without connection")
			}
			close(done)
		}()
		tun.handleNormalHTTPReq(999, 1, req)
	}()
	<-done
}

func TestHandleWebSocketReq_UnknownTunnel(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	req := protocol.HTTPRequest{
		Method:  "GET",
		Path:    "/ws",
		Headers: map[string][]string{},
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic when writing without connection")
			}
			close(done)
		}()
		tun.handleWebSocketReq(999, 1, req, func() {})
	}()
	<-done
}

func TestHandleWebSocketReq_DialError(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	tun.mappings[1] = 65535
	req := protocol.HTTPRequest{
		Method:  "GET",
		Path:    "/ws",
		Headers: map[string][]string{},
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic when writing without connection")
			}
			close(done)
		}()
		tun.handleWebSocketReq(1, 1, req, func() {})
	}()
	<-done
}

func TestHTTPRequest_HeaderFiltering(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	tun.mappings[1] = 3000

	req := protocol.HTTPRequest{
		Method: "POST",
		Path:   "/api",
		Headers: map[string][]string{
			"Content-Length":    {"100"},
			"Transfer-Encoding": {"chunked"},
			"X-Custom":          {"test"},
		},
		Body: []byte("data"),
	}

	httpReq, err := http.NewRequest(req.Method, "http://localhost:3000/api", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, values := range req.Headers {
		if k == "Content-Length" || k == "Transfer-Encoding" {
			continue
		}
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}

	if httpReq.Header.Get("Content-Length") != "" {
		t.Error("Content-Length should be filtered out")
	}
	if httpReq.Header.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding should be filtered out")
	}
	if httpReq.Header.Get("X-Custom") != "test" {
		t.Error("X-Custom should be present")
	}
}

func TestWriteFrame_NoConnection(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when writing without connection")
		}
	}()
	tun.writeFrame(protocol.Frame{
		Type:      protocol.MsgCloseTunnel,
		TunnelID:  0,
		RequestID: 0,
	})
}

func TestEncodeDecodeHTTPResponse_Client(t *testing.T) {
	original := protocol.HTTPResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"ok":true}`),
	}
	encoded, _ := protocol.EncodeHTTPResponse(original)
	decoded, err := protocol.DecodeHTTPResponse(encoded)
	if err != nil {
		t.Fatalf("DecodeHTTPResponse: %v", err)
	}
	if decoded.StatusCode != 200 {
		t.Errorf("StatusCode: want 200, got %d", decoded.StatusCode)
	}
	if string(decoded.Body) != `{"ok":true}` {
		t.Errorf("Body: want %q, got %q", `{"ok":true}`, decoded.Body)
	}
}

func TestIsWebSocketUpgradeReq_Client(t *testing.T) {
	req := protocol.HTTPRequest{
		Headers: map[string][]string{"Upgrade": {"websocket"}},
	}
	if !protocol.IsWebSocketUpgradeReq(req) {
		t.Error("should detect WebSocket upgrade")
	}

	req2 := protocol.HTTPRequest{
		Headers: map[string][]string{"Host": {"domain.com"}},
	}
	if protocol.IsWebSocketUpgradeReq(req2) {
		t.Error("should not detect WebSocket upgrade")
	}
}

func TestRegisterPayload_Encoding(t *testing.T) {
	port := uint16(8080)
	subdomain := "myapp"

	regPayload := make([]byte, 2)
	binary.BigEndian.PutUint16(regPayload, port)
	regPayload = append(regPayload, byte(len(subdomain)))
	regPayload = append(regPayload, []byte(subdomain)...)

	if len(regPayload) != 2+1+len(subdomain) {
		t.Errorf("payload length: want %d, got %d", 2+1+len(subdomain), len(regPayload))
	}
	if regPayload[2] != byte(len(subdomain)) {
		t.Errorf("subdomain length byte: want %d, got %d", len(subdomain), regPayload[2])
	}
	if string(regPayload[3:]) != subdomain {
		t.Errorf("subdomain: want %q, got %q", subdomain, regPayload[3:])
	}
}

func TestStreamRelay_CloseDone_Idempotent(t *testing.T) {
	r := &streamRelay{
		done: make(chan struct{}),
	}
	r.closeDone()
	r.closeDone()
	r.closeDone()
	select {
	case <-r.done:
	default:
		t.Fatal("done should be closed")
	}
}

func TestStreamRelay_CloseDone_RaceNoPanic(t *testing.T) {
	r := &streamRelay{
		done: make(chan struct{}),
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			r.closeDone()
		}()
	}
	wg.Wait()
}

func TestTunnel_Close_ClosesAllRelays(t *testing.T) {
	tun := NewTunnel("x", "", nil, nil, false)
	tun.relays[1] = &streamRelay{
		conn: newTestConn(),
		done: make(chan struct{}),
	}
	tun.relays[2] = &streamRelay{
		conn: newTestConn(),
		done: make(chan struct{}),
	}
	tun.Close()
	for id, r := range tun.relays {
		select {
		case <-r.done:
		default:
			t.Errorf("relay %d done should be closed", id)
		}
	}
}

type testConn struct {
	closed bool
	mu     sync.Mutex
}

var errTestClosed = &testNetError{s: "use of closed network connection"}

type testNetError struct{ s string }

func (e *testNetError) Error() string   { return e.s }
func (e *testNetError) Timeout() bool   { return false }
func (e *testNetError) Temporary() bool { return false }

func newTestConn() *testConn { return &testConn{} }

func (c *testConn) Read(b []byte) (int, error) {
	return 0, &net.OpError{Op: "read", Err: errTestClosed}
}

func (c *testConn) Write(b []byte) (int, error) {
	return 0, &net.OpError{Op: "write", Err: errTestClosed}
}
func (c *testConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *testConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *testConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *testConn) SetDeadline(t time.Time) error      { return nil }
func (c *testConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *testConn) SetWriteDeadline(t time.Time) error { return nil }

func TestContentLength(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string][]string
		want    int64
	}{
		{"present", map[string][]string{"Content-Length": {"12345"}}, 12345},
		{"case insensitive", map[string][]string{"content-length": {"7"}}, 7},
		{"absent", map[string][]string{}, -1},
		{"garbage", map[string][]string{"Content-Length": {"abc"}}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contentLength(tt.headers); got != tt.want {
				t.Errorf("want %d, got %d", tt.want, got)
			}
		})
	}
}

func TestBodyStreamReader_ReadAndEOF(t *testing.T) {
	bs := &bodyStream{ch: make(chan []byte, 4)}
	bs.ch <- []byte("hello ")
	bs.ch <- []byte("world")
	close(bs.ch)

	r := &bodyStreamReader{s: bs, done: make(chan struct{})}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("want %q, got %q", "hello world", data)
	}
}

func TestBodyStreamReader_Aborted(t *testing.T) {
	bs := &bodyStream{ch: make(chan []byte, 1)}
	bs.aborted.Store(true)
	close(bs.ch)

	r := &bodyStreamReader{s: bs, done: make(chan struct{})}
	if _, err := io.ReadAll(r); err == nil {
		t.Error("aborted stream must surface an error, not clean EOF")
	}
}

func TestBodyStreamReader_TunnelClosed(t *testing.T) {
	bs := &bodyStream{ch: make(chan []byte)}
	done := make(chan struct{})
	close(done)

	r := &bodyStreamReader{s: bs, done: done}
	if _, err := r.Read(make([]byte, 8)); err == nil {
		t.Error("closed tunnel must unblock the reader with an error")
	}
}
