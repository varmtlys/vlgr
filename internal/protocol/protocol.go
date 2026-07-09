package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	MsgAuth        byte = 0x01
	MsgAuthOK      byte = 0x02
	MsgAuthErr     byte = 0x03
	MsgRegister    byte = 0x04
	MsgRegisterOK  byte = 0x05
	MsgRegisterErr byte = 0x06
	MsgHTTPReq     byte = 0x07
	MsgHTTPRes     byte = 0x08
	MsgCloseTunnel byte = 0x09
	MsgError       byte = 0x0A
	MsgStreamData  byte = 0x0B
	MsgStreamClose byte = 0x0C

	HeaderSize    = 21
	MaxBodySize   = 32 << 20
	StreamBufSize = 32 * 1024

	MaxHeaders         = 256
	MaxValuesPerHeader = 64

	WriteWait        = 10 * time.Second
	PongWait         = 60 * time.Second
	PingPeriod       = 30 * time.Second
	RequestTimeout   = 30 * time.Second
	RelayIdleTimeout = 5 * time.Minute

	MaxTunnelsPerClient = 20
	StreamRelayBuf      = 256
	StreamSendTimeout   = 10 * time.Second
)

type Frame struct {
	Type      byte
	TunnelID  uint64
	RequestID uint64
	Payload   []byte
}

type HTTPRequest struct {
	Method  string
	Path    string
	Headers map[string][]string
	Body    []byte
}

type HTTPResponse struct {
	StatusCode uint16
	Headers    map[string][]string
	Body       []byte
}

func EncodeFrame(f Frame) []byte {
	buf := make([]byte, HeaderSize+len(f.Payload))
	buf[0] = f.Type
	binary.BigEndian.PutUint64(buf[1:9], f.TunnelID)
	binary.BigEndian.PutUint64(buf[9:17], f.RequestID)
	binary.BigEndian.PutUint32(buf[17:21], uint32(len(f.Payload)))
	copy(buf[21:], f.Payload)
	return buf
}

func DecodeFrame(data []byte) (Frame, error) {
	if len(data) < HeaderSize {
		return Frame{}, fmt.Errorf("frame too short: %d bytes, need at least %d", len(data), HeaderSize)
	}
	payloadLen := binary.BigEndian.Uint32(data[17:21])
	if payloadLen > MaxBodySize {
		return Frame{}, fmt.Errorf("frame payload exceeds max: %d > %d", payloadLen, MaxBodySize)
	}
	if len(data) < HeaderSize+int(payloadLen) {
		return Frame{}, fmt.Errorf("truncated payload: expected %d, got %d", payloadLen, len(data)-HeaderSize)
	}
	return Frame{
		Type:      data[0],
		TunnelID:  binary.BigEndian.Uint64(data[1:9]),
		RequestID: binary.BigEndian.Uint64(data[9:17]),
		Payload:   data[21 : 21+payloadLen],
	}, nil
}

const maxStringLen = 65535

func writeString(buf *bytes.Buffer, s string) error {
	if len(s) > maxStringLen {
		return fmt.Errorf("string too long: %d > %d", len(s), maxStringLen)
	}
	binary.Write(buf, binary.BigEndian, uint16(len(s)))
	buf.WriteString(s)
	return nil
}

func readString(reader *bytes.Reader) (string, error) {
	var length uint16
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return "", err
	}
	if int(length) > reader.Len() {
		return "", fmt.Errorf("string length %d exceeds remaining payload %d", length, reader.Len())
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeHeaders(buf *bytes.Buffer, headers map[string][]string) error {
	binary.Write(buf, binary.BigEndian, uint32(len(headers)))
	for k, values := range headers {
		if err := writeString(buf, k); err != nil {
			return fmt.Errorf("header key: %w", err)
		}
		binary.Write(buf, binary.BigEndian, uint32(len(values)))
		for _, v := range values {
			if err := writeString(buf, v); err != nil {
				return fmt.Errorf("header %q value: %w", k, err)
			}
		}
	}
	return nil
}

func readHeaders(reader *bytes.Reader) (map[string][]string, error) {
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read header count: %w", err)
	}
	if count > MaxHeaders {
		return nil, fmt.Errorf("too many headers: %d > %d", count, MaxHeaders)
	}
	if int(count)*2 > reader.Len() {
		return nil, fmt.Errorf("header count %d cannot fit in %d remaining bytes", count, reader.Len())
	}
	headers := make(map[string][]string, count)
	for i := uint32(0); i < count; i++ {
		k, err := readString(reader)
		if err != nil {
			return nil, fmt.Errorf("read header key: %w", err)
		}
		var valueCount uint32
		if err := binary.Read(reader, binary.BigEndian, &valueCount); err != nil {
			return nil, fmt.Errorf("read header value count: %w", err)
		}
		if valueCount > MaxValuesPerHeader {
			return nil, fmt.Errorf("too many values for header %q: %d > %d", k, valueCount, MaxValuesPerHeader)
		}
		values := make([]string, valueCount)
		for j := uint32(0); j < valueCount; j++ {
			v, err := readString(reader)
			if err != nil {
				return nil, fmt.Errorf("read header value: %w", err)
			}
			values[j] = v
		}
		headers[k] = values
	}
	return headers, nil
}

func writeBody(buf *bytes.Buffer, body []byte) {
	binary.Write(buf, binary.BigEndian, uint32(len(body)))
	buf.Write(body)
}

func readBody(reader *bytes.Reader) ([]byte, error) {
	var bodyLen uint32
	if err := binary.Read(reader, binary.BigEndian, &bodyLen); err != nil {
		return nil, fmt.Errorf("read body length: %w", err)
	}
	if bodyLen > MaxBodySize {
		return nil, fmt.Errorf("body too large: %d > %d", bodyLen, MaxBodySize)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

func EncodeHTTPRequest(req HTTPRequest) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeString(&buf, req.Method); err != nil {
		return nil, fmt.Errorf("method: %w", err)
	}
	if err := writeString(&buf, req.Path); err != nil {
		return nil, fmt.Errorf("path: %w", err)
	}
	if err := writeHeaders(&buf, req.Headers); err != nil {
		return nil, err
	}
	writeBody(&buf, req.Body)
	return buf.Bytes(), nil
}

func DecodeHTTPRequest(data []byte) (HTTPRequest, error) {
	reader := bytes.NewReader(data)
	var req HTTPRequest

	method, err := readString(reader)
	if err != nil {
		return req, fmt.Errorf("read method: %w", err)
	}
	req.Method = method

	path, err := readString(reader)
	if err != nil {
		return req, fmt.Errorf("read path: %w", err)
	}
	req.Path = path

	headers, err := readHeaders(reader)
	if err != nil {
		return req, err
	}
	req.Headers = headers

	if req.Body, err = readBody(reader); err != nil {
		return req, err
	}
	return req, nil
}

func EncodeHTTPResponse(resp HTTPResponse) ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, resp.StatusCode)
	if err := writeHeaders(&buf, resp.Headers); err != nil {
		return nil, err
	}
	writeBody(&buf, resp.Body)
	return buf.Bytes(), nil
}

func DecodeHTTPResponse(data []byte) (HTTPResponse, error) {
	reader := bytes.NewReader(data)
	var resp HTTPResponse

	if err := binary.Read(reader, binary.BigEndian, &resp.StatusCode); err != nil {
		return resp, fmt.Errorf("read status code: %w", err)
	}

	headers, err := readHeaders(reader)
	if err != nil {
		return resp, err
	}
	resp.Headers = headers

	if resp.Body, err = readBody(reader); err != nil {
		return resp, err
	}
	return resp, nil
}

func containsWebSocket(values []string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "websocket") {
				return true
			}
		}
	}
	return false
}

func IsWebSocketUpgrade(r *http.Request) bool {
	return containsWebSocket(r.Header["Upgrade"])
}

func IsWebSocketUpgradeReq(req HTTPRequest) bool {
	for k, values := range req.Headers {
		if strings.EqualFold(k, "Upgrade") && containsWebSocket(values) {
			return true
		}
	}
	return false
}

// Register payload: [port uint16][subdomainLen byte][subdomain...] (subdomain optional).
// RegisterOK payload: [urlLen byte][publicURL...][tunnelID uint64].
// Encoded and decoded here so client and server share one wire-format definition.

const MaxSubdomainLen = 63

func EncodeRegister(port uint16, subdomain string) ([]byte, error) {
	if len(subdomain) > MaxSubdomainLen {
		return nil, fmt.Errorf("subdomain too long: %d > %d", len(subdomain), MaxSubdomainLen)
	}
	payload := make([]byte, 2, 3+len(subdomain))
	binary.BigEndian.PutUint16(payload, port)
	if subdomain != "" {
		payload = append(payload, byte(len(subdomain)))
		payload = append(payload, subdomain...)
	}
	return payload, nil
}

func DecodeRegister(payload []byte) (port uint16, subdomain string, err error) {
	if len(payload) < 2 {
		return 0, "", fmt.Errorf("register payload too short: %d bytes", len(payload))
	}
	port = binary.BigEndian.Uint16(payload[:2])
	if len(payload) >= 3 {
		n := int(payload[2])
		if n > MaxSubdomainLen {
			return 0, "", fmt.Errorf("subdomain too long: %d > %d", n, MaxSubdomainLen)
		}
		if len(payload) < 3+n {
			return 0, "", fmt.Errorf("truncated register payload")
		}
		subdomain = string(payload[3 : 3+n])
	}
	return port, subdomain, nil
}

func EncodeRegisterOK(publicURL string, tunnelID uint64) ([]byte, error) {
	if len(publicURL) > 255 {
		return nil, fmt.Errorf("public URL too long: %d > 255", len(publicURL))
	}
	payload := make([]byte, 0, 9+len(publicURL))
	payload = append(payload, byte(len(publicURL)))
	payload = append(payload, publicURL...)
	payload = binary.BigEndian.AppendUint64(payload, tunnelID)
	return payload, nil
}

func DecodeRegisterOK(payload []byte) (publicURL string, tunnelID uint64, err error) {
	if len(payload) < 1 {
		return "", 0, fmt.Errorf("register response too short")
	}
	n := int(payload[0])
	if len(payload) < 1+n+8 {
		return "", 0, fmt.Errorf("register response truncated")
	}
	return string(payload[1 : 1+n]), binary.BigEndian.Uint64(payload[1+n:]), nil
}

// Auth payload: [tokenLen uint16][token][versionLen uint16][version].
// AuthOK payload: [versionLen uint16][serverVersion].
// Exchanged during the handshake so both sides can compare versions and warn
// on mismatch. version may be empty for older clients.

const MaxVersionLen = 255

func EncodeAuth(token, clientVersion string) ([]byte, error) {
	if len(token) > maxStringLen {
		return nil, fmt.Errorf("token too long: %d > %d", len(token), maxStringLen)
	}
	if len(clientVersion) > MaxVersionLen {
		return nil, fmt.Errorf("version too long: %d > %d", len(clientVersion), MaxVersionLen)
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(len(token)))
	buf.WriteString(token)
	binary.Write(&buf, binary.BigEndian, uint16(len(clientVersion)))
	buf.WriteString(clientVersion)
	return buf.Bytes(), nil
}

func DecodeAuth(payload []byte) (token, clientVersion string, err error) {
	reader := bytes.NewReader(payload)
	var tokenLen uint16
	if err = binary.Read(reader, binary.BigEndian, &tokenLen); err != nil {
		return "", "", fmt.Errorf("read token length: %w", err)
	}
	if int(tokenLen) > reader.Len() {
		return "", "", fmt.Errorf("token length %d exceeds remaining %d", tokenLen, reader.Len())
	}
	tokenBytes := make([]byte, tokenLen)
	if _, err = io.ReadFull(reader, tokenBytes); err != nil {
		return "", "", fmt.Errorf("read token: %w", err)
	}
	token = string(tokenBytes)

	var verLen uint16
	if err = binary.Read(reader, binary.BigEndian, &verLen); err != nil {
		return token, "", nil
	}
	if int(verLen) > reader.Len() {
		return token, "", fmt.Errorf("version length %d exceeds remaining %d", verLen, reader.Len())
	}
	verBytes := make([]byte, verLen)
	if _, err = io.ReadFull(reader, verBytes); err != nil {
		return token, "", fmt.Errorf("read version: %w", err)
	}
	clientVersion = string(verBytes)
	return token, clientVersion, nil
}

func EncodeAuthOK(serverVersion string) ([]byte, error) {
	if len(serverVersion) > MaxVersionLen {
		return nil, fmt.Errorf("version too long: %d > %d", len(serverVersion), MaxVersionLen)
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(len(serverVersion)))
	buf.WriteString(serverVersion)
	return buf.Bytes(), nil
}

func DecodeAuthOK(payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", nil
	}
	reader := bytes.NewReader(payload)
	var verLen uint16
	if err := binary.Read(reader, binary.BigEndian, &verLen); err != nil {
		return "", fmt.Errorf("read version length: %w", err)
	}
	if int(verLen) > reader.Len() {
		return "", fmt.Errorf("version length %d exceeds remaining %d", verLen, reader.Len())
	}
	verBytes := make([]byte, verLen)
	if _, err := io.ReadFull(reader, verBytes); err != nil {
		return "", fmt.Errorf("read version: %w", err)
	}
	return string(verBytes), nil
}

// PingLoop sends WebSocket pings every PingPeriod until done closes or a
// write fails. Shared by client and server (DRY).
func PingLoop(conn *websocket.Conn, writeMu *sync.Mutex, done <-chan struct{}) {
	ticker := time.NewTicker(PingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			writeMu.Lock()
			conn.SetWriteDeadline(time.Now().Add(WriteWait))
			err := conn.WriteMessage(websocket.PingMessage, nil)
			writeMu.Unlock()
			if err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

// HexPreview and TextPreview truncate payloads for debug logging.
func HexPreview(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	return fmt.Sprintf("%x", b)
}

func TextPreview(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// DebugLogHeaders dumps headers line by line with the given log prefix.
func DebugLogHeaders(prefix string, headers map[string][]string) {
	log.Printf("%s headers:", prefix)
	for k, values := range headers {
		for _, v := range values {
			log.Printf("%s   %s: %s", prefix, k, v)
		}
	}
}
