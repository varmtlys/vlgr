package server

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"testing"

	"vlgr/internal/protocol"
)

func TestNextRequestID_Concurrent(t *testing.T) {
	requestIDCounter = 0
	var results [100]uint64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = nextRequestID()
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]bool)
	for _, id := range results {
		if id == 0 {
			t.Error("request ID should not be 0")
		}
		if seen[id] {
			t.Errorf("duplicate request ID: %d", id)
		}
		seen[id] = true
	}
}

func TestNextRequestID_Monotonic(t *testing.T) {
	requestIDCounter = 0
	prev := nextRequestID()
	for i := 0; i < 1000; i++ {
		next := nextRequestID()
		if next <= prev {
			t.Errorf("not monotonic: %d <= %d", next, prev)
		}
		prev = next
	}
}

func TestHandleRegister_InvalidPayload(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "tunnel.domain.com",
		done:          make(chan struct{}),
	}

	// payload too short
	h.handleRegister(protocol.Frame{Payload: []byte{0x01}})
}

func TestHandleRegister_PortZero(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "tunnel.domain.com",
		done:          make(chan struct{}),
	}

	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, 0)
	h.handleRegister(protocol.Frame{Payload: payload})
}

func TestHandleRegister_SubdomainTooLong(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "tunnel.domain.com",
		done:          make(chan struct{}),
	}

	payload := make([]byte, 3)
	binary.BigEndian.PutUint16(payload, 3000)
	payload[2] = 64
	h.handleRegister(protocol.Frame{Payload: payload})
}

func TestHandleRegister_PublicURLTooLong(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "very-long-base-domain-name-that-makes-url-exceed-255-chars-total." +
			"very-long-base-domain-name-that-makes-url-exceed-255-chars-total." +
			"very-long-base-domain-name-that-makes-url-exceed-255-chars-total." +
			"very-long-base-domain-name-that-makes-url-exceed-255-chars-total.com",
		done: make(chan struct{}),
	}

	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, 3000)
	h.handleRegister(protocol.Frame{Payload: payload})

	if h.registry.Get(generateSubdomain()) != nil {
		t.Error("tunnel should not be registered when URL is too long")
	}
}

func TestHandleRegister_Success(t *testing.T) {
	requestIDCounter = 0
	r := NewRegistry()
	h := &ClientHandler{
		authenticated: true,
		registry:      r,
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "tunnel.domain.com",
		done:          make(chan struct{}),
	}

	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, 3000)
	h.handleRegister(protocol.Frame{Payload: payload})

	if len(h.tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(h.tunnels))
	}
	for _, tun := range h.tunnels {
		if r.Get(tun.Subdomain) == nil {
			t.Error("tunnel not found in registry")
		}
	}
}

func TestHandleRegister_WithSubdomain(t *testing.T) {
	requestIDCounter = 0
	r := NewRegistry()
	h := &ClientHandler{
		authenticated: true,
		registry:      r,
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "tunnel.domain.com",
		done:          make(chan struct{}),
	}

	sub := "myapp"
	payload := make([]byte, 2+1+len(sub))
	binary.BigEndian.PutUint16(payload, 8080)
	payload[2] = byte(len(sub))
	copy(payload[3:], sub)

	h.handleRegister(protocol.Frame{Payload: payload})

	if r.Get("myapp") == nil {
		t.Error("tunnel 'myapp' not found in registry")
	}
}

func TestHandleAuth_InvalidToken(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		expectedToken: "secret",
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}
	h.expectedHash = sha256.Sum256([]byte("secret"))

	h.handleAuth(protocol.Frame{Payload: []byte("wrong")})
	if h.authenticated {
		t.Error("should not authenticate with wrong token")
	}
}

func TestHandleAuth_ValidToken(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		expectedToken: "secret",
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}
	h.expectedHash = sha256.Sum256([]byte("secret"))

	h.handleAuth(protocol.Frame{Payload: []byte("secret")})
	if !h.authenticated {
		t.Error("should authenticate with correct token")
	}
}

func TestHandleAuth_EmptyToken(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		expectedToken: "",
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	h.handleAuth(protocol.Frame{Payload: []byte("anything")})
	if !h.authenticated {
		t.Error("should authenticate with empty expected token")
	}
}

func TestHandleFrame_RejectBeforeAuth(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		registry: NewRegistry(),
		tunnels:  make(map[uint64]*Tunnel),
		pending:  make(map[uint64]*pendingReq),
		done:     make(chan struct{}),
	}

	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, 3000)
	h.handleFrame(protocol.Frame{
		Type:    protocol.MsgRegister,
		Payload: payload,
	})

	if len(h.tunnels) != 0 {
		t.Error("should reject register before auth")
	}
}

func TestHandleFrame_UnknownType(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	h.handleFrame(protocol.Frame{Type: 0xFF})
}

func TestForwardHTTP_WriteError(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	req := protocol.HTTPRequest{
		Method:  "GET",
		Path:    "/test",
		Headers: map[string][]string{},
	}

	_, _, cleanup, err := h.ForwardHTTP(1, req, nil)
	if err == nil {
		t.Error("expected write error (no connection)")
	}
	if cleanup != nil {
		cleanup()
	}
}

func TestCleanup_ClosesAllPending(t *testing.T) {
	requestIDCounter = 0
	r := NewRegistry()
	h := &ClientHandler{
		authenticated: true,
		registry:      r,
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	pr := &pendingReq{
		response:   make(chan protocol.HTTPResponse, 1),
		streamData: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	h.pending[1] = pr
	h.pending[2] = &pendingReq{
		response: make(chan protocol.HTTPResponse, 1),
		done:     make(chan struct{}),
	}

	h.cleanup()

	select {
	case _, ok := <-pr.streamData:
		if ok {
			t.Error("streamData should be closed")
		}
	default:
		t.Error("streamData should be closed")
	}

	select {
	case <-pr.done:
	default:
		t.Error("done should be closed")
	}

	if len(h.pending) != 0 {
		t.Errorf("pending should be empty, got %d entries", len(h.pending))
	}
}

func TestHandleHTTPRes_NoPending(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	resp := protocol.HTTPResponse{StatusCode: 200}
	payload := protocol.EncodeHTTPResponse(resp)

	h.handleHTTPRes(protocol.Frame{
		RequestID: 999,
		Payload:   payload,
	})
}

func TestHandleStreamData_NoPending(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	h.handleStreamData(protocol.Frame{
		RequestID: 999,
		Payload:   []byte("data"),
	})
}

func TestHandleStreamClose_NoPending(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	h.handleStreamClose(protocol.Frame{RequestID: 999})
}

func TestHandleStreamData_NotStreamPending(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	pr := &pendingReq{
		response: make(chan protocol.HTTPResponse, 1),
		done:     make(chan struct{}),
	}
	h.pending[1] = pr

	h.handleStreamData(protocol.Frame{
		RequestID: 1,
		Payload:   []byte("data"),
	})
}

func TestForwardHTTP_Timeout(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	req := protocol.HTTPRequest{
		Method:  "GET",
		Path:    "/timeout",
		Headers: map[string][]string{},
	}

	// ForwardHTTP will fail because no connection exists (write error)
	_, _, _, err := h.ForwardHTTP(1, req, nil)
	if err == nil {
		t.Error("expected write error (no connection)")
	}
}

func TestHandleHTTPRes_WithPending(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	pr := &pendingReq{
		response: make(chan protocol.HTTPResponse, 1),
		done:     make(chan struct{}),
	}
	h.pending[1] = pr

	resp := protocol.HTTPResponse{StatusCode: 200, Body: []byte("ok")}
	payload := protocol.EncodeHTTPResponse(resp)

	go h.handleHTTPRes(protocol.Frame{RequestID: 1, Payload: payload})

	got := <-pr.response
	if got.StatusCode != 200 {
		t.Errorf("StatusCode: want 200, got %d", got.StatusCode)
	}
}

func TestHandleHTTPRes_WithPending_Done(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	pr := &pendingReq{
		response: make(chan protocol.HTTPResponse, 1),
		done:     make(chan struct{}),
	}
	close(pr.done)
	h.pending[1] = pr

	resp := protocol.HTTPResponse{StatusCode: 200}
	payload := protocol.EncodeHTTPResponse(resp)

	h.handleHTTPRes(protocol.Frame{RequestID: 1, Payload: payload})
}

func TestHandleStreamData_WithPending(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	pr := &pendingReq{
		streamData: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	h.pending[1] = pr

	go h.handleStreamData(protocol.Frame{RequestID: 1, Payload: []byte("hello")})

	got := <-pr.streamData
	if string(got) != "hello" {
		t.Errorf("streamData: want %q, got %q", "hello", got)
	}
}

func TestHandleStreamData_WithPending_Done(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	pr := &pendingReq{
		streamData: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	close(pr.done)
	h.pending[1] = pr

	h.handleStreamData(protocol.Frame{RequestID: 1, Payload: []byte("dropped")})
}

func TestHandleStreamClose_WithPending(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	pr := &pendingReq{
		streamData: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	h.pending[1] = pr

	h.handleStreamClose(protocol.Frame{RequestID: 1})

	select {
	case _, ok := <-pr.streamData:
		if ok {
			t.Error("streamData should be closed")
		}
	default:
		t.Error("streamData should be closed")
	}
}

func TestHandleFrame_CloseTunnel(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	h.handleFrame(protocol.Frame{Type: protocol.MsgCloseTunnel})

	select {
	case <-h.done:
	default:
		t.Error("done should be closed after cleanup")
	}
}

func TestHandleHTTPRes_DecodeError(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	h.handleHTTPRes(protocol.Frame{
		RequestID: 1,
		Payload:   []byte{0xFF, 0xFF},
	})
}

func TestHandleRegister_SubdomainEmpty(t *testing.T) {
	requestIDCounter = 0
	r := NewRegistry()
	h := &ClientHandler{
		authenticated: true,
		registry:      r,
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "tunnel.domain.com",
		done:          make(chan struct{}),
	}

	payload := make([]byte, 3)
	binary.BigEndian.PutUint16(payload, 3000)
	payload[2] = 0
	h.handleRegister(protocol.Frame{Payload: payload})

	if len(h.tunnels) != 1 {
		t.Errorf("expected 1 tunnel, got %d", len(h.tunnels))
	}
}

func TestForwardHTTP_Success(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	req := protocol.HTTPRequest{
		Method:  "GET",
		Path:    "/test",
		Headers: map[string][]string{},
	}

	// Will fail because no connection, but tests the write error path
	_, _, _, err := h.ForwardHTTP(1, req, nil)
	if err == nil {
		t.Error("expected write error")
	}
}

func TestForwardHTTP_TimeoutPath(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}

	// ForwardHTTP fails on write error, which tests the error cleanup path
	_, _, cleanup, err := h.ForwardHTTP(1, protocol.HTTPRequest{
		Method:  "GET",
		Path:    "/",
		Headers: map[string][]string{},
	}, nil)
	if err == nil {
		t.Error("expected error")
	}
	if cleanup != nil {
		t.Error("cleanup should be nil on error")
	}
	if len(h.pending) != 0 {
		t.Error("pending should be empty after error cleanup")
	}
}

func TestPendingReq_DoubleCloseDone_NoPanic(t *testing.T) {
	pr := &pendingReq{
		response: make(chan protocol.HTTPResponse, 1),
		done:     make(chan struct{}),
	}
	pr.closeDone()
	pr.closeDone()
	pr.closeDone()
}

func TestPendingReq_DoubleCloseStream_NoPanic(t *testing.T) {
	pr := &pendingReq{
		streamData: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	pr.closeStream()
	pr.closeStream()
}

func TestCleanup_DoesNotPanicOnDoubleCall(t *testing.T) {
	requestIDCounter = 0
	h := &ClientHandler{
		authenticated: true,
		registry:      NewRegistry(),
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}
	pr := &pendingReq{
		streamData: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	h.pending[1] = pr

	h.cleanup()
	h.cleanup()
}

func TestHandleRegister_MaxTunnelsPerClient(t *testing.T) {
	requestIDCounter = 0
	r := NewRegistry()
	h := &ClientHandler{
		authenticated: true,
		registry:      r,
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "test.local",
		done:          make(chan struct{}),
	}
	for i := 0; i < maxTunnelsPerClient; i++ {
		payload := make([]byte, 2)
		payload[0] = byte(3000 + i >> 8)
		payload[1] = byte(3000 + i)
		h.handleRegister(protocol.Frame{Payload: payload})
	}
	if len(h.tunnels) != maxTunnelsPerClient {
		t.Fatalf("expected %d tunnels, got %d", maxTunnelsPerClient, len(h.tunnels))
	}
	payload := make([]byte, 2)
	payload[0] = 0
	payload[1] = 99
	h.handleRegister(protocol.Frame{Payload: payload})
	if len(h.tunnels) != maxTunnelsPerClient {
		t.Errorf("tunnel count exceeded: got %d, want %d", len(h.tunnels), maxTunnelsPerClient)
	}
}

func TestHandleRegister_RejectsBadSubdomain(t *testing.T) {
	requestIDCounter = 0
	r := NewRegistry()
	h := &ClientHandler{
		authenticated: true,
		registry:      r,
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		baseDomain:    "test.local",
		done:          make(chan struct{}),
	}
	for _, bad := range []string{"a..b", "a/b", "a b", "evil;inject", "a$b"} {
		payload := make([]byte, 2+1+len(bad))
		payload[0] = 0
		payload[1] = 80
		payload[2] = byte(len(bad))
		copy(payload[3:], bad)
		before := len(h.tunnels)
		h.handleRegister(protocol.Frame{Payload: payload})
		if len(h.tunnels) != before {
			t.Errorf("subdomain %q should be rejected, but registered", bad)
		}
	}
}

func TestValidSubdomain(t *testing.T) {
	good := []string{"", "abc", "a-b-c", "abc123", "MyApp", "xn--abc"}
	for _, s := range good {
		if !validSubdomain(s) {
			t.Errorf("validSubdomain(%q) = false, want true", s)
		}
	}
	bad := []string{"a..b", "a/b", "a b", "evil;inject", "a$b", "a@b", "a\nb"}
	for _, s := range bad {
		if validSubdomain(s) {
			t.Errorf("validSubdomain(%q) = true, want false", s)
		}
	}
}

func TestValidHeaderValue(t *testing.T) {
	if !validHeaderValue("plain text") {
		t.Error("plain text should be valid")
	}
	if validHeaderValue("with\rCRLF") {
		t.Error("CRLF must be invalid")
	}
	if validHeaderValue("with\nLF") {
		t.Error("LF must be invalid")
	}
	if validHeaderValue("with\x00null") {
		t.Error("NUL must be invalid")
	}
}

func TestValidHeaderName(t *testing.T) {
	if !validHeaderName("Content-Type") {
		t.Error("Content-Type should be valid")
	}
	if validHeaderName("a b") {
		t.Error("space must be invalid")
	}
	if validHeaderName("a:b") {
		t.Error("colon must be invalid")
	}
}

func TestCleanupRace_DoneAndStream_NoPanic(t *testing.T) {
	requestIDCounter = 0
	r := NewRegistry()
	h := &ClientHandler{
		authenticated: true,
		registry:      r,
		tunnels:       make(map[uint64]*Tunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}
	pr := &pendingReq{
		streamData: make(chan []byte, 256),
		done:       make(chan struct{}),
	}
	h.pending[42] = pr

	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 3; i++ {
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			h.cleanup()
		}()
	}
	wg.Wait()
}
