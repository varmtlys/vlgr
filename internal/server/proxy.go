package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"vlgr/internal/protocol"
)

const (
	relayIdleTimeout = 5 * time.Minute
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
		log.Printf("[debug] request headers:")
		for k, values := range r.Header {
			for _, v := range values {
				log.Printf("[debug]   %s: %s", k, v)
			}
		}
		if len(body) > 0 {
			bodyPreview := string(body)
			if len(bodyPreview) > 200 {
				bodyPreview = bodyPreview[:200] + "..."
			}
			log.Printf("[debug] request body: %s", bodyPreview)
		}
	}

	headers := make(map[string][]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values
		}
	}
	headers["Host"] = []string{r.Host}

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

	var streamData chan []byte
	if protocol.IsWebSocketUpgrade(r) {
		streamData = make(chan []byte, streamRelayBuf)
		if p.debug {
			log.Printf("[debug] WebSocket upgrade detected for %s", requestPath)
		}
	}

	requestID, resp, cleanup, err := tunnel.Handler.ForwardHTTP(tunnel.ID, req, streamData)
	if err != nil {
		log.Printf("[proxy] forward error for %s: %v", subdomain, err)
		http.Error(w, "tunnel error", http.StatusBadGateway)
		return
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	if streamData == nil {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		for key, values := range resp.Headers {
			if !validHeaderName(key) {
				log.Printf("[proxy] dropping response header with invalid name: %q", key)
				continue
			}
			for _, value := range values {
				if !validHeaderValue(value) {
					log.Printf("[proxy] dropping response header %q with invalid value (CRLF?)", key)
					continue
				}
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(int(resp.StatusCode))
		w.Write(resp.Body)
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
	for key, values := range resp.Headers {
		if !validHeaderName(key) {
			log.Printf("[proxy] dropping response header with invalid name: %q", key)
			continue
		}
		for _, value := range values {
			if !validHeaderValue(value) {
				log.Printf("[proxy] dropping response header %q with invalid value (CRLF?)", key)
				continue
			}
			bufrw.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}
	bufrw.WriteString("\r\n")
	if err := bufrw.Flush(); err != nil {
		log.Printf("[proxy] flush error for %s: %v", subdomain, err)
		conn.Close()
		return
	}

	if p.debug {
		log.Printf("[debug] WebSocket relay started for %s (request #%d)", requestPath, resp.StatusCode)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, protocol.StreamBufSize)
		conn.SetReadDeadline(time.Now().Add(relayIdleTimeout))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				tunnel.Handler.SendStreamClose(requestID)
				return
			}
			conn.SetReadDeadline(time.Now().Add(relayIdleTimeout))
			if err := tunnel.Handler.SendStreamData(requestID, buf[:n]); err != nil {
				tunnel.Handler.SendStreamClose(requestID)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for data := range streamData {
			conn.SetWriteDeadline(time.Now().Add(relayIdleTimeout))
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
		conn.Close()
	}()

	wg.Wait()
	conn.Close()

	if p.debug {
		log.Printf("[debug] WebSocket relay ended for %s", requestPath)
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
