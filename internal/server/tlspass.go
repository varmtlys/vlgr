package server

import (
	"log"
	"net"
	"time"

	"vlgr/internal/protocol"
)

// TLSPassthrough is a single public TLS listener that multiplexes many tunnels
// by SNI. It never terminates TLS: it peeks the ClientHello, routes by the
// server_name hostname, and relays raw bytes to the matching client's local
// TLS port. Tunnels register via MsgRegisterTLS into the shared tlsRegistry.
type TLSPassthrough struct {
	addr       string
	baseDomain string
	registry   *Registry
	ln         net.Listener
}

func NewTLSPassthrough(addr, baseDomain string, registry *Registry) *TLSPassthrough {
	return &TLSPassthrough{addr: addr, baseDomain: baseDomain, registry: registry}
}

// Start binds the listener (reporting a bad address immediately) and accepts
// in the background.
func (t *TLSPassthrough) Start() error {
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return err
	}
	t.ln = ln
	go t.accept()
	log.Printf("[tls] SNI passthrough listening on %s", t.addr)
	return nil
}

func (t *TLSPassthrough) Stop() {
	if t.ln != nil {
		t.ln.Close()
	}
}

func (t *TLSPassthrough) accept() {
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			return
		}
		go t.route(conn)
	}
}

func (t *TLSPassthrough) route(conn net.Conn) {
	prefix, sni, ok := peekClientHello(conn)
	if !ok {
		log.Printf("[tls] no SNI in ClientHello from %s, dropping", conn.RemoteAddr())
		conn.Close()
		return
	}

	subdomain := extractSubdomain(sni, t.baseDomain)
	if subdomain == "" {
		log.Printf("[tls] SNI %q outside base domain, dropping", sni)
		conn.Close()
		return
	}

	tunnel := t.registry.Get(subdomain)
	if tunnel == nil {
		log.Printf("[tls] no TLS tunnel for %q, dropping", sni)
		conn.Close()
		return
	}

	// relayPublicConn owns conn from here (including Close) and forwards the
	// peeked ClientHello bytes before the live stream.
	tunnel.Handler.relayPublicConn(tunnel.ID, conn, prefix)
}

// peekClientHello reads the initial TLS ClientHello record without consuming
// it: the returned prefix holds every byte read so it can be forwarded intact.
func peekClientHello(conn net.Conn) (prefix []byte, sni string, ok bool) {
	conn.SetReadDeadline(time.Now().Add(protocol.RequestTimeout))
	defer conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for len(buf) < 8192 {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if name, found := extractSNI(buf); found {
				return buf, name, true
			}
		}
		if err != nil {
			break
		}
	}
	return buf, "", false
}
