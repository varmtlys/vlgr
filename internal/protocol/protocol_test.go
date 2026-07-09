package protocol

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestEncodeDecodeFrame_Roundtrip(t *testing.T) {
	original := Frame{
		Type:      MsgHTTPReq,
		TunnelID:  42,
		RequestID: 100,
		Payload:   []byte("hello world"),
	}
	encoded := EncodeFrame(original)
	decoded, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if decoded.Type != original.Type {
		t.Errorf("Type: want %d, got %d", original.Type, decoded.Type)
	}
	if decoded.TunnelID != original.TunnelID {
		t.Errorf("TunnelID: want %d, got %d", original.TunnelID, decoded.TunnelID)
	}
	if decoded.RequestID != original.RequestID {
		t.Errorf("RequestID: want %d, got %d", original.RequestID, decoded.RequestID)
	}
	if !bytes.Equal(decoded.Payload, original.Payload) {
		t.Errorf("Payload: want %q, got %q", original.Payload, decoded.Payload)
	}
}

func TestEncodeDecodeFrame_EmptyPayload(t *testing.T) {
	original := Frame{
		Type:      MsgAuthOK,
		TunnelID:  0,
		RequestID: 0,
		Payload:   nil,
	}
	encoded := EncodeFrame(original)
	if len(encoded) != HeaderSize {
		t.Errorf("encoded length: want %d, got %d", HeaderSize, len(encoded))
	}
	decoded, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if len(decoded.Payload) != 0 {
		t.Errorf("payload: want empty, got %d bytes", len(decoded.Payload))
	}
}

func TestDecodeFrame_TooShort(t *testing.T) {
	_, err := DecodeFrame(make([]byte, HeaderSize-1))
	if err == nil {
		t.Error("expected error for short frame")
	}
}

func TestDecodeFrame_PayloadExceedsMax(t *testing.T) {
	buf := make([]byte, HeaderSize)
	buf[0] = MsgHTTPReq
	binaryWrite(buf[17:21], MaxFrameSize+1)
	_, err := DecodeFrame(buf)
	if err == nil {
		t.Error("expected error for oversized payload")
	}
}

func TestDecodeFrame_MaxBodyPlusHeadersFits(t *testing.T) {
	// A max-size body plus encoded headers must fit within MaxFrameSize,
	// otherwise one large request kills the whole tunnel connection.
	payloadLen := MaxBodySize + 100*1024
	buf := make([]byte, HeaderSize+payloadLen)
	buf[0] = MsgHTTPReq
	binaryWrite(buf[17:21], uint32(payloadLen))
	if _, err := DecodeFrame(buf); err != nil {
		t.Fatalf("frame with max body + headers should decode: %v", err)
	}
}

func TestDecodeFrame_TruncatedPayload(t *testing.T) {
	buf := make([]byte, HeaderSize+10)
	buf[0] = MsgHTTPReq
	binaryWrite(buf[17:21], 100)
	_, err := DecodeFrame(buf)
	if err == nil {
		t.Error("expected error for truncated payload")
	}
}

func TestDecodeFrame_MaxPayload(t *testing.T) {
	buf := make([]byte, HeaderSize+MaxBodySize)
	buf[0] = MsgHTTPReq
	binaryWrite(buf[17:21], MaxBodySize)
	f, err := DecodeFrame(buf)
	if err != nil {
		t.Fatalf("DecodeFrame at max payload failed: %v", err)
	}
	if len(f.Payload) != MaxBodySize {
		t.Errorf("payload length: want %d, got %d", MaxBodySize, len(f.Payload))
	}
}

func TestEncodeDecodeHTTPRequest_Roundtrip(t *testing.T) {
	original := HTTPRequest{
		Method: "POST",
		Path:   "/api/v1/data?format=json",
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"Accept":       {"application/json", "text/plain"},
		},
		Body: []byte(`{"key":"value"}`),
	}
	encoded, _ := EncodeHTTPRequest(original)
	decoded, err := DecodeHTTPRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeHTTPRequest failed: %v", err)
	}
	if decoded.Method != original.Method {
		t.Errorf("Method: want %q, got %q", original.Method, decoded.Method)
	}
	if decoded.Path != original.Path {
		t.Errorf("Path: want %q, got %q", original.Path, decoded.Path)
	}
	if len(decoded.Headers) != len(original.Headers) {
		t.Errorf("Headers count: want %d, got %d", len(original.Headers), len(decoded.Headers))
	}
	for k, v := range original.Headers {
		got, ok := decoded.Headers[k]
		if !ok {
			t.Errorf("missing header %q", k)
			continue
		}
		if len(got) != len(v) {
			t.Errorf("header %q values count: want %d, got %d", k, len(v), len(got))
		}
	}
	if !bytes.Equal(decoded.Body, original.Body) {
		t.Errorf("Body: want %q, got %q", original.Body, decoded.Body)
	}
}

func TestEncodeDecodeHTTPRequest_EmptyBody(t *testing.T) {
	original := HTTPRequest{
		Method:  "GET",
		Path:    "/",
		Headers: map[string][]string{},
		Body:    nil,
	}
	encoded, _ := EncodeHTTPRequest(original)
	decoded, err := DecodeHTTPRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeHTTPRequest failed: %v", err)
	}
	if decoded.Method != "GET" {
		t.Errorf("Method: want GET, got %q", decoded.Method)
	}
	if len(decoded.Body) != 0 {
		t.Errorf("Body length: want 0, got %d", len(decoded.Body))
	}
}

func TestDecodeHTTPRequest_BodyTooLarge(t *testing.T) {
	var buf bytes.Buffer
	writeString(&buf, "GET")
	writeString(&buf, "/")
	binaryWriteTo(&buf, uint32(0))
	binaryWriteTo(&buf, MaxBodySize+1)
	_, err := DecodeHTTPRequest(buf.Bytes())
	if err == nil {
		t.Error("expected error for oversized body")
	}
}

func TestEncodeDecodeHTTPResponse_Roundtrip(t *testing.T) {
	original := HTTPResponse{
		StatusCode: 200,
		Headers: map[string][]string{
			"Content-Type":   {"text/html; charset=utf-8"},
			"Content-Length": {"13"},
			"Set-Cookie":     {"a=1", "b=2"},
		},
		Body: []byte("<html>OK</html>"),
	}
	encoded, _ := EncodeHTTPResponse(original)
	decoded, err := DecodeHTTPResponse(encoded)
	if err != nil {
		t.Fatalf("DecodeHTTPResponse failed: %v", err)
	}
	if decoded.StatusCode != original.StatusCode {
		t.Errorf("StatusCode: want %d, got %d", original.StatusCode, decoded.StatusCode)
	}
	if len(decoded.Headers) != len(original.Headers) {
		t.Errorf("Headers count: want %d, got %d", len(original.Headers), len(decoded.Headers))
	}
	if !bytes.Equal(decoded.Body, original.Body) {
		t.Errorf("Body: want %q, got %q", original.Body, decoded.Body)
	}
}

func TestEncodeDecodeHTTPResponse_ErrorStatus(t *testing.T) {
	original := HTTPResponse{
		StatusCode: 502,
		Headers:    map[string][]string{"Content-Type": {"text/plain"}},
		Body:       []byte("Bad Gateway"),
	}
	encoded, _ := EncodeHTTPResponse(original)
	decoded, err := DecodeHTTPResponse(encoded)
	if err != nil {
		t.Fatalf("DecodeHTTPResponse failed: %v", err)
	}
	if decoded.StatusCode != 502 {
		t.Errorf("StatusCode: want 502, got %d", decoded.StatusCode)
	}
	if string(decoded.Body) != "Bad Gateway" {
		t.Errorf("Body: want %q, got %q", "Bad Gateway", decoded.Body)
	}
}

func TestDecodeHTTPResponse_BodyTooLarge(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint16(200))
	binaryWriteTo(&buf, uint32(0))
	binaryWriteTo(&buf, MaxBodySize+1)
	_, err := DecodeHTTPResponse(buf.Bytes())
	if err == nil {
		t.Error("expected error for oversized body")
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name     string
		header   http.Header
		expected bool
	}{
		{"plain upgrade", http.Header{"Upgrade": {"websocket"}}, true},
		{"mixed case", http.Header{"Upgrade": {"WebSocket"}}, true},
		{"with spaces", http.Header{"Upgrade": {" websocket "}}, true},
		{"multiple values", http.Header{"Upgrade": {"h2c, websocket"}}, true},
		{"no upgrade", http.Header{}, false},
		{"other upgrade", http.Header{"Upgrade": {"h2c"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Header: tt.header}
			if got := IsWebSocketUpgrade(r); got != tt.expected {
				t.Errorf("IsWebSocketUpgrade: want %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestIsWebSocketUpgradeReq(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string][]string
		expected bool
	}{
		{"websocket upgrade", map[string][]string{"Upgrade": {"websocket"}}, true},
		{"mixed case key", map[string][]string{"upgrade": {"websocket"}}, true},
		{"no upgrade header", map[string][]string{"Host": {"domain.com"}}, false},
		{"other value", map[string][]string{"Upgrade": {"h2c"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := HTTPRequest{Headers: tt.headers}
			if got := IsWebSocketUpgradeReq(req); got != tt.expected {
				t.Errorf("IsWebSocketUpgradeReq: want %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestReadHeaders_HighCount(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint32(10000))
	data := buf.Bytes()
	reader := bytes.NewReader(data)
	_, err := readHeaders(reader)
	if err == nil {
		t.Error("expected error for truncated header data")
	}
}

func TestReadHeaders_ZeroCount(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint32(0))
	headers, err := readHeaders(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readHeaders with 0 count failed: %v", err)
	}
	if len(headers) != 0 {
		t.Errorf("expected 0 headers, got %d", len(headers))
	}
}

func TestReadString_MaxLength(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint16(65535))
	_, err := readString(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Error("expected error for truncated max-length string")
	}
}

func TestReadString_ZeroLength(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint16(0))
	s, err := readString(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readString with 0 length failed: %v", err)
	}
	if s != "" {
		t.Errorf("expected empty string, got %q", s)
	}
}

func TestWriteHeaders_Empty(t *testing.T) {
	var buf bytes.Buffer
	writeHeaders(&buf, map[string][]string{})
	data := buf.Bytes()
	reader := bytes.NewReader(data)
	headers, err := readHeaders(reader)
	if err != nil {
		t.Fatalf("readHeaders failed: %v", err)
	}
	if len(headers) != 0 {
		t.Errorf("expected 0 headers, got %d", len(headers))
	}
}

func TestDecodeHTTPRequest_Truncated(t *testing.T) {
	_, err := DecodeHTTPRequest([]byte{0x00})
	if err == nil {
		t.Error("expected error for truncated request data")
	}
}

func TestDecodeHTTPResponse_Truncated(t *testing.T) {
	_, err := DecodeHTTPResponse([]byte{0x00})
	if err == nil {
		t.Error("expected error for truncated response data")
	}
}

func TestReadHeaders_TruncatedKey(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint32(1))
	binaryWriteTo(&buf, uint16(5))
	buf.Write([]byte("sh"))
	_, err := readHeaders(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Error("expected error for truncated key")
	}
}

func TestReadHeaders_TruncatedValue(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint32(1))
	key := "X-Test"
	binaryWriteTo(&buf, uint16(len(key)))
	buf.Write([]byte(key))
	binaryWriteTo(&buf, uint32(2))
	binaryWriteTo(&buf, uint16(10))
	buf.Write([]byte("short"))
	_, err := readHeaders(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Error("expected error for truncated header value")
	}
}

func TestEncodeHTTPResponse_NoBody(t *testing.T) {
	resp := HTTPResponse{
		StatusCode: 404,
		Headers:    map[string][]string{"X-Empty": {}},
		Body:       nil,
	}
	encoded, _ := EncodeHTTPResponse(resp)
	decoded, err := DecodeHTTPResponse(encoded)
	if err != nil {
		t.Fatalf("DecodeHTTPResponse: %v", err)
	}
	if decoded.StatusCode != 404 {
		t.Errorf("StatusCode: want 404, got %d", decoded.StatusCode)
	}
	if len(decoded.Body) != 0 {
		t.Errorf("Body: want empty, got %d bytes", len(decoded.Body))
	}
}

func TestEncodeHTTPRequest_NoHeaders(t *testing.T) {
	req := HTTPRequest{
		Method:  "DELETE",
		Path:    "/resource/1",
		Headers: nil,
		Body:    []byte{},
	}
	encoded, _ := EncodeHTTPRequest(req)
	decoded, err := DecodeHTTPRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeHTTPRequest: %v", err)
	}
	if decoded.Method != "DELETE" {
		t.Errorf("Method: want DELETE, got %q", decoded.Method)
	}
}

func TestReadHeaders_RejectsHugeCount(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint32(0xFFFFFFFF))
	_, err := readHeaders(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected error for huge count, got nil")
	}
	if !strings.Contains(err.Error(), "too many headers") {
		t.Errorf("expected 'too many headers' error, got: %v", err)
	}
}

func TestReadHeaders_RejectsHugeValueCount(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint32(1))
	binaryWriteTo(&buf, uint16(3))
	buf.WriteString("abc")
	binaryWriteTo(&buf, uint32(MaxValuesPerHeader+1))
	_, err := readHeaders(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected error for huge valueCount")
	}
	if !strings.Contains(err.Error(), "too many values") {
		t.Errorf("expected 'too many values' error, got: %v", err)
	}
}

func TestReadString_RejectsOversizedLength(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint16(1000))
	buf.WriteString("short")
	_, err := readString(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected error for truncated string")
	}
}

func TestEncodeDecodeAuth_Roundtrip(t *testing.T) {
	encoded, err := EncodeAuth("my-token", "v1.2.3")
	if err != nil {
		t.Fatalf("EncodeAuth: %v", err)
	}
	token, ver, err := DecodeAuth(encoded)
	if err != nil {
		t.Fatalf("DecodeAuth: %v", err)
	}
	if token != "my-token" {
		t.Errorf("token: want %q, got %q", "my-token", token)
	}
	if ver != "v1.2.3" {
		t.Errorf("version: want %q, got %q", "v1.2.3", ver)
	}
}

func TestEncodeDecodeAuth_EmptyToken(t *testing.T) {
	encoded, err := EncodeAuth("", "v1.0.0")
	if err != nil {
		t.Fatalf("EncodeAuth: %v", err)
	}
	token, ver, err := DecodeAuth(encoded)
	if err != nil {
		t.Fatalf("DecodeAuth: %v", err)
	}
	if token != "" {
		t.Errorf("token: want empty, got %q", token)
	}
	if ver != "v1.0.0" {
		t.Errorf("version: want %q, got %q", "v1.0.0", ver)
	}
}

func TestEncodeDecodeAuth_EmptyVersion(t *testing.T) {
	encoded, err := EncodeAuth("tok", "")
	if err != nil {
		t.Fatalf("EncodeAuth: %v", err)
	}
	token, ver, err := DecodeAuth(encoded)
	if err != nil {
		t.Fatalf("DecodeAuth: %v", err)
	}
	if token != "tok" {
		t.Errorf("token: want %q, got %q", "tok", token)
	}
	if ver != "" {
		t.Errorf("version: want empty, got %q", ver)
	}
}

func TestEncodeDecodeAuth_TokenTooLong(t *testing.T) {
	long := strings.Repeat("a", maxStringLen+1)
	_, err := EncodeAuth(long, "v1.0.0")
	if err == nil {
		t.Error("expected error for oversized token")
	}
}

func TestEncodeDecodeAuth_VersionTooLong(t *testing.T) {
	long := strings.Repeat("a", MaxVersionLen+1)
	_, err := EncodeAuth("tok", long)
	if err == nil {
		t.Error("expected error for oversized version")
	}
}

func TestDecodeAuth_TruncatedToken(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint16(100))
	_, _, err := DecodeAuth(buf.Bytes())
	if err == nil {
		t.Error("expected error for truncated token")
	}
}

func TestDecodeAuth_TruncatedVersion(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint16(3))
	buf.WriteString("tok")
	binaryWriteTo(&buf, uint16(100))
	_, _, err := DecodeAuth(buf.Bytes())
	if err == nil {
		t.Error("expected error for truncated version")
	}
}

func TestEncodeDecodeAuthOK_Roundtrip(t *testing.T) {
	encoded, err := EncodeAuthOK("v2.0.0")
	if err != nil {
		t.Fatalf("EncodeAuthOK: %v", err)
	}
	ver, err := DecodeAuthOK(encoded)
	if err != nil {
		t.Fatalf("DecodeAuthOK: %v", err)
	}
	if ver != "v2.0.0" {
		t.Errorf("version: want %q, got %q", "v2.0.0", ver)
	}
}

func TestDecodeAuthOK_Empty(t *testing.T) {
	ver, err := DecodeAuthOK(nil)
	if err != nil {
		t.Fatalf("DecodeAuthOK(nil): %v", err)
	}
	if ver != "" {
		t.Errorf("version: want empty, got %q", ver)
	}
}

func TestEncodeAuthOK_TooLong(t *testing.T) {
	long := strings.Repeat("a", MaxVersionLen+1)
	_, err := EncodeAuthOK(long)
	if err == nil {
		t.Error("expected error for oversized version")
	}
}

func TestDecodeAuthOK_Truncated(t *testing.T) {
	var buf bytes.Buffer
	binaryWriteTo(&buf, uint16(100))
	_, err := DecodeAuthOK(buf.Bytes())
	if err == nil {
		t.Error("expected error for truncated version")
	}
}

func binaryWrite(buf []byte, v uint32) {
	buf[0] = byte(v >> 24)
	buf[1] = byte(v >> 16)
	buf[2] = byte(v >> 8)
	buf[3] = byte(v)
}

func binaryWriteTo(buf *bytes.Buffer, v interface{}) {
	switch val := v.(type) {
	case uint16:
		buf.WriteByte(byte(val >> 8))
		buf.WriteByte(byte(val))
	case uint32:
		buf.WriteByte(byte(val >> 24))
		buf.WriteByte(byte(val >> 16))
		buf.WriteByte(byte(val >> 8))
		buf.WriteByte(byte(val))
	}
}

func TestEncodeDecodeRegister_Roundtrip(t *testing.T) {
	payload, err := EncodeRegister(8080, "myapp")
	if err != nil {
		t.Fatalf("EncodeRegister: %v", err)
	}
	port, sub, err := DecodeRegister(payload)
	if err != nil {
		t.Fatalf("DecodeRegister: %v", err)
	}
	if port != 8080 || sub != "myapp" {
		t.Errorf("roundtrip: want (8080, myapp), got (%d, %q)", port, sub)
	}
}

func TestEncodeDecodeRegister_NoSubdomain(t *testing.T) {
	payload, err := EncodeRegister(3000, "")
	if err != nil {
		t.Fatalf("EncodeRegister: %v", err)
	}
	port, sub, err := DecodeRegister(payload)
	if err != nil {
		t.Fatalf("DecodeRegister: %v", err)
	}
	if port != 3000 || sub != "" {
		t.Errorf("roundtrip: want (3000, \"\"), got (%d, %q)", port, sub)
	}
}

func TestEncodeRegister_SubdomainTooLong(t *testing.T) {
	if _, err := EncodeRegister(80, strings.Repeat("a", MaxSubdomainLen+1)); err == nil {
		t.Error("expected error for oversized subdomain")
	}
}

func TestDecodeRegister_Truncated(t *testing.T) {
	if _, _, err := DecodeRegister([]byte{0x1F, 0x90, 10, 'a', 'b'}); err == nil {
		t.Error("expected error for truncated subdomain")
	}
	if _, _, err := DecodeRegister([]byte{0x01}); err == nil {
		t.Error("expected error for short payload")
	}
}

func TestEncodeDecodeRegisterOK_Roundtrip(t *testing.T) {
	payload, err := EncodeRegisterOK("app.tunnel.domain.com", 42)
	if err != nil {
		t.Fatalf("EncodeRegisterOK: %v", err)
	}
	url, id, err := DecodeRegisterOK(payload)
	if err != nil {
		t.Fatalf("DecodeRegisterOK: %v", err)
	}
	if url != "app.tunnel.domain.com" || id != 42 {
		t.Errorf("roundtrip: want (app.tunnel.domain.com, 42), got (%q, %d)", url, id)
	}
}

func TestDecodeRegisterOK_Truncated(t *testing.T) {
	if _, _, err := DecodeRegisterOK(nil); err == nil {
		t.Error("expected error for empty payload")
	}
	if _, _, err := DecodeRegisterOK([]byte{5, 'a', 'b'}); err == nil {
		t.Error("expected error for truncated payload")
	}
}

func TestEncodeHTTPRequest_StringTooLong(t *testing.T) {
	_, err := EncodeHTTPRequest(HTTPRequest{
		Method: "GET",
		Path:   "/" + strings.Repeat("x", 70000),
	})
	if err == nil {
		t.Error("expected error for oversized path string")
	}
}
