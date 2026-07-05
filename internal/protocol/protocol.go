package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
	binary.Write(&buf, binary.BigEndian, uint32(len(req.Body)))
	buf.Write(req.Body)
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

	var bodyLen uint32
	if err := binary.Read(reader, binary.BigEndian, &bodyLen); err != nil {
		return req, fmt.Errorf("read body length: %w", err)
	}
	if bodyLen > MaxBodySize {
		return req, fmt.Errorf("body too large: %d > %d", bodyLen, MaxBodySize)
	}
	req.Body = make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, req.Body); err != nil {
		return req, fmt.Errorf("read body: %w", err)
	}

	return req, nil
}

func EncodeHTTPResponse(resp HTTPResponse) ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, resp.StatusCode)
	if err := writeHeaders(&buf, resp.Headers); err != nil {
		return nil, err
	}
	binary.Write(&buf, binary.BigEndian, uint32(len(resp.Body)))
	buf.Write(resp.Body)
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

	var bodyLen uint32
	if err := binary.Read(reader, binary.BigEndian, &bodyLen); err != nil {
		return resp, fmt.Errorf("read body length: %w", err)
	}
	if bodyLen > MaxBodySize {
		return resp, fmt.Errorf("body too large: %d > %d", bodyLen, MaxBodySize)
	}
	resp.Body = make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, resp.Body); err != nil {
		return resp, fmt.Errorf("read body: %w", err)
	}

	return resp, nil
}

func IsWebSocketUpgrade(r *http.Request) bool {
	for _, v := range r.Header["Upgrade"] {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "websocket") {
				return true
			}
		}
	}
	return false
}

func IsWebSocketUpgradeReq(req HTTPRequest) bool {
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
