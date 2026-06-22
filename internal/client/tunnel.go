package client

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/protocol"
)

const (
	maxBodySize   = 32 << 20
	streamBufSize = 32 * 1024
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type streamRelay struct {
	conn      net.Conn
	requestID uint64
	done      chan struct{}
}

type Tunnel struct {
	serverAddr string
	token      string
	localPort  uint16
	subdomain  string
	useTLS     bool
	debug      bool

	conn      *websocket.Conn
	publicURL string
	tunnelID  uint64

	writeMu sync.Mutex
	done    chan struct{}

	relays   map[uint64]*streamRelay
	relaysMu sync.Mutex
}

func NewTunnel(serverAddr, token string, localPort uint16, subdomain string, useTLS bool) *Tunnel {
	return &Tunnel{
		serverAddr: serverAddr,
		token:      token,
		localPort:  localPort,
		subdomain:  subdomain,
		useTLS:     useTLS,
		done:       make(chan struct{}),
		relays:     make(map[uint64]*streamRelay),
	}
}

func (t *Tunnel) SetDebug(enabled bool) {
	t.debug = enabled
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
	t.conn = conn

	authFrame := protocol.EncodeFrame(protocol.Frame{
		Type:    protocol.MsgAuth,
		Payload: []byte(t.token),
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
	log.Printf("[client] authenticated")

	regPayload := make([]byte, 2)
	binary.BigEndian.PutUint16(regPayload, t.localPort)
	if t.subdomain != "" {
		regPayload = append(regPayload, byte(len(t.subdomain)))
		regPayload = append(regPayload, []byte(t.subdomain)...)
	}

	regFrame := protocol.EncodeFrame(protocol.Frame{
		Type:    protocol.MsgRegister,
		Payload: regPayload,
	})
	if err := conn.WriteMessage(websocket.BinaryMessage, regFrame); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	_, msg, err = conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read register response: %w", err)
	}
	regResp, err := protocol.DecodeFrame(msg)
	if err != nil {
		return fmt.Errorf("decode register response: %w", err)
	}
	if regResp.Type == protocol.MsgRegisterErr {
		return fmt.Errorf("registration failed: %s", string(regResp.Payload))
	}
	if regResp.Type != protocol.MsgRegisterOK {
		return fmt.Errorf("unexpected register response type: 0x%02x", regResp.Type)
	}

	payload := regResp.Payload
	if len(payload) < 1 {
		return fmt.Errorf("register response too short")
	}
	urlLen := payload[0]
	if len(payload) < 1+int(urlLen)+8 {
		return fmt.Errorf("register response truncated")
	}
	t.publicURL = string(payload[1 : 1+urlLen])
	t.tunnelID = binary.BigEndian.Uint64(payload[1+urlLen:])

	log.Printf("[client] tunnel ready: %s -> localhost:%d", t.publicURL, t.localPort)
	return nil
}

func (t *Tunnel) Run() {
	defer t.conn.Close()

	conn := t.conn
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	go t.pingLoop()

	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[client] read error: %v", err)
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
		case protocol.MsgStreamData:
			t.handleStreamData(frame)
		case protocol.MsgStreamClose:
			t.handleStreamClose(frame)
		case protocol.MsgError:
			log.Printf("[client] server error: %s", string(frame.Payload))
		case protocol.MsgCloseTunnel:
			log.Printf("[client] server closed tunnel")
			return
		default:
			log.Printf("[client] unknown message type: 0x%02x", frame.Type)
		}
	}
}

func (t *Tunnel) handleStreamData(frame protocol.Frame) {
	t.relaysMu.Lock()
	relay, ok := t.relays[frame.RequestID]
	t.relaysMu.Unlock()

	if !ok {
		return
	}

	select {
	case <-relay.done:
	default:
		relay.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		relay.conn.Write(frame.Payload)
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
		close(relay.done)
		relay.conn.Close()
	}
}

func (t *Tunnel) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.writeMu.Lock()
			t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			t.conn.WriteMessage(websocket.PingMessage, nil)
			t.writeMu.Unlock()
		case <-t.done:
			return
		}
	}
}

func isWebSocketUpgradeReq(req protocol.HTTPRequest) bool {
	for k, values := range req.Headers {
		if strings.EqualFold(k, "Upgrade") {
			for _, v := range values {
				for _, part := range strings.Split(v, ",") {
					if strings.EqualFold(strings.TrimSpace(part), "websocket") {
						return true
					}
				}
			}
		}
	}
	return false
}

func (t *Tunnel) handleHTTPReq(frame protocol.Frame) {
	req, err := protocol.DecodeHTTPRequest(frame.Payload)
	if err != nil {
		log.Printf("[client] decode HTTP request error: %v (payload %d bytes)", err, len(frame.Payload))
		if t.debug && len(frame.Payload) > 0 {
			preview := len(frame.Payload)
			if preview > 256 {
				preview = 256
			}
			log.Printf("[debug] request payload hex: %x", frame.Payload[:preview])
			textPreview := string(frame.Payload)
			if len(textPreview) > 200 {
				textPreview = textPreview[:200]
			}
			log.Printf("[debug] request payload text: %q", textPreview)
		}
		t.sendHTTPError(frame.RequestID, 502, err.Error())
		return
	}

	if isWebSocketUpgradeReq(req) {
		t.handleWebSocketReq(frame.RequestID, req)
		return
	}

	t.handleNormalHTTPReq(frame.RequestID, req)
}

func (t *Tunnel) handleNormalHTTPReq(requestID uint64, req protocol.HTTPRequest) {
	log.Printf("[client] proxying %s %s (body: %d bytes)", req.Method, req.Path, len(req.Body))

	if t.debug {
		log.Printf("[debug] request headers:")
		for k, values := range req.Headers {
			for _, v := range values {
				log.Printf("[debug]   %s: %s", k, v)
			}
		}
		if len(req.Body) > 0 {
			bodyPreview := string(req.Body)
			if len(bodyPreview) > 200 {
				bodyPreview = bodyPreview[:200] + "..."
			}
			log.Printf("[debug] request body: %s", bodyPreview)
		}
	}

	targetURL := fmt.Sprintf("http://localhost:%d%s", t.localPort, req.Path)
	if t.debug {
		log.Printf("[debug] forwarding to %s", targetURL)
	}

	httpReq, err := http.NewRequest(req.Method, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		log.Printf("[client] create local request error: %v", err)
		t.sendHTTPError(requestID, 502, err.Error())
		return
	}

	for k, values := range req.Headers {
		if k == "Content-Length" || k == "Transfer-Encoding" {
			continue
		}
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Host", fmt.Sprintf("localhost:%d", t.localPort))

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		log.Printf("[client] local HTTP error: %v", err)
		t.sendHTTPError(requestID, 502, err.Error())
		return
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodySize))
	if err != nil {
		log.Printf("[client] read response body error: %v", err)
		t.sendHTTPError(requestID, 502, err.Error())
		return
	}

	statusCode := uint16(httpResp.StatusCode)
	log.Printf("[client] local response: %d %s (%d body bytes)", statusCode, http.StatusText(int(statusCode)), len(body))

	if t.debug {
		log.Printf("[debug] response headers:")
		for k, values := range httpResp.Header {
			for _, v := range values {
				log.Printf("[debug]   %s: %s", k, v)
			}
		}
		if len(body) > 0 {
			bodyPreview := string(body)
			if len(bodyPreview) > 200 {
				bodyPreview = bodyPreview[:200] + "..."
			}
			log.Printf("[debug] response body: %s", bodyPreview)
		}
	}

	headers := make(map[string][]string)
	for k, values := range httpResp.Header {
		if len(values) > 0 {
			headers[k] = values
		}
	}

	t.writeResponse(requestID, protocol.HTTPResponse{
		StatusCode: statusCode,
		Headers:    headers,
		Body:       body,
	})
}

func (t *Tunnel) handleWebSocketReq(requestID uint64, req protocol.HTTPRequest) {
	log.Printf("[client] WebSocket upgrade: %s %s (%d bytes headers payload)", req.Method, req.Path, len(req.Headers))

	addr := fmt.Sprintf("localhost:%d", t.localPort)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("[client] WebSocket dial error: %v", err)
		t.sendHTTPError(requestID, 502, fmt.Sprintf("dial localhost:%d: %v", t.localPort, err))
		return
	}

	targetURL := fmt.Sprintf("http://localhost:%d%s", t.localPort, req.Path)
	httpReq, err := http.NewRequest(req.Method, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		log.Printf("[client] create WebSocket request error: %v", err)
		conn.Close()
		t.sendHTTPError(requestID, 502, err.Error())
		return
	}

	for k, values := range req.Headers {
		if k == "Content-Length" || k == "Transfer-Encoding" {
			continue
		}
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Host = fmt.Sprintf("localhost:%d", t.localPort)

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := httpReq.Write(conn); err != nil {
		log.Printf("[client] WebSocket write request error: %v", err)
		conn.Close()
		t.sendHTTPError(requestID, 502, err.Error())
		return
	}

	br := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	httpResp, err := http.ReadResponse(br, httpReq)
	if err != nil {
		log.Printf("[client] WebSocket read response error: %v", err)
		conn.Close()
		t.sendHTTPError(requestID, 502, err.Error())
		return
	}

	conn.SetDeadline(time.Time{})

	statusCode := uint16(httpResp.StatusCode)
	headers := make(map[string][]string)
	for k, values := range httpResp.Header {
		if len(values) > 0 {
			headers[k] = values
		}
	}

	log.Printf("[client] WebSocket response: %d %s", statusCode, http.StatusText(int(statusCode)))

	t.writeResponse(requestID, protocol.HTTPResponse{
		StatusCode: statusCode,
		Headers:    headers,
		Body:       nil,
	})

	if statusCode != 101 {
		io.Copy(io.Discard, br)
		conn.Close()
		return
	}

	relay := &streamRelay{
		conn:      conn,
		requestID: requestID,
		done:      make(chan struct{}),
	}

	t.relaysMu.Lock()
	t.relays[requestID] = relay
	t.relaysMu.Unlock()

	if t.debug {
		log.Printf("[debug] WebSocket relay started for request #%d", requestID)
	}

	defer func() {
		t.relaysMu.Lock()
		delete(t.relays, requestID)
		t.relaysMu.Unlock()
		close(relay.done)
		conn.Close()
		t.writeFrame(protocol.Frame{
			Type:      protocol.MsgStreamClose,
			TunnelID:  t.tunnelID,
			RequestID: requestID,
		})
		if t.debug {
			log.Printf("[debug] WebSocket relay ended for request #%d", requestID)
		}
	}()

	buf := make([]byte, streamBufSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if err := t.writeFrame(protocol.Frame{
			Type:      protocol.MsgStreamData,
			TunnelID:  t.tunnelID,
			RequestID: requestID,
			Payload:   append([]byte{}, buf[:n]...),
		}); err != nil {
			return
		}
	}
}

func (t *Tunnel) writeResponse(requestID uint64, resp protocol.HTTPResponse) {
	respPayload := protocol.EncodeHTTPResponse(resp)
	t.writeFrame(protocol.Frame{
		Type:      protocol.MsgHTTPRes,
		TunnelID:  t.tunnelID,
		RequestID: requestID,
		Payload:   respPayload,
	})
}

func (t *Tunnel) writeFrame(frame protocol.Frame) error {
	data := protocol.EncodeFrame(frame)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return t.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (t *Tunnel) sendHTTPError(requestID uint64, statusCode uint16, msg string) {
	t.writeResponse(requestID, protocol.HTTPResponse{
		StatusCode: statusCode,
		Headers:    map[string][]string{"Content-Type": {"text/plain"}},
		Body:       []byte(msg),
	})
}

func (t *Tunnel) Close() {
	close(t.done)
	if t.conn != nil {
		t.writeFrame(protocol.Frame{
			Type: protocol.MsgCloseTunnel,
		})
		t.conn.Close()
	}
}

func (t *Tunnel) PublicURL() string {
	return t.publicURL
}
