package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"vlgr/internal/protocol"
)

const maxBodySize = 32 << 20

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

	resp, err := tunnel.Handler.ForwardHTTP(req)
	if err != nil {
		log.Printf("[proxy] forward error for %s: %v", subdomain, err)
		http.Error(w, "tunnel error", http.StatusBadGateway)
		return
	}

	if p.debug {
		log.Printf("[debug] response status: %d", resp.StatusCode)
		log.Printf("[debug] response headers:")
		for k, values := range resp.Headers {
			for _, v := range values {
				log.Printf("[debug]   %s: %s", k, v)
			}
		}
		if len(resp.Body) > 0 {
			bodyPreview := string(resp.Body)
			if len(bodyPreview) > 200 {
				bodyPreview = bodyPreview[:200] + "..."
			}
			log.Printf("[debug] response body: %s", bodyPreview)
		}
	}

	for key, values := range resp.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(int(resp.StatusCode))
	w.Write(resp.Body)
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
