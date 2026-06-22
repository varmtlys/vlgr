package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
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
)

const HeaderSize = 21

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

func writeString(buf *bytes.Buffer, s string) {
	b := []byte(s)
	binary.Write(buf, binary.BigEndian, uint16(len(b)))
	buf.Write(b)
}

func readString(reader *bytes.Reader) (string, error) {
	var length uint16
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return "", err
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func EncodeHTTPRequest(req HTTPRequest) []byte {
	var buf bytes.Buffer

	writeString(&buf, req.Method)
	writeString(&buf, req.Path)

	binary.Write(&buf, binary.BigEndian, uint32(len(req.Headers)))
	for k, values := range req.Headers {
		writeString(&buf, k)
		binary.Write(&buf, binary.BigEndian, uint32(len(values)))
		for _, v := range values {
			writeString(&buf, v)
		}
	}

	binary.Write(&buf, binary.BigEndian, uint32(len(req.Body)))
	if len(req.Body) > 0 {
		buf.Write(req.Body)
	}

	return buf.Bytes()
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

	var headerCount uint32
	if err := binary.Read(reader, binary.BigEndian, &headerCount); err != nil {
		return req, fmt.Errorf("read header count: %w", err)
	}

	req.Headers = make(map[string][]string, headerCount)
	for i := uint32(0); i < headerCount; i++ {
		k, err := readString(reader)
		if err != nil {
			return req, fmt.Errorf("read header key: %w", err)
		}
		var valueCount uint32
		if err := binary.Read(reader, binary.BigEndian, &valueCount); err != nil {
			return req, fmt.Errorf("read header value count: %w", err)
		}
		values := make([]string, valueCount)
		for j := uint32(0); j < valueCount; j++ {
			v, err := readString(reader)
			if err != nil {
				return req, fmt.Errorf("read header value: %w", err)
			}
			values[j] = v
		}
		req.Headers[k] = values
	}

	var bodyLen uint32
	if err := binary.Read(reader, binary.BigEndian, &bodyLen); err != nil {
		return req, fmt.Errorf("read body length: %w", err)
	}
	req.Body = make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, req.Body); err != nil {
		return req, fmt.Errorf("read body: %w", err)
	}

	return req, nil
}

func EncodeHTTPResponse(resp HTTPResponse) []byte {
	var buf bytes.Buffer

	binary.Write(&buf, binary.BigEndian, resp.StatusCode)

	binary.Write(&buf, binary.BigEndian, uint32(len(resp.Headers)))
	for k, values := range resp.Headers {
		writeString(&buf, k)
		binary.Write(&buf, binary.BigEndian, uint32(len(values)))
		for _, v := range values {
			writeString(&buf, v)
		}
	}

	binary.Write(&buf, binary.BigEndian, uint32(len(resp.Body)))
	if len(resp.Body) > 0 {
		buf.Write(resp.Body)
	}

	return buf.Bytes()
}

func DecodeHTTPResponse(data []byte) (HTTPResponse, error) {
	reader := bytes.NewReader(data)
	var resp HTTPResponse

	if err := binary.Read(reader, binary.BigEndian, &resp.StatusCode); err != nil {
		return resp, fmt.Errorf("read status code: %w", err)
	}

	var headerCount uint32
	if err := binary.Read(reader, binary.BigEndian, &headerCount); err != nil {
		return resp, fmt.Errorf("read header count: %w", err)
	}

	resp.Headers = make(map[string][]string, headerCount)
	for i := uint32(0); i < headerCount; i++ {
		k, err := readString(reader)
		if err != nil {
			return resp, fmt.Errorf("read header key: %w", err)
		}
		var valueCount uint32
		if err := binary.Read(reader, binary.BigEndian, &valueCount); err != nil {
			return resp, fmt.Errorf("read header value count: %w", err)
		}
		values := make([]string, valueCount)
		for j := uint32(0); j < valueCount; j++ {
			v, err := readString(reader)
			if err != nil {
				return resp, fmt.Errorf("read header value: %w", err)
			}
			values[j] = v
		}
		resp.Headers[k] = values
	}

	var bodyLen uint32
	if err := binary.Read(reader, binary.BigEndian, &bodyLen); err != nil {
		return resp, fmt.Errorf("read body length: %w", err)
	}
	resp.Body = make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, resp.Body); err != nil {
		return resp, fmt.Errorf("read body: %w", err)
	}

	return resp, nil
}
