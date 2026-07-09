package server

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"vlgr/internal/protocol"
)

type ReverseProxy struct {
	registry   *Registry
	baseDomain string
	debug      bool
}

func NewReverseProxy(registry *Registry, baseDomain string, debug bool) *ReverseProxy {
	return &ReverseProxy{registry: registry, baseDomain: baseDomain, debug: debug}
}

func (p *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	subdomain := extractSubdomain(r.Host, p.baseDomain)
	if subdomain == "" {
		http.Error(w, "no subdomain found in Host header", http.StatusBadRequest)
		return
	}

	tunnel := p.registry.Get(subdomain)
	if tunnel == nil {
		http.Error(w, fmt.Sprintf("tunnel %q not found", subdomain), http.StatusNotFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "request body too large or read error", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body.Close()

	log.Printf("[proxy] received %s %s (body: %d bytes)", r.Method, r.URL.EscapedPath(), len(body))

	if p.debug {
		protocol.DebugLogHeaders("[debug] request", r.Header)
		if len(body) > 0 {
			log.Printf("[debug] request body: %s", protocol.TextPreview(body, 200))
		}
	}

	headers := make(map[string][]string, len(r.Header)+4)
	for key, values := range r.Header {
		headers[key] = values
	}
	headers["Host"] = []string{r.Host}
	headers["X-Forwarded-Host"] = []string{r.Host}
	if len(headers["X-Forwarded-Proto"]) == 0 {
		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}
		headers["X-Forwarded-Proto"] = []string{proto}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		// Copy before append: the slice is shared with r.Header.
		xff := append([]string(nil), headers["X-Forwarded-For"]...)
		headers["X-Forwarded-For"] = append(xff, host)
	}

	requestPath := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		requestPath += "?" + r.URL.RawQuery
	}

	req := protocol.HTTPRequest{
		Method:  r.Method,
		Path:    requestPath,
		Headers: headers,
		Body:    body,
	}

	if !protocol.IsWebSocketUpgrade(r) {
		resp, err := tunnel.Handler.ForwardHTTP(tunnel.ID, req)
		if err != nil {
			log.Printf("[proxy] forward error for %s: %v", subdomain, err)
			http.Error(w, "tunnel error", http.StatusBadGateway)
			return
		}
		writeNormalResponse(w, resp)
		return
	}

	p.serveWebSocket(w, tunnel, req, subdomain)
}

// serveWebSocket forwards an Upgrade request through the tunnel and, on 101,
// hijacks the connection and relays raw bytes in both directions.
func (p *ReverseProxy) serveWebSocket(w http.ResponseWriter, tunnel *Tunnel, req protocol.HTTPRequest, subdomain string) {
	if p.debug {
		log.Printf("[debug] WebSocket upgrade detected for %s", req.Path)
	}
	streamData := make(chan []byte, protocol.StreamRelayBuf)

	requestID, resp, cleanup, err := tunnel.Handler.ForwardWebSocket(tunnel.ID, req, streamData)
	if err != nil {
		log.Printf("[proxy] forward error for %s: %v", subdomain, err)
		http.Error(w, "tunnel error", http.StatusBadGateway)
		return
	}
	defer cleanup()

	// The local app rejected the upgrade — relay its response as-is.
	if resp.StatusCode != 101 {
		writeNormalResponse(w, resp)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("[proxy] hijacking not supported for %s", subdomain)
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		log.Printf("[proxy] hijack error for %s: %v", subdomain, err)
		http.Error(w, "hijack error", http.StatusInternalServerError)
		return
	}

	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	copyValidHeaders(resp.Headers, func(key, value string) {
		fmt.Fprintf(bufrw, "%s: %s\r\n", key, value)
	})
	bufrw.WriteString("\r\n")
	if err := bufrw.Flush(); err != nil {
		log.Printf("[proxy] flush error for %s: %v", subdomain, err)
		conn.Close()
		return
	}

	if p.debug {
		log.Printf("[debug] WebSocket relay started for %s (request #%d)", req.Path, requestID)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, protocol.StreamBufSize)
		conn.SetReadDeadline(time.Now().Add(protocol.RelayIdleTimeout))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				tunnel.Handler.SendStreamClose(requestID)
				return
			}
			conn.SetReadDeadline(time.Now().Add(protocol.RelayIdleTimeout))
			if err := tunnel.Handler.SendStreamData(requestID, buf[:n]); err != nil {
				tunnel.Handler.SendStreamClose(requestID)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for data := range streamData {
			conn.SetWriteDeadline(time.Now().Add(protocol.RelayIdleTimeout))
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
		conn.Close()
	}()

	wg.Wait()
	conn.Close()

	if p.debug {
		log.Printf("[debug] WebSocket relay ended for %s", req.Path)
	}
}

func writeNormalResponse(w http.ResponseWriter, resp protocol.HTTPResponse) {
	copyValidHeaders(resp.Headers, w.Header().Add)
	// Security defaults only when the app didn't set its own.
	if w.Header().Get("X-Content-Type-Options") == "" {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	if w.Header().Get("X-Frame-Options") == "" {
		w.Header().Set("X-Frame-Options", "DENY")
	}
	w.WriteHeader(int(resp.StatusCode))
	w.Write(resp.Body)
}

func copyValidHeaders(headers map[string][]string, add func(key, value string)) {
	for key, values := range headers {
		if !validHeaderName(key) {
			log.Printf("[proxy] dropping response header with invalid name: %q", key)
			continue
		}
		for _, value := range values {
			if !validHeaderValue(value) {
				log.Printf("[proxy] dropping response header %q with invalid value (CRLF?)", key)
				continue
			}
			add(key, value)
		}
	}
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' || c == '\n' {
			return false
		}
		if c == 0 {
			return false
		}
	}
	return true
}

func extractSubdomain(host, baseDomain string) string {
	if idx := strings.IndexByte(host, ':'); idx != -1 {
		host = host[:idx]
	}
	if idx := strings.IndexByte(baseDomain, ':'); idx != -1 {
		baseDomain = baseDomain[:idx]
	}

	suffix := "." + baseDomain
	if host == baseDomain || !strings.HasSuffix(host, suffix) {
		return ""
	}

	prefix := strings.TrimSuffix(host, suffix)
	if prefix == "" || strings.Contains(prefix, ".") {
		return ""
	}
	if !validSubdomain(prefix) {
		return ""
	}
	return prefix
}
