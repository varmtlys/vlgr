package server

import (
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/protocol"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = 30 * time.Second
	requestTimeout = 30 * time.Second
)

type pendingReq struct {
	response chan protocol.HTTPResponse
	done     chan struct{}
}

type ClientHandler struct {
	conn       *websocket.Conn
	registry   *Registry
	tunnel     *Tunnel
	baseDomain string

	pending map[uint64]*pendingReq
	mu      sync.Mutex
	writeMu sync.Mutex

	done chan struct{}
}

func NewClientHandler(conn *websocket.Conn, registry *Registry, baseDomain string) *ClientHandler {
	return &ClientHandler{
		conn:       conn,
		registry:   registry,
		baseDomain: baseDomain,
		pending:    make(map[uint64]*pendingReq),
		done:       make(chan struct{}),
	}
}

func (h *ClientHandler) Run() {
	defer h.cleanup()

	h.conn.SetReadDeadline(time.Now().Add(pongWait))
	h.conn.SetPongHandler(func(string) error {
		h.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	go h.pingLoop()

	for {
		h.conn.SetReadDeadline(time.Now().Add(pongWait))
		_, msg, err := h.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[handler] read error: %v", err)
			}
			return
		}

		frame, err := protocol.DecodeFrame(msg)
		if err != nil {
			log.Printf("[handler] decode error: %v", err)
			continue
		}

		switch frame.Type {
		case protocol.MsgAuth:
			h.handleAuth(frame)
		case protocol.MsgRegister:
			h.handleRegister(frame)
		case protocol.MsgHTTPRes:
			h.handleHTTPRes(frame)
		case protocol.MsgCloseTunnel:
			log.Printf("[handler] client requested close")
			return
		default:
			log.Printf("[handler] unknown message type: 0x%02x", frame.Type)
		}
	}
}

func (h *ClientHandler) pingLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.writeMu.Lock()
			h.conn.SetWriteDeadline(time.Now().Add(writeWait))
			h.conn.WriteMessage(websocket.PingMessage, nil)
			h.writeMu.Unlock()
		case <-h.done:
			return
		}
	}
}

func (h *ClientHandler) handleAuth(frame protocol.Frame) {
	log.Printf("[handler] client authenticated with token: %s", string(frame.Payload))
	h.writeMessage(protocol.MsgAuthOK, 0, 0, nil)
}

func (h *ClientHandler) handleRegister(frame protocol.Frame) {
	if len(frame.Payload) < 2 {
		h.writeError(0, "invalid register payload")
		return
	}

	localPort := binary.BigEndian.Uint16(frame.Payload[:2])

	var requestedSubdomain string
	if len(frame.Payload) > 3 {
		subdomainLen := frame.Payload[2]
		if int(subdomainLen)+3 <= len(frame.Payload) {
			requestedSubdomain = string(frame.Payload[3 : 3+subdomainLen])
		}
	}

	if requestedSubdomain == "" {
		requestedSubdomain = generateSubdomain()
	}

	tunnel, err := h.registry.Register(requestedSubdomain, localPort, h)
	if err != nil {
		h.writeError(frame.TunnelID, err.Error())
		return
	}

	h.tunnel = tunnel

	publicURL := fmt.Sprintf("%s.%s", requestedSubdomain, h.baseDomain)
	respPayload := append([]byte{byte(len(publicURL))}, []byte(publicURL)...)
	tunnelIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(tunnelIDBytes, tunnel.ID)
	respPayload = append(respPayload, tunnelIDBytes...)

	h.writeMessage(protocol.MsgRegisterOK, tunnel.ID, 0, respPayload)
	log.Printf("[handler] tunnel registered: %s -> localhost:%d", publicURL, localPort)
}

func (h *ClientHandler) handleHTTPRes(frame protocol.Frame) {
	resp, err := protocol.DecodeHTTPResponse(frame.Payload)
	if err != nil {
		log.Printf("[handler] decode HTTP response error: %v", err)
		return
	}

	h.mu.Lock()
	pr, ok := h.pending[frame.RequestID]
	if ok {
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

func (h *ClientHandler) ForwardHTTP(req protocol.HTTPRequest) (protocol.HTTPResponse, error) {
	requestID := nextRequestID()

	pr := &pendingReq{
		response: make(chan protocol.HTTPResponse, 1),
		done:     make(chan struct{}),
	}

	h.mu.Lock()
	h.pending[requestID] = pr
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.pending, requestID)
		h.mu.Unlock()
		close(pr.done)
	}()

	payload := protocol.EncodeHTTPRequest(req)
	tunnelID := uint64(0)
	if h.tunnel != nil {
		tunnelID = h.tunnel.ID
	}

	if err := h.writeMessage(protocol.MsgHTTPReq, tunnelID, requestID, payload); err != nil {
		return protocol.HTTPResponse{}, fmt.Errorf("forward: write error: %w", err)
	}

	select {
	case resp := <-pr.response:
		return resp, nil
	case <-time.After(requestTimeout):
		return protocol.HTTPResponse{}, fmt.Errorf("forward: timeout after %v", requestTimeout)
	}
}

func (h *ClientHandler) writeMessage(msgType byte, tunnelID, requestID uint64, payload []byte) error {
	frame := protocol.EncodeFrame(protocol.Frame{
		Type:      msgType,
		TunnelID:  tunnelID,
		RequestID: requestID,
		Payload:   payload,
	})

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	h.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return h.conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (h *ClientHandler) writeError(requestID uint64, msg string) {
	h.writeMessage(protocol.MsgError, 0, requestID, []byte(msg))
}

func (h *ClientHandler) cleanup() {
	if h.tunnel != nil {
		h.registry.Unregister(h.tunnel.Subdomain)
		log.Printf("[handler] tunnel %s unregistered", h.tunnel.Subdomain)
	}
	close(h.done)
	h.conn.Close()
}
