package server

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"time"

	"vlgr/internal/version"
)

// AdminServer exposes a small read-only REST API plus a status dashboard for
// operating the relay. Bind it to a loopback/admin address; an optional bearer
// token guards it when the address is not loopback.
type AdminServer struct {
	addr     string
	token    string
	registry *Registry
	started  time.Time
	srv      *http.Server
}

func NewAdminServer(addr, token string, registry *Registry) *AdminServer {
	return &AdminServer{addr: addr, token: token, registry: registry, started: time.Now()}
}

// Start binds and serves in the background; a bad address is reported at once.
func (a *AdminServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", a.guard(a.handleStatus))
	mux.HandleFunc("/api/tunnels", a.guard(a.handleTunnels))
	mux.HandleFunc("/", a.guard(a.handleIndex))

	ln, err := net.Listen("tcp", a.addr)
	if err != nil {
		return err
	}
	a.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go a.srv.Serve(ln)
	log.Printf("[admin] REST API + dashboard on http://%s", a.addr)
	return nil
}

func (a *AdminServer) Stop() {
	if a.srv != nil {
		a.srv.Close()
	}
}

// guard authorizes an admin request. A loopback caller reaching the server by
// a loopback Host is trusted without a token (the local dashboard) — the Host
// check blocks DNS-rebinding. Any other caller must present the bearer token,
// which is refused outright when no token is configured, so binding the admin
// API to a non-loopback address without a token does not expose it.
func (a *AdminServer) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if (peerIsLoopback(r.RemoteAddr) && hostIsLoopback(r.Host)) || a.tokenOK(r) {
			h(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (a *AdminServer) tokenOK(r *http.Request) bool {
	if a.token == "" {
		return false
	}
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	return len(got) > len(prefix) && subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(a.token)) == 1
}

func peerIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hostIsLoopback(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *AdminServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeAdminJSON(w, map[string]any{
		"version":   version.Version,
		"uptimeSec": int64(time.Since(a.started).Seconds()),
		"tunnels":   len(a.registry.Snapshot()),
	})
}

func (a *AdminServer) handleTunnels(w http.ResponseWriter, r *http.Request) {
	writeAdminJSON(w, a.registry.Snapshot())
}

func writeAdminJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (a *AdminServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

const adminHTML = `<!doctype html><meta charset=utf-8><title>vlgr admin</title>
<style>body{font:14px system-ui,sans-serif;margin:2rem;max-width:40rem}
h1{font-size:1.2rem}table{border-collapse:collapse;width:100%}
td,th{border-bottom:1px solid #ccc;padding:.4rem .6rem;text-align:left}
code{background:#f2f2f2;padding:.1rem .3rem;border-radius:3px}</style>
<h1>vlgr relay</h1><p id=s>loading…</p>
<table><thead><tr><th>Subdomain</th><th>Tunnel ID</th></tr></thead><tbody id=t></tbody></table>
<script>
async function tick(){
 try{
  const st=await (await fetch('api/status')).json();
  document.getElementById('s').textContent=
   'v'+st.version+' · '+st.tunnels+' tunnel(s) · up '+st.uptimeSec+'s';
  const rows=await (await fetch('api/tunnels')).json();
  document.getElementById('t').innerHTML=(rows||[]).map(r=>
   '<tr><td><code>'+r.subdomain+'</code></td><td>'+r.id+'</td></tr>').join('');
 }catch(e){document.getElementById('s').textContent='error: '+e}
}
tick();setInterval(tick,3000);
</script>`
