package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dashMaxEntries = 200
	dashBodyCap    = 128 << 10 // per-body capture cap for display/replay
)

// ReqEntry is one recorded HTTP exchange that passed through the tunnel.
type ReqEntry struct {
	ID          int
	Time        time.Time
	Method      string
	Path        string
	Host        string
	LocalPort   uint16
	Status      int
	Duration    time.Duration
	ReqHeaders  map[string][]string
	RespHeaders map[string][]string
	ReqBody     []byte
	RespBody    []byte
	ReqStreamed bool
	Replayed    bool
}

// Dashboard records recent requests and serves a local web inspector.
type Dashboard struct {
	addr string

	mu      sync.Mutex
	entries []*ReqEntry
	nextID  int

	subsMu sync.Mutex
	subs   map[chan *ReqEntry]struct{}

	srv *http.Server
}

func NewDashboard(addr string) *Dashboard {
	return &Dashboard{
		addr: addr,
		subs: make(map[chan *ReqEntry]struct{}),
	}
}

// Start launches the inspector HTTP server. It binds before returning so a
// bad address is reported immediately.
func (d *Dashboard) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/api/requests", d.handleList)
	mux.HandleFunc("/api/request/", d.handleDetail)
	mux.HandleFunc("/api/replay/", d.handleReplay)
	mux.HandleFunc("/api/stream", d.handleStream)

	ln, err := net.Listen("tcp", d.addr)
	if err != nil {
		return err
	}
	d.srv = &http.Server{Handler: mux}
	go d.srv.Serve(ln)
	log.Printf("[client] inspector dashboard on http://%s", d.addr)
	return nil
}

func (d *Dashboard) Stop() {
	if d.srv != nil {
		d.srv.Close()
	}
}

// record captures one exchange and notifies live subscribers.
func (d *Dashboard) record(e *ReqEntry) {
	d.mu.Lock()
	d.nextID++
	e.ID = d.nextID
	d.entries = append(d.entries, e)
	if len(d.entries) > dashMaxEntries {
		d.entries = d.entries[len(d.entries)-dashMaxEntries:]
	}
	d.mu.Unlock()

	d.subsMu.Lock()
	for ch := range d.subs {
		select {
		case ch <- e:
		default: // slow client — drop rather than block the relay
		}
	}
	d.subsMu.Unlock()
}

// recordHTTP is the hook the tunnel calls after forwarding a request.
func (d *Dashboard) recordHTTP(method, path string, localPort uint16, reqHeaders map[string][]string, reqBody []byte, reqStreamed bool, status int, respHeaders map[string][]string, respBody []byte, started time.Time) {
	d.record(&ReqEntry{
		Time:        started,
		Method:      method,
		Path:        path,
		Host:        firstHeader(reqHeaders, "Host"),
		LocalPort:   localPort,
		Status:      status,
		Duration:    time.Since(started),
		ReqHeaders:  reqHeaders,
		RespHeaders: respHeaders,
		ReqBody:     capBody(reqBody),
		RespBody:    capBody(respBody),
		ReqStreamed: reqStreamed,
	})
}

func capBody(b []byte) []byte {
	if len(b) > dashBodyCap {
		return b[:dashBodyCap]
	}
	return b
}

func firstHeader(h map[string][]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// ─── JSON shapes ─────────────────────────────────────────────────────────────

type jsonEntry struct {
	ID          int                 `json:"id"`
	Time        string              `json:"time"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Host        string              `json:"host"`
	LocalPort   uint16              `json:"localPort"`
	Status      int                 `json:"status"`
	DurationMs  int64               `json:"durationMs"`
	ReqHeaders  map[string][]string `json:"reqHeaders,omitempty"`
	RespHeaders map[string][]string `json:"respHeaders,omitempty"`
	ReqBody     string              `json:"reqBody,omitempty"`
	RespBody    string              `json:"respBody,omitempty"`
	ReqStreamed bool                `json:"reqStreamed"`
	Replayed    bool                `json:"replayed"`
}

func (e *ReqEntry) toJSON(full bool) jsonEntry {
	j := jsonEntry{
		ID:          e.ID,
		Time:        e.Time.Format("15:04:05.000"),
		Method:      e.Method,
		Path:        e.Path,
		Host:        e.Host,
		LocalPort:   e.LocalPort,
		Status:      e.Status,
		DurationMs:  e.Duration.Milliseconds(),
		ReqStreamed: e.ReqStreamed,
		Replayed:    e.Replayed,
	}
	if full {
		j.ReqHeaders = e.ReqHeaders
		j.RespHeaders = e.RespHeaders
		j.ReqBody = string(e.ReqBody)
		j.RespBody = string(e.RespBody)
	}
	return j
}

func (d *Dashboard) handleList(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	out := make([]jsonEntry, 0, len(d.entries))
	for i := len(d.entries) - 1; i >= 0; i-- { // newest first
		out = append(out, d.entries[i].toJSON(false))
	}
	d.mu.Unlock()
	writeJSON(w, out)
}

func (d *Dashboard) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/request/"))
	e := d.find(id)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, e.toJSON(true))
}

func (d *Dashboard) find(id int) *ReqEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

func (d *Dashboard) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan *ReqEntry, 16)
	d.subsMu.Lock()
	d.subs[ch] = struct{}{}
	d.subsMu.Unlock()
	defer func() {
		d.subsMu.Lock()
		delete(d.subs, ch)
		d.subsMu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			data, _ := json.Marshal(e.toJSON(false))
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

var replayClient = &http.Client{Timeout: 30 * time.Second}

func (d *Dashboard) handleReplay(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/replay/"))
	e := d.find(id)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	if e.ReqStreamed {
		http.Error(w, "cannot replay a streamed request body", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("http://localhost:%d%s", e.LocalPort, e.Path)
	started := time.Now()
	req, err := http.NewRequest(e.Method, url, bytes.NewReader(e.ReqBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k, values := range e.ReqHeaders {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		if strings.EqualFold(k, "Host") {
			req.Host = values[0]
			continue
		}
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}

	resp, err := replayClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, dashBodyCap))

	d.record(&ReqEntry{
		Time:        started,
		Method:      e.Method,
		Path:        e.Path,
		Host:        e.Host,
		LocalPort:   e.LocalPort,
		Status:      resp.StatusCode,
		Duration:    time.Since(started),
		ReqHeaders:  e.ReqHeaders,
		RespHeaders: resp.Header,
		ReqBody:     e.ReqBody,
		RespBody:    body,
		Replayed:    true,
	})
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}
