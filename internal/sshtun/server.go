// Package sshtun exposes local services through the relay using a stock SSH
// client — no vlgr agent required. A user runs, for example:
//
//	ssh -N -R 0:localhost:3000 tunnel.domain.com -p 2222
//
// and the relay allocates a public TCP port, forwarding every connection back
// over the SSH channel to the user's local service. This is the standard
// OpenSSH remote-forwarding flow (tcpip-forward), so any ssh client works.
package sshtun

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"

	"golang.org/x/crypto/ssh"
)

// PortAllocator hands out and reclaims public TCP ports. Satisfied by the
// server package's allocator so SSH and vlgr tunnels can share a range.
type PortAllocator interface {
	Allocate(preferred uint16) (uint16, error)
	Release(port uint16)
}

// Server accepts SSH connections and serves their remote forwards.
type Server struct {
	addr  string
	token string
	alloc PortAllocator
	cfg   *ssh.ServerConfig
	ln    net.Listener
}

// New builds an SSH tunnel server. token (when non-empty) is required as the
// SSH password. hostKeyPath, when set, loads/persists the host key so clients
// don't see key-changed warnings across restarts; empty uses an ephemeral key.
func New(addr, token, hostKeyPath string, alloc PortAllocator) (*Server, error) {
	signer, err := hostKey(hostKeyPath)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ServerConfig{}
	if token == "" {
		cfg.NoClientAuth = true
	} else {
		cfg.PasswordCallback = func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if subtle.ConstantTimeCompare(pass, []byte(token)) != 1 {
				return nil, fmt.Errorf("invalid password")
			}
			return nil, nil
		}
	}
	cfg.AddHostKey(signer)

	return &Server{addr: addr, token: token, alloc: alloc, cfg: cfg}, nil
}

// hostKey loads an ed25519 host key from path, generating (and saving, when a
// path is given) one if absent. An empty path yields an ephemeral key.
func hostKey(path string) (ssh.Signer, error) {
	if path != "" {
		if pem, err := os.ReadFile(path); err == nil {
			return ssh.ParsePrivateKey(pem)
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		return nil, err
	}
	if path != "" {
		if block, err := ssh.MarshalPrivateKey(priv, ""); err == nil {
			os.WriteFile(path, pem.EncodeToMemory(block), 0600)
		}
	}
	return signer, nil
}

// Start binds and serves in the background; a bad address is reported at once.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go s.accept()
	log.Printf("[ssh] agentless SSH tunnels listening on %s", s.addr)
	return nil
}

func (s *Server) Stop() {
	if s.ln != nil {
		s.ln.Close()
	}
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(nConn net.Conn) {
	sc, chans, reqs, err := ssh.NewServerConn(nConn, s.cfg)
	if err != nil {
		nConn.Close()
		return
	}
	defer sc.Close()
	log.Printf("[ssh] client connected: %s", sc.RemoteAddr())

	// Reject session/exec channels: this server only does remote forwards.
	go rejectChannels(chans)
	s.handleGlobalRequests(sc, reqs)
}

func rejectChannels(chans <-chan ssh.NewChannel) {
	for ch := range chans {
		ch.Reject(ssh.Prohibited, "only remote port forwarding (-R) is supported")
	}
}

// bindReq is the payload of a tcpip-forward global request (RFC 4254 §7.1).
type bindReq struct {
	Addr string
	Port uint32
}

// handleGlobalRequests services tcpip-forward / cancel-tcpip-forward for one
// SSH connection, tracking the public listeners it opens.
func (s *Server) handleGlobalRequests(sc ssh.Conn, reqs <-chan *ssh.Request) {
	forwards := map[uint16]net.Listener{}
	defer func() {
		for port, ln := range forwards {
			ln.Close()
			s.alloc.Release(port)
		}
	}()

	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			s.startForward(sc, req, forwards)
		case "cancel-tcpip-forward":
			s.cancelForward(req, forwards)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (s *Server) startForward(sc ssh.Conn, req *ssh.Request, forwards map[uint16]net.Listener) {
	var b bindReq
	if err := ssh.Unmarshal(req.Payload, &b); err != nil {
		req.Reply(false, nil)
		return
	}

	port, err := s.alloc.Allocate(uint16(b.Port))
	if err != nil {
		log.Printf("[ssh] allocate port for %s: %v", sc.RemoteAddr(), err)
		req.Reply(false, nil)
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		s.alloc.Release(port)
		req.Reply(false, nil)
		return
	}
	forwards[port] = ln

	// The reply carries the bound port so `ssh -R 0:...` learns its port.
	if req.WantReply {
		req.Reply(true, ssh.Marshal(struct{ Port uint32 }{uint32(port)}))
	}
	log.Printf("[ssh] remote forward ready: :%d -> %s", port, sc.RemoteAddr())

	go s.acceptForward(sc, ln, b.Addr, port)
}

func (s *Server) cancelForward(req *ssh.Request, forwards map[uint16]net.Listener) {
	var b bindReq
	if err := ssh.Unmarshal(req.Payload, &b); err == nil {
		if ln, ok := forwards[uint16(b.Port)]; ok {
			ln.Close()
			s.alloc.Release(uint16(b.Port))
			delete(forwards, uint16(b.Port))
		}
	}
	if req.WantReply {
		req.Reply(true, nil)
	}
}

// forwardChanReq is the payload opening a forwarded-tcpip channel back to the
// SSH client (RFC 4254 §7.2).
type forwardChanReq struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

func (s *Server) acceptForward(sc ssh.Conn, ln net.Listener, bindAddr string, bindPort uint16) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.forwardConn(sc, conn, bindAddr, bindPort)
	}
}

func (s *Server) forwardConn(sc ssh.Conn, conn net.Conn, bindAddr string, bindPort uint16) {
	defer conn.Close()

	originAddr, originPortStr, _ := net.SplitHostPort(conn.RemoteAddr().String())
	originPort, _ := strconv.Atoi(originPortStr)

	payload := ssh.Marshal(forwardChanReq{
		DestAddr:   bindAddr,
		DestPort:   uint32(bindPort),
		OriginAddr: originAddr,
		OriginPort: uint32(originPort),
	})
	ch, reqs, err := sc.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		return
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)

	// Splice the public connection and the SSH channel both ways.
	done := make(chan struct{}, 2)
	go func() { io.Copy(ch, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, ch); done <- struct{}{} }()
	<-done
}
