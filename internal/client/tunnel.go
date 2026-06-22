package client

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"vlgr/internal/protocol"
)

const maxBodySize = 32 << 20

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
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
}

func NewTunnel(serverAddr, token string, localPort uint16, subdomain string, useTLS bool) *Tunnel {
	return &Tunnel{
		serverAddr: serverAddr,
		token:      token,
		localPort:  localPort,
		subdomain:  subdomain,
		useTLS:     useTLS,
		done:       make(chan struct{}),
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
	case protocol.MsgError:
			log.Printf("[client] server error: %s", string(frame.Payload))
		case protocol.MsgCloseTunnel:
			log.Printf("[client] server closed tunnel")
			return
		}
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
		t.sendHTTPError(frame.RequestID, 502, err.Error())
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
		t.sendHTTPError(frame.RequestID, 502, err.Error())
		return
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodySize))
	if err != nil {
		log.Printf("[client] read response body error: %v", err)
		t.sendHTTPError(frame.RequestID, 502, err.Error())
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

	t.writeResponse(frame.RequestID, protocol.HTTPResponse{
		StatusCode: statusCode,
		Headers:    headers,
		Body:       body,
	})
}

func (t *Tunnel) writeResponse(requestID uint64, resp protocol.HTTPResponse) {
	respPayload := protocol.EncodeHTTPResponse(resp)

	frame := protocol.EncodeFrame(protocol.Frame{
		Type:      protocol.MsgHTTPRes,
		TunnelID:  t.tunnelID,
		RequestID: requestID,
		Payload:   respPayload,
	})

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := t.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		log.Printf("[client] write HTTP response error: %v", err)
	}
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
		closeFrame := protocol.EncodeFrame(protocol.Frame{
			Type: protocol.MsgCloseTunnel,
		})
		t.writeMu.Lock()
		t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		t.conn.WriteMessage(websocket.BinaryMessage, closeFrame)
		t.conn.Close()
		t.writeMu.Unlock()
	}
}

func (t *Tunnel) PublicURL() string {
	return t.publicURL
}
