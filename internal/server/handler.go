package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/protocol"
	"vlgr/internal/version"
)

// tcpTunnel is a raw TCP tunnel: a public listener whose connections are
// relayed to the client's local port.
type tcpTunnel struct {
	id         uint64
	publicPort uint16
	localPort  uint16
	ln         net.Listener
}

type pendingReq struct {
	response   chan protocol.HTTPResponse
	streamData chan []byte
	done       chan struct{}
	doneOnce   sync.Once
	streamOnce sync.Once
	// ready (TCP tunnels only) is closed once the client has opened its
	// local connection, so the server doesn't forward public bytes into a
	// relay that isn't wired up yet.
	ready     chan struct{}
	readyOnce sync.Once
}

func (pr *pendingReq) closeReady() {
	if pr.ready == nil {
		return
	}
	pr.readyOnce.Do(func() { close(pr.ready) })
}

func (pr *pendingReq) closeDone() {
	pr.doneOnce.Do(func() { close(pr.done) })
}

func (pr *pendingReq) closeStream() {
	if pr.streamData == nil {
		return
	}
	pr.streamOnce.Do(func() { close(pr.streamData) })
}

type ClientHandler struct {
	conn          *websocket.Conn
	registry      *Registry
	tunnels       map[uint64]*Tunnel
	tunnelCount   int
	baseDomain    string
	expectedToken string
	expectedHash  [32]byte
	authenticated bool
	clientVersion string
	debug         bool

	// TCP tunnel support (nil allocator = TCP disabled on this server).
	tcpAlloc   *PortAllocator
	tcpHost    string
	tcpTunnels map[uint64]*tcpTunnel

	pending      map[uint64]*pendingReq
	mu           sync.Mutex
	writeMu      sync.Mutex
	requestIDSeq uint64

	done      chan struct{}
	closeOnce sync.Once
}

func NewClientHandler(conn *websocket.Conn, registry *Registry, baseDomain string, expectedToken string, debug bool, tcpAlloc *PortAllocator, tcpHost string) *ClientHandler {
	h := &ClientHandler{
		conn:          conn,
		registry:      registry,
		tunnels:       make(map[uint64]*Tunnel),
		baseDomain:    baseDomain,
		expectedToken: expectedToken,
		debug:         debug,
		tcpAlloc:      tcpAlloc,
		tcpHost:       tcpHost,
		tcpTunnels:    make(map[uint64]*tcpTunnel),
		pending:       make(map[uint64]*pendingReq),
		done:          make(chan struct{}),
	}
	if expectedToken != "" {
		h.expectedHash = sha256.Sum256([]byte(expectedToken))
	}
	return h
}

func (h *ClientHandler) isAuthenticated() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authenticated
}

func (h *ClientHandler) nextRequestID() uint64 {
	return atomic.AddUint64(&h.requestIDSeq, 1)
}

func (h *ClientHandler) removePending(requestID uint64) {
	h.mu.Lock()
	delete(h.pending, requestID)
	h.mu.Unlock()
}

func (h *ClientHandler) Run() {
	defer h.cleanup()

	h.conn.SetReadLimit(protocol.MaxFrameSize + protocol.HeaderSize)
	h.conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
	h.conn.SetPongHandler(func(string) error {
		h.conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
		return nil
	})

	// Idle unauthenticated connections would hold connSem slots forever
	// (pings keep them alive) — drop them if auth doesn't arrive in time.
	authTimer := time.AfterFunc(protocol.AuthTimeout, func() {
		if !h.isAuthenticated() {
			log.Printf("[handler] closing connection: no auth within %v", protocol.AuthTimeout)
			h.cleanup()
		}
	})
	defer authTimer.Stop()

	go h.pingLoop()

	for {
		h.conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
		_, msg, err := h.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[handler] read error: %v", err)
			}
			return
		}

		frame, err := protocol.DecodeFrame(msg)
		if err != nil {
			log.Printf("[handler] decode error: %v (%d bytes)", err, len(msg))
			if h.debug && len(msg) > 0 {
				log.Printf("[debug] raw frame hex: %s", protocol.HexPreview(msg, 128))
			}
			continue
		}

		h.handleFrame(frame)
	}
}

func (h *ClientHandler) handleFrame(frame protocol.Frame) {
	if !h.isAuthenticated() && frame.Type != protocol.MsgAuth {
		log.Printf("[handler] rejecting message type 0x%02x before auth", frame.Type)
		h.writeMessage(protocol.MsgAuthErr, 0, 0, []byte("authenticate first"))
		return
	}

	switch frame.Type {
	case protocol.MsgAuth:
		h.handleAuth(frame)
	case protocol.MsgRegister:
		h.handleRegister(frame)
	case protocol.MsgUnregister:
		h.handleUnregister(frame)
	case protocol.MsgRegisterTCP:
		h.handleRegisterTCP(frame)
	case protocol.MsgTCPOpen:
		h.handleTCPReady(frame)
	case protocol.MsgHTTPRes:
		h.handleHTTPRes(frame)
	case protocol.MsgStreamData:
		h.handleStreamData(frame)
	case protocol.MsgStreamClose:
		h.handleStreamClose(frame)
	case protocol.MsgCloseTunnel:
		log.Printf("[handler] client requested close")
		h.cleanup()
	default:
		log.Printf("[handler] unknown message type: 0x%02x", frame.Type)
	}
}

func (h *ClientHandler) pingLoop() {
	protocol.PingLoop(h.conn, &h.writeMu, h.done)
}

func (h *ClientHandler) handleAuth(frame protocol.Frame) {
	token, clientVersion, err := protocol.DecodeAuth(frame.Payload)
	if err != nil {
		log.Printf("[handler] auth rejected: malformed auth payload: %v", err)
		h.writeMessage(protocol.MsgAuthErr, 0, 0, []byte("malformed auth"))
		h.cleanup()
		return
	}

	if h.expectedToken != "" {
		gotHash := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(h.expectedHash[:], gotHash[:]) != 1 {
			log.Printf("[handler] auth rejected: invalid token")
			// Flat cost per attempt to slow down token brute force.
			time.Sleep(time.Second)
			h.writeMessage(protocol.MsgAuthErr, 0, 0, []byte("invalid token"))
			h.cleanup()
			return
		}
	}
	h.mu.Lock()
	h.authenticated = true
	h.mu.Unlock()
	h.clientVersion = clientVersion

	if clientVersion != "" && clientVersion != version.Version {
		log.Printf("[handler] WARNING: version mismatch — client %q vs server %q; connection may be unstable", clientVersion, version.Version)
	}

	authOKPayload, err := protocol.EncodeAuthOK(version.Version)
	if err != nil {
		log.Printf("[handler] encode auth ok: %v", err)
		h.writeMessage(protocol.MsgAuthOK, 0, 0, nil)
		return
	}
	h.writeMessage(protocol.MsgAuthOK, 0, 0, authOKPayload)
	log.Printf("[handler] client authenticated")
}

func (h *ClientHandler) handleRegister(frame protocol.Frame) {
	localPort, requestedSubdomain, err := protocol.DecodeRegister(frame.Payload)
	if err != nil {
		h.registerErr(err.Error())
		return
	}
	if localPort == 0 {
		h.registerErr("invalid port: 0")
		return
	}
	if !validSubdomain(requestedSubdomain) {
		h.registerErr("invalid subdomain characters")
		return
	}
	if requestedSubdomain == "" {
		requestedSubdomain = generateSubdomain()
	}

	publicURL := fmt.Sprintf("%s.%s", requestedSubdomain, h.baseDomain)
	if len(publicURL) > 255 {
		h.registerErr("public URL too long")
		return
	}

	h.mu.Lock()
	if h.tunnelCount >= protocol.MaxTunnelsPerClient {
		h.mu.Unlock()
		h.registerErr(fmt.Sprintf("too many tunnels: max %d per client", protocol.MaxTunnelsPerClient))
		return
	}
	tunnel, err := h.registry.Register(requestedSubdomain, h)
	if err != nil {
		h.mu.Unlock()
		h.registerErr(err.Error())
		return
	}
	h.tunnels[tunnel.ID] = tunnel
	h.tunnelCount++
	h.mu.Unlock()

	// URL length validated above, encoding cannot fail.
	respPayload, _ := protocol.EncodeRegisterOK(publicURL, tunnel.ID)
	h.writeMessage(protocol.MsgRegisterOK, tunnel.ID, 0, respPayload)
	log.Printf("[handler] tunnel registered: %s -> localhost:%d", publicURL, localPort)
}

func (h *ClientHandler) handleUnregister(frame protocol.Frame) {
	h.mu.Lock()
	tunnel, ok := h.tunnels[frame.TunnelID]
	if ok {
		delete(h.tunnels, frame.TunnelID)
		h.tunnelCount--
	}
	h.mu.Unlock()

	if !ok {
		h.writeMessage(protocol.MsgUnregisterErr, frame.TunnelID, 0, []byte("unknown tunnel"))
		return
	}

	h.registry.Unregister(tunnel.Subdomain)
	h.writeMessage(protocol.MsgUnregisterOK, frame.TunnelID, 0, nil)
	log.Printf("[handler] tunnel %s unregistered by client", tunnel.Subdomain)
}

func (h *ClientHandler) handleRegisterTCP(frame protocol.Frame) {
	localPort, remotePort, err := protocol.DecodeRegisterTCP(frame.Payload)
	if err != nil {
		h.registerErr(err.Error())
		return
	}
	if localPort == 0 {
		h.registerErr("invalid port: 0")
		return
	}
	if h.tcpAlloc == nil {
		h.registerErr("tcp tunnels not enabled on this server")
		return
	}

	h.mu.Lock()
	if h.tunnelCount >= protocol.MaxTunnelsPerClient {
		h.mu.Unlock()
		h.registerErr(fmt.Sprintf("too many tunnels: max %d per client", protocol.MaxTunnelsPerClient))
		return
	}
	h.mu.Unlock()

	publicPort, err := h.tcpAlloc.Allocate(remotePort)
	if err != nil {
		h.registerErr(err.Error())
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", publicPort))
	if err != nil {
		h.tcpAlloc.Release(publicPort)
		h.registerErr(fmt.Sprintf("listen on public port %d: %v", publicPort, err))
		return
	}

	tt := &tcpTunnel{
		id:         generateTunnelID(),
		publicPort: publicPort,
		localPort:  localPort,
		ln:         ln,
	}

	h.mu.Lock()
	h.tcpTunnels[tt.id] = tt
	h.tunnelCount++
	h.mu.Unlock()

	publicURL := fmt.Sprintf("%s:%d", h.tcpHost, publicPort)
	respPayload, err := protocol.EncodeRegisterOK(publicURL, tt.id)
	if err != nil {
		h.closeTCPTunnel(tt)
		h.registerErr(err.Error())
		return
	}
	h.writeMessage(protocol.MsgRegisterOK, tt.id, 0, respPayload)
	log.Printf("[handler] TCP tunnel registered: %s -> localhost:%d", publicURL, localPort)

	go h.acceptTCP(tt)
}

// acceptTCP relays every connection on the public listener to the client.
// handleTCPReady marks a TCP connection's client-side relay as established.
func (h *ClientHandler) handleTCPReady(frame protocol.Frame) {
	h.mu.Lock()
	pr, ok := h.pending[frame.RequestID]
	h.mu.Unlock()
	if ok {
		pr.closeReady()
	}
}

func (h *ClientHandler) acceptTCP(tt *tcpTunnel) {
	for {
		conn, err := tt.ln.Accept()
		if err != nil {
			return // listener closed on cleanup
		}
		go h.handleTCPConn(tt, conn)
	}
}

func (h *ClientHandler) handleTCPConn(tt *tcpTunnel, conn net.Conn) {
	defer conn.Close()

	requestID := h.nextRequestID()
	pr := &pendingReq{
		streamData: make(chan []byte, protocol.StreamRelayBuf),
		done:       make(chan struct{}),
		ready:      make(chan struct{}),
	}
	h.mu.Lock()
	h.pending[requestID] = pr
	h.mu.Unlock()
	defer func() {
		h.removePending(requestID)
		pr.closeDone()
	}()

	// Ask the client to open a matching local connection.
	if err := h.writeMessage(protocol.MsgTCPOpen, tt.id, requestID, nil); err != nil {
		return
	}

	// Wait for the client to confirm its local connection before pumping
	// public bytes, otherwise early bytes race the relay setup and are lost.
	select {
	case <-pr.ready:
	case <-time.After(protocol.RequestTimeout):
		return
	case <-h.done:
		return
	}

	// client → public: drain stream data onto the public connection.
	go func() {
		for {
			select {
			case data, ok := <-pr.streamData:
				if !ok {
					conn.Close()
					return
				}
				conn.SetWriteDeadline(time.Now().Add(protocol.WriteWait))
				if _, err := conn.Write(data); err != nil {
					conn.Close()
					return
				}
			case <-pr.done:
				conn.Close()
				return
			case <-h.done:
				conn.Close()
				return
			}
		}
	}()

	// public → client: read from the public connection into stream frames.
	buf := make([]byte, protocol.StreamBufSize)
	conn.SetReadDeadline(time.Now().Add(protocol.RelayIdleTimeout))
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			// EncodeFrame copies the payload, so buf can be reused.
			if werr := h.SendStreamData(requestID, buf[:n]); werr != nil {
				break
			}
			conn.SetReadDeadline(time.Now().Add(protocol.RelayIdleTimeout))
		}
		if err != nil {
			break
		}
	}
	h.SendStreamClose(requestID)
}

func (h *ClientHandler) closeTCPTunnel(tt *tcpTunnel) {
	tt.ln.Close()
	h.tcpAlloc.Release(tt.publicPort)
}

func (h *ClientHandler) handleHTTPRes(frame protocol.Frame) {
	resp, err := protocol.DecodeHTTPResponse(frame.Payload)
	if err != nil {
		log.Printf("[handler] decode HTTP response error: %v (payload %d bytes)", err, len(frame.Payload))
		if h.debug && len(frame.Payload) > 0 {
			log.Printf("[debug] response payload hex: %s", protocol.HexPreview(frame.Payload, 256))
		}
		return
	}

	h.mu.Lock()
	pr, ok := h.pending[frame.RequestID]
	if ok && pr.streamData == nil {
		delete(h.pending, frame.RequestID)
	}
	h.mu.Unlock()

	if !ok {
		log.Printf("[handler] no pending request for ID %d", frame.RequestID)
		return
	}

	select {
	case pr.response <- resp:
	case <-pr.done:
	case <-time.After(5 * time.Second):
		log.Printf("[handler] response channel blocked for request %d", frame.RequestID)
	}
}

func (h *ClientHandler) handleStreamData(frame protocol.Frame) {
	h.mu.Lock()
	pr, ok := h.pending[frame.RequestID]
	h.mu.Unlock()

	if !ok || pr.streamData == nil {
		return
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[handler] panic in handleStreamData: %v", r)
			}
		}()
		select {
		case pr.streamData <- frame.Payload:
		case <-pr.done:
		case <-time.After(protocol.StreamSendTimeout):
			log.Printf("[handler] stream send timeout for request %d (slow consumer), closing stream", frame.RequestID)
			h.removePending(frame.RequestID)
			pr.closeStream()
			h.SendStreamClose(frame.RequestID)
		}
	}()
}

func (h *ClientHandler) handleStreamClose(frame protocol.Frame) {
	h.mu.Lock()
	pr, ok := h.pending[frame.RequestID]
	if ok {
		delete(h.pending, frame.RequestID)
	}
	h.mu.Unlock()

	if !ok || pr.streamData == nil {
		return
	}

	// If the client couldn't open its local connection it sends a close
	// instead of a ready ack; unblock the waiting relay so it tears down.
	pr.closeReady()
	pr.closeStream()
}

// startForward registers a pending request and sends the MsgHTTPReq frame.
// The caller must eventually call finishForward (directly or via the cleanup
// func returned by forward).
func (h *ClientHandler) startForward(tunnelID uint64, req protocol.HTTPRequest, streamData chan []byte) (uint64, *pendingReq, error) {
	requestID := h.nextRequestID()

	if h.debug {
		log.Printf("[debug] forward request #%d: %s %s (%d headers, %d body bytes, streamed=%v)",
			requestID, req.Method, req.Path, len(req.Headers), len(req.Body), req.BodyStreamed)
	}

	pr := &pendingReq{
		response:   make(chan protocol.HTTPResponse, 1),
		streamData: streamData,
		done:       make(chan struct{}),
	}

	h.mu.Lock()
	h.pending[requestID] = pr
	h.mu.Unlock()

	payload, err := protocol.EncodeHTTPRequest(req)
	if err == nil && len(payload) > protocol.MaxFrameSize {
		err = fmt.Errorf("encoded request exceeds max frame size: %d > %d", len(payload), protocol.MaxFrameSize)
	}
	if err != nil {
		h.finishForward(requestID, pr)
		return 0, nil, fmt.Errorf("forward: encode error: %w", err)
	}

	if err := h.writeMessage(protocol.MsgHTTPReq, tunnelID, requestID, payload); err != nil {
		h.finishForward(requestID, pr)
		return 0, nil, fmt.Errorf("forward: write error: %w", err)
	}

	return requestID, pr, nil
}

// awaitResponse waits for the MsgHTTPRes frame; on timeout it tears the
// pending request down.
func (h *ClientHandler) awaitResponse(requestID uint64, pr *pendingReq, timeout time.Duration) (protocol.HTTPResponse, error) {
	select {
	case resp := <-pr.response:
		if h.debug {
			log.Printf("[debug] forward response #%d: status %d (%d headers, %d body bytes, streamed=%v)",
				requestID, resp.StatusCode, len(resp.Headers), len(resp.Body), resp.BodyStreamed)
		}
		return resp, nil
	case <-time.After(timeout):
		h.finishForward(requestID, pr)
		return protocol.HTTPResponse{}, fmt.Errorf("forward: timeout after %v", timeout)
	}
}

func (h *ClientHandler) finishForward(requestID uint64, pr *pendingReq) {
	h.removePending(requestID)
	pr.closeDone()
}

// forward relays a request and returns the response. With a non-nil
// streamData (WebSocket upgrade) the caller must invoke cleanup when the
// relay ends — it keeps the pending entry alive for stream frames.
func (h *ClientHandler) forward(tunnelID uint64, req protocol.HTTPRequest, streamData chan []byte) (requestID uint64, resp protocol.HTTPResponse, cleanup func(), err error) {
	requestID, pr, err := h.startForward(tunnelID, req, streamData)
	if err != nil {
		return 0, protocol.HTTPResponse{}, nil, err
	}

	resp, err = h.awaitResponse(requestID, pr, protocol.RequestTimeout)
	if err != nil {
		return 0, protocol.HTTPResponse{}, nil, err
	}

	if streamData == nil {
		h.finishForward(requestID, pr)
	}

	return requestID, resp, func() {
		if streamData != nil {
			h.finishForward(requestID, pr)
		}
	}, nil
}

func (h *ClientHandler) SendStreamData(requestID uint64, data []byte) error {
	return h.writeMessage(protocol.MsgStreamData, 0, requestID, data)
}

func (h *ClientHandler) SendStreamClose(requestID uint64) error {
	return h.writeMessage(protocol.MsgStreamClose, 0, requestID, nil)
}

func (h *ClientHandler) writeMessage(msgType byte, tunnelID, requestID uint64, payload []byte) error {
	frame := protocol.EncodeFrame(protocol.Frame{
		Type:      msgType,
		TunnelID:  tunnelID,
		RequestID: requestID,
		Payload:   payload,
	})

	if h.conn == nil {
		return fmt.Errorf("no connection")
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	h.conn.SetWriteDeadline(time.Now().Add(protocol.WriteWait))
	return h.conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (h *ClientHandler) registerErr(msg string) {
	h.writeMessage(protocol.MsgRegisterErr, 0, 0, []byte(msg))
}

func (h *ClientHandler) cleanup() {
	h.closeOnce.Do(func() {
		for _, tunnel := range h.tunnels {
			h.registry.Unregister(tunnel.Subdomain)
			log.Printf("[handler] tunnel %s unregistered", tunnel.Subdomain)
		}

		for _, tt := range h.tcpTunnels {
			h.closeTCPTunnel(tt)
			log.Printf("[handler] TCP tunnel :%d closed", tt.publicPort)
		}

		h.mu.Lock()
		for id, pr := range h.pending {
			pr.closeStream()
			pr.closeDone()
			delete(h.pending, id)
		}
		h.mu.Unlock()

		close(h.done)
		if h.conn != nil {
			h.conn.Close()
		}
	})
}

func validSubdomain(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > protocol.MaxSubdomainLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}
