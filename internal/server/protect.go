package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
)

// Protector gates public traffic before it reaches a tunnel. Both guards are
// optional: a zero Protector allows everything. Configured once at startup and
// applied to every request by the reverse proxy.
type Protector struct {
	// Basic auth: empty user disables it.
	user     string
	passHash [32]byte

	// IP allowlist: nil/empty allows every source.
	allow []*net.IPNet
}

// NewProtector builds a Protector from "user:pass" (empty = no basic auth) and
// a comma-separated CIDR/IP allowlist (empty = allow all).
func NewProtector(basicAuth, allowIPs string) (*Protector, error) {
	p := &Protector{}
	if basicAuth != "" {
		user, pass, ok := strings.Cut(basicAuth, ":")
		if !ok || user == "" {
			return nil, fmt.Errorf("basic auth must be \"user:pass\"")
		}
		p.user = user
		p.passHash = sha256.Sum256([]byte(pass))
	}
	for _, part := range strings.Split(allowIPs, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Accept both bare IPs and CIDRs; a bare IP becomes a /32 or /128.
		if !strings.Contains(part, "/") {
			if ip := net.ParseIP(part); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				part = fmt.Sprintf("%s/%d", part, bits)
			}
		}
		_, cidr, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed IP %q: %w", part, err)
		}
		p.allow = append(p.allow, cidr)
	}
	return p, nil
}

// enabled reports whether any guard is configured (skips work when not).
func (p *Protector) enabled() bool {
	return p != nil && (p.user != "" || len(p.allow) > 0)
}

// Allow enforces the guards; on rejection it writes the response and returns
// false. subdomain is only used for logging.
func (p *Protector) Allow(w http.ResponseWriter, r *http.Request, subdomain string) bool {
	if !p.enabled() {
		return true
	}
	if len(p.allow) > 0 && !p.ipAllowed(r) {
		log.Printf("[protect] %s: source %s not in allowlist", subdomain, clientIP(r))
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if p.user != "" && !p.basicAuthOK(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="vlgr"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (p *Protector) basicAuthOK(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	gotPass := sha256.Sum256([]byte(pass))
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(p.user)) == 1
	passOK := subtle.ConstantTimeCompare(gotPass[:], p.passHash[:]) == 1
	return userOK && passOK
}

func (p *Protector) ipAllowed(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return false
	}
	for _, cidr := range p.allow {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP returns the originating client IP: the leftmost X-Forwarded-For
// entry when present (set by Caddy), else the RemoteAddr host.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
