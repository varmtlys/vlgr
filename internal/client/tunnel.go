package client

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/protocol"
	"vlgr/internal/version"
)

// No global Timeout: it would abort streamed transfers mid-flight. The
// response header must still arrive within RequestTimeout.
var httpClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		ResponseHeaderTimeout: protocol.RequestTimeout,
	},
}

const (
	maxConcurrentLocalReqs = 100
	maxConcurrentRelays    = 200
)

type streamRelay struct {
	conn     net.Conn
	done     chan struct{}
	doneOnce sync.Once
}

// bodyStream buffers incoming request-body chunks for a streamed upload.
type bodyStream struct {
	ch      chan []byte
	aborted atomic.Bool
}

// bodyStreamReader adapts a bodyStream to io.Reader for http.Request.Body.
type bodyStreamReader struct {
	s    *bodyStream
	done <-chan struct{}
	rem  []byte
}

func (r *bodyStreamReader) Read(p []byte) (int, error) {
	for len(r.rem) == 0 {
		select {
		case data, ok := <-r.s.ch:
			if !ok {
				if r.s.aborted.Load() {
					return 0, fmt.Errorf("body stream aborted")
				}
				return 0, io.EOF
			}
			r.rem = data
		case <-r.done:
			return 0, fmt.Errorf("tunnel closed")
		}
	}
	n := copy(p, r.rem)
	r.rem = r.rem[n:]
	return n, nil
}

func (r *streamRelay) closeDone() {
	r.doneOnce.Do(func() { close(r.done) })
}

type addResult struct {
	url string
	err error
}

// Forward describes one active port forward (tunnel).
type Forward struct {
	TunnelID uint64
	Port     uint16
	URL      string
}

type Tunnel struct {
	serverAddr string
	token      string
	ports      []uint16
	subdomains []string
	useTLS     bool
	debug      bool

	conn *websocket.Conn

	// forwards holds the active tunnels in registration order.
	forwards   []Forward
	mappingsMu sync.Mutex

	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once

	relays   map[uint64]*streamRelay
	relaysMu sync.Mutex

	// Streamed HTTP bodies: incoming request bodies and cancel signals
	// for outgoing streamed responses, keyed by requestID.
	bodyStreams map[uint64]*bodyStream
	respCancels map[uint64]chan struct{}
	streamsMu   sync.Mutex

	localReqSem chan struct{}
	relaySem    chan struct{}

	controlLn   net.Listener
	controlAddr string

	addMu   sync.Mutex
	addCh   chan addResult
	addPort uint16
	delCh   chan error
	delID   uint64

	// onChange, when set, is called after the set of forwards changes
	// (used by the tray UI to refresh its menu).
	onChange func()
}

func NewTunnel(serverAddr, token string, ports []uint16, subdomains []string, useTLS bool) *Tunnel {
	return &Tunnel{
		serverAddr:  serverAddr,
		token:       token,
		ports:       ports,
		subdomains:  subdomains,
		useTLS:      useTLS,
		done:        make(chan struct{}),
		relays:      make(map[uint64]*streamRelay),
		localReqSem: make(chan struct{}, maxConcurrentLocalReqs),
		relaySem:    make(chan struct{}, maxConcurrentRelays),
		bodyStreams: make(map[uint64]*bodyStream),
		respCancels: make(map[uint64]chan struct{}),
	}
}

func (t *Tunnel) SetDebug(debug bool) {
	t.debug = debug
}

func (t *Tunnel) Connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	scheme := "ws"
	if t.useTLS {
		scheme = "wss"
	}
	url := scheme + "://" + t.serverAddr + "/_tunnel"
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	conn.SetReadLimit(protocol.MaxFrameSize + protocol.HeaderSize)
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	t.conn = conn

	authPayload, err := protocol.EncodeAuth(t.token, version.Version)
	if err != nil {
		return fmt.Errorf("encode auth: %w", err)
	}
	authFrame := protocol.EncodeFrame(protocol.Frame{
		Type:    protocol.MsgAuth,
		Payload: authPayload,
	})
	if err := conn.WriteMessage(websocket.BinaryMessage, authFrame); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	authResp, err := protocol.DecodeFrame(msg)
	if err != nil {
		return fmt.Errorf("decode auth response: %w", err)
	}
	if authResp.Type == protocol.MsgAuthErr {
		return fmt.Errorf("authentication rejected: %s", string(authResp.Payload))
	}
	if authResp.Type != protocol.MsgAuthOK {
		return fmt.Errorf("unexpected auth response type: 0x%02x", authResp.Type)
	}

	serverVersion, err := protocol.DecodeAuthOK(authResp.Payload)
	if err != nil {
		log.Printf("[client] warning: could not decode server version: %v", err)
	} else if serverVersion != "" && serverVersion != version.Version {
		log.Printf("[client] WARNING: version mismatch — client %q vs server %q; connection may be unstable", version.Version, serverVersion)
	}
	log.Printf("[client] authenticated")

	for i, port := range t.ports {
		subdomain := ""
		if i < len(t.subdomains) {
			subdomain = t.subdomains[i]
		}

		regPayload, err := protocol.EncodeRegister(port, subdomain)
		if err != nil {
			return fmt.Errorf("encode register for port %d: %w", port, err)
		}

		regFrame := protocol.EncodeFrame(protocol.Frame{
			Type:    protocol.MsgRegister,
			Payload: regPayload,
		})
		if err := conn.WriteMessage(websocket.BinaryMessage, regFrame); err != nil {
			return fmt.Errorf("send register for port %d: %w", port, err)
		}

		// Per-response deadline: one absolute deadline for the whole loop
		// would spuriously fail with many ports on a slow link.
		conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, msg, err = conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read register response for port %d: %w", port, err)
		}
		regResp, err := protocol.DecodeFrame(msg)
		if err != nil {
			return fmt.Errorf("decode register response for port %d: %w", port, err)
		}
		if regResp.Type == protocol.MsgRegisterErr || regResp.Type == protocol.MsgError {
			return fmt.Errorf("registration for port %d failed: %s", port, string(regResp.Payload))
		}
		if regResp.Type != protocol.MsgRegisterOK {
			return fmt.Errorf("unexpected register response type for port %d: 0x%02x", port, regResp.Type)
		}

		publicURL, tunnelID, err := protocol.DecodeRegisterOK(regResp.Payload)
		if err != nil {
			return fmt.Errorf("register response for port %d: %w", port, err)
		}
		t.setMapping(tunnelID, port, publicURL)

		log.Printf("[client] tunnel ready: %s -> localhost:%d", publicURL, port)
	}

	return nil
}

func (t *Tunnel) Run() {
	defer t.closeOnce.Do(func() { close(t.done) })
	defer t.conn.Close()

	conn := t.conn
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
		return nil
	})

	go t.pingLoop()

	for {
		conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-t.done: // local Close() — expected, don't log as error
			default:
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("[client] read error: %v", err)
				} else {
					log.Printf("[client] connection closed")
				}
			}
			return
		}

		frame, err := protocol.DecodeFrame(msg)
		if err != nil {
			log.Printf("[client] decode error: %v", err)
			continue
		}

		switch frame.Type {
		case protocol.MsgHTTPReq:
			if t.debug {
				log.Printf("[debug] received HTTP request frame: tunnel=%d request=%d payload=%d bytes",
					frame.TunnelID, frame.RequestID, len(frame.Payload))
			}
			go t.handleHTTPReq(frame)
		case protocol.MsgRegisterOK:
			t.handleAddResponse(frame, nil)
		case protocol.MsgRegisterErr:
			t.handleAddResponse(frame, fmt.Errorf("%s", string(frame.Payload)))
		case protocol.MsgUnregisterOK:
			t.handleDelResponse(nil)
		case protocol.MsgUnregisterErr:
			t.handleDelResponse(fmt.Errorf("%s", string(frame.Payload)))
		case protocol.MsgStreamData:
			t.handleStreamData(frame)
		case protocol.MsgStreamClose:
			t.handleStreamClose(frame)
		case protocol.MsgError:
			if !t.handleAddResponse(frame, fmt.Errorf("%s", string(frame.Payload))) {
				log.Printf("[client] server error: %s", string(frame.Payload))
			}
		case protocol.MsgCloseTunnel:
			log.Printf("[client] server closed tunnel")
			return
		default:
			log.Printf("[client] unknown message type: 0x%02x", frame.Type)
		}
	}
}

// handleAddResponse resolves a pending AddPort call; err is non-nil for
// MsgRegisterErr/MsgError frames. Returns false when no add is pending.
func (t *Tunnel) handleAddResponse(frame protocol.Frame, err error) bool {
	t.addMu.Lock()
	ch := t.addCh
	port := t.addPort
	t.addCh = nil
	t.addMu.Unlock()

	if ch == nil {
		return false
	}

	if err != nil {
		ch <- addResult{err: err}
		return true
	}

	url, tunnelID, decErr := protocol.DecodeRegisterOK(frame.Payload)
	if decErr != nil {
		ch <- addResult{err: decErr}
		return true
	}
	t.setMapping(tunnelID, port, url)
	log.Printf("[client] tunnel ready: %s -> localhost:%d", url, port)
	ch <- addResult{url: url}
	return true
}

func (t *Tunnel) handleStreamData(frame protocol.Frame) {
	t.relaysMu.Lock()
	relay, ok := t.relays[frame.RequestID]
	t.relaysMu.Unlock()

	if ok {
		select {
		case <-relay.done:
			return
		default:
		}

		relay.conn.SetWriteDeadline(time.Now().Add(protocol.WriteWait))
		if _, err := relay.conn.Write(frame.Payload); err != nil {
			log.Printf("[client] stream write error for request %d: %v", frame.RequestID, err)
			relay.closeDone()
			relay.conn.Close()
		}
		return
	}

	// Streamed request body chunk. Safe to hold frame.Payload: each
	// ReadMessage returns a fresh buffer.
	t.streamsMu.Lock()
	bs, ok := t.bodyStreams[frame.RequestID]
	t.streamsMu.Unlock()
	if !ok {
		return
	}

	select {
	case bs.ch <- frame.Payload:
	case <-time.After(protocol.StreamSendTimeout):
		// Local app is not consuming the upload; abort rather than
		// silently dropping a chunk.
		log.Printf("[client] body stream stalled for request %d, aborting", frame.RequestID)
		t.streamsMu.Lock()
		delete(t.bodyStreams, frame.RequestID)
		t.streamsMu.Unlock()
		bs.aborted.Store(true)
		close(bs.ch)
	}
}

func (t *Tunnel) handleStreamClose(frame protocol.Frame) {
	t.relaysMu.Lock()
	relay, ok := t.relays[frame.RequestID]
	if ok {
		delete(t.relays, frame.RequestID)
	}
	t.relaysMu.Unlock()

	if ok {
		relay.closeDone()
		relay.conn.Close()
		return
	}

	// Request body EOF for a streamed upload.
	t.streamsMu.Lock()
	bs, ok := t.bodyStreams[frame.RequestID]
	if ok {
		delete(t.bodyStreams, frame.RequestID)
	}
	t.streamsMu.Unlock()
	if ok {
		close(bs.ch)
		return
	}

	// Server aborted a streamed response (viewer disconnected).
	t.streamsMu.Lock()
	cancel, ok := t.respCancels[frame.RequestID]
	if ok {
		delete(t.respCancels, frame.RequestID)
	}
	t.streamsMu.Unlock()
	if ok {
		close(cancel)
	}
}

func (t *Tunnel) StartControl() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	t.controlLn = ln
	t.controlAddr = ln.Addr().String()

	go t.serveControl()
	return nil
}

func (t *Tunnel) ControlAddr() string {
	return t.controlAddr
}

func (t *Tunnel) serveControl() {
	for {
		conn, err := t.controlLn.Accept()
		if err != nil {
			return
		}
		go t.handleControlConn(conn)
	}
}

func (t *Tunnel) handleControlConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		switch parts[0] {
		case "ADD":
			if len(parts) < 2 {
				fmt.Fprintf(conn, "ERR bad command\n")
				continue
			}
			port, err := strconv.Atoi(parts[1])
			if err != nil || port < 1 || port > 65535 {
				fmt.Fprintf(conn, "ERR invalid port\n")
				continue
			}
			subdomain := ""
			if len(parts) > 2 {
				subdomain = parts[2]
			}
			url, err := t.AddPort(uint16(port), subdomain)
			if err != nil {
				fmt.Fprintf(conn, "ERR %v\n", err)
			} else {
				fmt.Fprintf(conn, "OK %s\n", url)
			}
		case "DEL":
			if len(parts) < 2 {
				fmt.Fprintf(conn, "ERR bad command\n")
				continue
			}
			port, err := strconv.Atoi(parts[1])
			if err != nil || port < 1 || port > 65535 {
				fmt.Fprintf(conn, "ERR invalid port\n")
				continue
			}
			if err := t.RemovePort(uint16(port)); err != nil {
				fmt.Fprintf(conn, "ERR %v\n", err)
			} else {
				fmt.Fprintf(conn, "OK removed port %d\n", port)
			}
		case "LIST":
			// Response: "OK <n>" followed by n lines "<port> <url>".
			forwards := t.Forwards()
			fmt.Fprintf(conn, "OK %d\n", len(forwards))
			for _, f := range forwards {
				fmt.Fprintf(conn, "%d %s\n", f.Port, f.URL)
			}
		default:
			fmt.Fprintf(conn, "ERR bad command\n")
		}
	}
}

// handleDelResponse resolves a pending RemovePort call; err is non-nil for
// MsgUnregisterErr frames.
func (t *Tunnel) handleDelResponse(err error) {
	t.addMu.Lock()
	ch := t.delCh
	id := t.delID
	t.delCh = nil
	t.addMu.Unlock()

	if ch == nil {
		return
	}
	if err == nil {
		t.removeMapping(id)
	}
	ch <- err
}

// RemovePort unregisters the forward for the given local port on the server
// and removes it locally.
func (t *Tunnel) RemovePort(port uint16) error {
	var tunnelID uint64
	found := false
	t.mappingsMu.Lock()
	for _, f := range t.forwards {
		if f.Port == port {
			tunnelID = f.TunnelID
			found = true
			break
		}
	}
	t.mappingsMu.Unlock()
	if !found {
		return fmt.Errorf("no forward for port %d", port)
	}

	t.addMu.Lock()
	if t.delCh != nil {
		t.addMu.Unlock()
		return fmt.Errorf("another delete operation in progress")
	}
	ch := make(chan error, 1)
	t.delCh = ch
	t.delID = tunnelID
	t.addMu.Unlock()

	if err := t.writeFrame(protocol.Frame{
		Type:     protocol.MsgUnregister,
		TunnelID: tunnelID,
	}); err != nil {
		t.addMu.Lock()
		t.delCh = nil
		t.addMu.Unlock()
		return fmt.Errorf("send unregister: %w", err)
	}

	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.addMu.Lock()
		t.delCh = nil
		t.addMu.Unlock()
		return fmt.Errorf("remove port timeout")
	}
}

func (t *Tunnel) AddPort(port uint16, subdomain string) (string, error) {
	t.addMu.Lock()
	if t.addCh != nil {
		t.addMu.Unlock()
		return "", fmt.Errorf("another add operation in progress")
	}

	regPayload, err := protocol.EncodeRegister(port, subdomain)
	if err != nil {
		t.addMu.Unlock()
		return "", err
	}

	ch := make(chan addResult, 1)
	t.addCh = ch
	t.addPort = port
	t.addMu.Unlock()

	if err := t.writeFrame(protocol.Frame{
		Type:    protocol.MsgRegister,
		Payload: regPayload,
	}); err != nil {
		t.addMu.Lock()
		t.addCh = nil
		t.addMu.Unlock()
		return "", fmt.Errorf("send register: %w", err)
	}

	select {
	case result := <-ch:
		return result.url, result.err
	case <-time.After(15 * time.Second):
		t.addMu.Lock()
		t.addCh = nil
		t.addMu.Unlock()
		return "", fmt.Errorf("add port timeout")
	}
}

func (t *Tunnel) pingLoop() {
	protocol.PingLoop(t.conn, &t.writeMu, t.done)
}

// SetOnChange registers a callback invoked after the set of forwards
// changes. Must be set before Connect.
func (t *Tunnel) SetOnChange(fn func()) {
	t.onChange = fn
}

func (t *Tunnel) notifyChange() {
	if t.onChange != nil {
		t.onChange()
	}
}

func (t *Tunnel) setMapping(tunnelID uint64, port uint16, url string) {
	t.mappingsMu.Lock()
	t.forwards = append(t.forwards, Forward{TunnelID: tunnelID, Port: port, URL: url})
	t.mappingsMu.Unlock()
	t.notifyChange()
}

func (t *Tunnel) removeMapping(tunnelID uint64) {
	t.mappingsMu.Lock()
	for i, f := range t.forwards {
		if f.TunnelID == tunnelID {
			t.forwards = append(t.forwards[:i], t.forwards[i+1:]...)
			break
		}
	}
	t.mappingsMu.Unlock()
	t.notifyChange()
}

// Forwards returns a snapshot of the active forwards.
func (t *Tunnel) Forwards() []Forward {
	t.mappingsMu.Lock()
	defer t.mappingsMu.Unlock()
	out := make([]Forward, len(t.forwards))
	copy(out, t.forwards)
	return out
}

func (t *Tunnel) portFor(tunnelID uint64) (uint16, bool) {
	t.mappingsMu.Lock()
	defer t.mappingsMu.Unlock()
	for _, f := range t.forwards {
		if f.TunnelID == tunnelID {
			return f.Port, true
		}
	}
	return 0, false
}

// lookupPort resolves the local port for a tunnel, reporting 502 to the
// server when the tunnel is unknown.
func (t *Tunnel) lookupPort(tunnelID, requestID uint64) (uint16, bool) {
	port, ok := t.portFor(tunnelID)
	if !ok {
		log.Printf("[client] unknown tunnel ID %d", tunnelID)
		t.sendHTTPError(tunnelID, requestID, 502, fmt.Sprintf("unknown tunnel %d", tunnelID))
	}
	return port, ok
}

func copyHeaders(dst *http.Request, src map[string][]string) {
	for k, values := range src {
		switch k {
		case "Content-Length", "Transfer-Encoding":
			continue
		case "Host":
			if len(values) > 0 {
				dst.Host = values[0]
			}
			continue
		}
		for _, v := range values {
			dst.Header.Add(k, v)
		}
	}
}

func (t *Tunnel) handleHTTPReq(frame protocol.Frame) {
	select {
	case t.localReqSem <- struct{}{}:
	default:
		t.sendHTTPError(frame.TunnelID, frame.RequestID, 503, "too many concurrent local requests")
		return
	}
	var relOnce sync.Once
	release := func() { relOnce.Do(func() { <-t.localReqSem }) }
	defer release()

	req, err := protocol.DecodeHTTPRequest(frame.Payload)
	if err != nil {
		log.Printf("[client] decode HTTP request error: %v (payload %d bytes)", err, len(frame.Payload))
		if t.debug && len(frame.Payload) > 0 {
			log.Printf("[debug] request payload hex: %s", protocol.HexPreview(frame.Payload, 256))
			log.Printf("[debug] request payload text: %q", protocol.TextPreview(frame.Payload, 200))
		}
		t.sendHTTPError(frame.TunnelID, frame.RequestID, 502, err.Error())
		return
	}

	if protocol.IsWebSocketUpgradeReq(req) {
		t.handleWebSocketReq(frame.TunnelID, frame.RequestID, req, release)
		return
	}

	t.handleNormalHTTPReq(frame.TunnelID, frame.RequestID, req)
}

func (t *Tunnel) handleNormalHTTPReq(tunnelID, requestID uint64, req protocol.HTTPRequest) {
	localPort, ok := t.lookupPort(tunnelID, requestID)
	if !ok {
		return
	}
	log.Printf("[client] proxying %s %s -> localhost:%d (body: %d bytes)", req.Method, req.Path, localPort, len(req.Body))

	if t.debug {
		protocol.DebugLogHeaders("[debug] request", req.Headers)
		if len(req.Body) > 0 {
			log.Printf("[debug] request body: %s", protocol.TextPreview(req.Body, 200))
		}
	}

	targetURL := fmt.Sprintf("http://localhost:%d%s", localPort, req.Path)
	if t.debug {
		log.Printf("[debug] forwarding to %s", targetURL)
	}

	var bodyReader io.Reader = bytes.NewReader(req.Body)
	if req.BodyStreamed {
		bs := &bodyStream{ch: make(chan []byte, protocol.StreamRelayBuf)}
		t.streamsMu.Lock()
		t.bodyStreams[requestID] = bs
		t.streamsMu.Unlock()
		defer func() {
			t.streamsMu.Lock()
			delete(t.bodyStreams, requestID)
			t.streamsMu.Unlock()
		}()
		bodyReader = &bodyStreamReader{s: bs, done: t.done}
	}

	httpReq, err := http.NewRequest(req.Method, targetURL, bodyReader)
	if err != nil {
		log.Printf("[client] create local request error: %v", err)
		t.sendHTTPError(tunnelID, requestID, 502, err.Error())
		return
	}
	if req.BodyStreamed {
		httpReq.ContentLength = contentLength(req.Headers)
	}

	copyHeaders(httpReq, req.Headers)

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		log.Printf("[client] local HTTP error: %v", err)
		t.sendHTTPError(tunnelID, requestID, 502, err.Error())
		return
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, protocol.InlineBodyLimit+1))
	if err != nil {
		log.Printf("[client] read response body error: %v", err)
		t.sendHTTPError(tunnelID, requestID, 502, err.Error())
		return
	}

	statusCode := uint16(httpResp.StatusCode)
	streamed := len(body) > protocol.InlineBodyLimit
	log.Printf("[client] localhost:%d response: %d %s (%d body bytes, streamed: %v)", localPort, statusCode, http.StatusText(int(statusCode)), len(body), streamed)

	if t.debug {
		protocol.DebugLogHeaders("[debug] response", httpResp.Header)
		if len(body) > 0 {
			log.Printf("[debug] response body: %s", protocol.TextPreview(body, 200))
		}
	}

	if !streamed {
		t.writeResponse(tunnelID, requestID, protocol.HTTPResponse{
			StatusCode: statusCode,
			Headers:    httpResp.Header,
			Body:       body,
		})
		return
	}

	t.streamResponseBody(tunnelID, requestID, statusCode, httpResp.Header, body, httpResp.Body)
}

// contentLength extracts Content-Length from tunnel headers, -1 if absent.
func contentLength(headers map[string][]string) int64 {
	for k, values := range headers {
		if strings.EqualFold(k, "Content-Length") && len(values) > 0 {
			if n, err := strconv.ParseInt(values[0], 10, 64); err == nil {
				return n
			}
		}
	}
	return -1
}

// streamResponseBody sends the response headers with the streamed-body marker,
// then pumps the prefix and the rest of the body as stream frames.
func (t *Tunnel) streamResponseBody(tunnelID, requestID uint64, statusCode uint16, headers map[string][]string, prefix []byte, rest io.Reader) {
	// Long transfers are capped with the relays, not the request slots.
	select {
	case t.relaySem <- struct{}{}:
		defer func() { <-t.relaySem }()
	default:
		t.sendHTTPError(tunnelID, requestID, 503, "too many concurrent streamed transfers")
		return
	}

	cancel := make(chan struct{})
	t.streamsMu.Lock()
	t.respCancels[requestID] = cancel
	t.streamsMu.Unlock()
	defer func() {
		t.streamsMu.Lock()
		delete(t.respCancels, requestID)
		t.streamsMu.Unlock()
	}()

	t.writeResponse(tunnelID, requestID, protocol.HTTPResponse{
		StatusCode:   statusCode,
		Headers:      headers,
		BodyStreamed: true,
	})

	defer t.writeFrame(protocol.Frame{
		Type:      protocol.MsgStreamClose,
		TunnelID:  tunnelID,
		RequestID: requestID,
	})

	send := func(data []byte) bool {
		select {
		case <-cancel:
			return false
		case <-t.done:
			return false
		default:
		}
		return t.writeFrame(protocol.Frame{
			Type:      protocol.MsgStreamData,
			TunnelID:  tunnelID,
			RequestID: requestID,
			Payload:   data,
		}) == nil
	}

	if !send(prefix) {
		return
	}
	buf := make([]byte, protocol.StreamBufSize)
	for {
		n, err := rest.Read(buf)
		if n > 0 && !send(buf[:n]) {
			return
		}
		if err != nil {
			return
		}
	}
}

func (t *Tunnel) handleWebSocketReq(tunnelID, requestID uint64, req protocol.HTTPRequest, release func()) {
	// Long-lived relays are capped separately from the HTTP request slots.
	select {
	case t.relaySem <- struct{}{}:
		defer func() { <-t.relaySem }()
	default:
		t.sendHTTPError(tunnelID, requestID, 503, "too many concurrent websocket relays")
		return
	}

	localPort, ok := t.lookupPort(tunnelID, requestID)
	if !ok {
		return
	}
	log.Printf("[client] WebSocket upgrade: %s %s -> localhost:%d (%d bytes headers payload)", req.Method, req.Path, localPort, len(req.Headers))

	addr := fmt.Sprintf("localhost:%d", localPort)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("[client] WebSocket dial error: %v", err)
		t.sendHTTPError(tunnelID, requestID, 502, fmt.Sprintf("dial localhost:%d: %v", localPort, err))
		return
	}

	targetURL := fmt.Sprintf("http://localhost:%d%s", localPort, req.Path)
	httpReq, err := http.NewRequest(req.Method, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		log.Printf("[client] create WebSocket request error: %v", err)
		conn.Close()
		t.sendHTTPError(tunnelID, requestID, 502, err.Error())
		return
	}

	copyHeaders(httpReq, req.Headers)

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := httpReq.Write(conn); err != nil {
		log.Printf("[client] WebSocket write request error: %v", err)
		conn.Close()
		t.sendHTTPError(tunnelID, requestID, 502, err.Error())
		return
	}

	br := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	httpResp, err := http.ReadResponse(br, httpReq)
	if err != nil {
		log.Printf("[client] WebSocket read response error: %v", err)
		conn.Close()
		t.sendHTTPError(tunnelID, requestID, 502, err.Error())
		return
	}

	conn.SetDeadline(time.Time{})

	statusCode := uint16(httpResp.StatusCode)
	log.Printf("[client] WebSocket response: %d %s", statusCode, http.StatusText(int(statusCode)))

	t.writeResponse(tunnelID, requestID, protocol.HTTPResponse{
		StatusCode: statusCode,
		Headers:    httpResp.Header,
		Body:       nil,
	})

	if statusCode != 101 {
		io.Copy(io.Discard, br)
		conn.Close()
		return
	}

	relay := &streamRelay{
		conn: conn,
		done: make(chan struct{}),
	}

	t.relaysMu.Lock()
	t.relays[requestID] = relay
	t.relaysMu.Unlock()

	release()

	if t.debug {
		log.Printf("[debug] WebSocket relay started for request #%d", requestID)
	}

	defer func() {
		relay.closeDone()
		t.relaysMu.Lock()
		delete(t.relays, requestID)
		t.relaysMu.Unlock()
		conn.Close()
		t.writeFrame(protocol.Frame{
			Type:      protocol.MsgStreamClose,
			TunnelID:  tunnelID,
			RequestID: requestID,
		})
		if t.debug {
			log.Printf("[debug] WebSocket relay ended for request #%d", requestID)
		}
	}()

	buf := make([]byte, protocol.StreamBufSize)
	conn.SetReadDeadline(time.Now().Add(protocol.RelayIdleTimeout))
	for {
		n, err := br.Read(buf)
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(protocol.RelayIdleTimeout))
		// No copy needed: writeFrame is synchronous and EncodeFrame
		// copies the payload before buf is reused.
		if err := t.writeFrame(protocol.Frame{
			Type:      protocol.MsgStreamData,
			TunnelID:  tunnelID,
			RequestID: requestID,
			Payload:   buf[:n],
		}); err != nil {
			return
		}
	}
}

func (t *Tunnel) writeResponse(tunnelID, requestID uint64, resp protocol.HTTPResponse) {
	respPayload, err := protocol.EncodeHTTPResponse(resp)
	if err == nil && len(respPayload) > protocol.MaxFrameSize {
		err = fmt.Errorf("encoded response exceeds max frame size: %d > %d", len(respPayload), protocol.MaxFrameSize)
	}
	if err != nil {
		log.Printf("[client] encode response error: %v", err)
		respPayload, _ = protocol.EncodeHTTPResponse(protocol.HTTPResponse{
			StatusCode: 502,
			Headers:    map[string][]string{"Content-Type": {"text/plain"}},
			Body:       []byte("vlgr: response encoding failed: " + err.Error()),
		})
	}
	t.writeFrame(protocol.Frame{
		Type:      protocol.MsgHTTPRes,
		TunnelID:  tunnelID,
		RequestID: requestID,
		Payload:   respPayload,
	})
}

func (t *Tunnel) writeFrame(frame protocol.Frame) error {
	data := protocol.EncodeFrame(frame)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.conn.SetWriteDeadline(time.Now().Add(protocol.WriteWait))
	return t.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (t *Tunnel) sendHTTPError(tunnelID, requestID uint64, statusCode uint16, msg string) {
	t.writeResponse(tunnelID, requestID, protocol.HTTPResponse{
		StatusCode: statusCode,
		Headers:    map[string][]string{"Content-Type": {"text/plain"}},
		Body:       []byte(msg),
	})
}

func (t *Tunnel) Close() {
	t.closeOnce.Do(func() {
		close(t.done)
	})

	if t.controlLn != nil {
		t.controlLn.Close()
	}

	t.relaysMu.Lock()
	for id, relay := range t.relays {
		relay.closeDone()
		relay.conn.Close()
		delete(t.relays, id)
	}
	t.relaysMu.Unlock()

	if t.conn != nil {
		t.writeFrame(protocol.Frame{
			Type: protocol.MsgCloseTunnel,
		})
		t.conn.Close()
	}
}

func (t *Tunnel) PublicURL() string {
	t.mappingsMu.Lock()
	defer t.mappingsMu.Unlock()
	urls := make([]string, 0, len(t.forwards))
	for _, f := range t.forwards {
		urls = append(urls, f.URL)
	}
	return strings.Join(urls, ", ")
}
