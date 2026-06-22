package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"vlgr/internal/protocol"
)

const (
	maxBodySize    = 32 << 20
	streamBufSize  = 32 * 1024
)

type ReverseProxy struct {
	registry   *Registry
	baseDomain string
	debug      bool
}

func NewReverseProxy(registry *Registry, baseDomain string, debug bool) *ReverseProxy {
	return &ReverseProxy{registry: registry, baseDomain: baseDomain, debug: debug}
}

func isWebSocketUpgrade(r *http.Request) bool {
	for _, v := range r.Header["Upgrade"] {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "websocket") {
				return true
			}
		}
	}
	return false
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusInternalServerError)
		return
	}
	r.Body.Close()

	log.Printf("[proxy] received %s %s (body: %d bytes)", r.Method, r.URL.RequestURI(), len(body))

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

	req := protocol.HTTPRequest{
		Method:  r.Method,
		Path:    r.URL.RequestURI(),
		Headers: headers,
		Body:    body,
	}

	var streamData chan []byte
	if isWebSocketUpgrade(r) {
		streamData = make(chan []byte, 256)
		if p.debug {
			log.Printf("[debug] WebSocket upgrade detected for %s", r.URL.RequestURI())
		}
	}

	requestID, resp, cleanup, err := tunnel.Handler.ForwardHTTP(req, streamData)
	if err != nil {
		log.Printf("[proxy] forward error for %s: %v", subdomain, err)
		http.Error(w, "tunnel error", http.StatusBadGateway)
		return
	}
	defer cleanup()

	if streamData == nil {
		for key, values := range resp.Headers {
			for _, value := range values {
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
		for _, value := range values {
			bufrw.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}
	bufrw.WriteString("\r\n")
	bufrw.Flush()

	if p.debug {
		log.Printf("[debug] WebSocket relay started for %s (request #%d)", r.URL.RequestURI(), resp.StatusCode)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, streamBufSize)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				tunnel.Handler.SendStreamClose(requestID)
				return
			}
			if err := tunnel.Handler.SendStreamData(requestID, buf[:n]); err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for data := range streamData {
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()

	wg.Wait()
	conn.Close()

	if p.debug {
		log.Printf("[debug] WebSocket relay ended for %s", r.URL.RequestURI())
	}
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
	return prefix
}
