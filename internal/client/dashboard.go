package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vlgr/internal/version"
)

const (
	dashBodyCap         = 128 << 10 // per-body capture cap for display/replay
	defaultInspectLimit = 1000
	maxInspectLimit     = 100000
)

// clampInspectLimit keeps the ring-buffer size within [1, maxInspectLimit];
// a non-positive limit falls back to the default.
func clampInspectLimit(limit int) int {
	if limit <= 0 {
		return defaultInspectLimit
	}
	if limit > maxInspectLimit {
		return maxInspectLimit
	}
	return limit
}

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
	addr       string
	maxEntries int

	mu      sync.Mutex
	entries []*ReqEntry
	nextID  int

	subsMu sync.Mutex
	subs   map[chan *ReqEntry]struct{}

	srv *http.Server
}

// NewDashboard builds an inspector bound to addr keeping at most limit rows
// (clamped to [1, 100000]; <=0 uses the default 1000). Older rows are
// dropped as new ones arrive on top.
func NewDashboard(addr string, limit int) *Dashboard {
	return &Dashboard{
		addr:       addr,
		maxEntries: clampInspectLimit(limit),
		subs:       make(map[chan *ReqEntry]struct{}),
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
	mux.HandleFunc("/api/export.har", d.handleExportHAR)
	mux.HandleFunc("/api/export.txt", d.handleExportText)

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
	if len(d.entries) > d.maxEntries {
		d.entries = d.entries[len(d.entries)-d.maxEntries:]
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
	copyHeaders(req, e.ReqHeaders)

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
	// Keep the browser-side row cap in sync with the server ring buffer.
	page := strings.ReplaceAll(dashboardHTML, "__MAX_ENTRIES__", strconv.Itoa(d.maxEntries))
	w.Write([]byte(page))
}

// snapshot returns the entries oldest-first for export.
func (d *Dashboard) snapshot() []*ReqEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*ReqEntry, len(d.entries))
	copy(out, d.entries)
	return out
}

func attachment(w http.ResponseWriter, ext, contentType string) {
	name := "vlgr-inspector-" + time.Now().Format("20060102-150405") + "." + ext
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
}

// ─── HAR export (HTTP Archive 1.2) ───────────────────────────────────────────

type harNV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func harHeaders(h map[string][]string) []harNV {
	out := []harNV{}
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, harNV{Name: k, Value: v})
		}
	}
	return out
}

func (d *Dashboard) handleExportHAR(w http.ResponseWriter, r *http.Request) {
	type postData struct {
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
	}
	type request struct {
		Method      string    `json:"method"`
		URL         string    `json:"url"`
		HTTPVersion string    `json:"httpVersion"`
		Cookies     []harNV   `json:"cookies"`
		Headers     []harNV   `json:"headers"`
		QueryString []harNV   `json:"queryString"`
		PostData    *postData `json:"postData,omitempty"`
		HeadersSize int       `json:"headersSize"`
		BodySize    int       `json:"bodySize"`
	}
	type content struct {
		Size     int    `json:"size"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text,omitempty"`
	}
	type response struct {
		Status      int     `json:"status"`
		StatusText  string  `json:"statusText"`
		HTTPVersion string  `json:"httpVersion"`
		Cookies     []harNV `json:"cookies"`
		Headers     []harNV `json:"headers"`
		Content     content `json:"content"`
		RedirectURL string  `json:"redirectURL"`
		HeadersSize int     `json:"headersSize"`
		BodySize    int     `json:"bodySize"`
	}
	type timings struct {
		Send    int   `json:"send"`
		Wait    int64 `json:"wait"`
		Receive int   `json:"receive"`
	}
	type entry struct {
		StartedDateTime string   `json:"startedDateTime"`
		Time            int64    `json:"time"`
		Request         request  `json:"request"`
		Response        response `json:"response"`
		Cache           struct{} `json:"cache"`
		Timings         timings  `json:"timings"`
	}

	var entries []entry
	for _, e := range d.snapshot() {
		req := request{
			Method: e.Method, URL: entryURL(e), HTTPVersion: "HTTP/1.1",
			Cookies: []harNV{}, Headers: harHeaders(e.ReqHeaders), QueryString: []harNV{},
			HeadersSize: -1, BodySize: len(e.ReqBody),
		}
		if len(e.ReqBody) > 0 {
			req.PostData = &postData{MimeType: firstHeader(e.ReqHeaders, "Content-Type"), Text: string(e.ReqBody)}
		}
		entries = append(entries, entry{
			StartedDateTime: e.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
			Time:            e.Duration.Milliseconds(),
			Request:         req,
			Response: response{
				Status: e.Status, StatusText: http.StatusText(e.Status), HTTPVersion: "HTTP/1.1",
				Cookies: []harNV{}, Headers: harHeaders(e.RespHeaders),
				Content:     content{Size: len(e.RespBody), MimeType: firstHeader(e.RespHeaders, "Content-Type"), Text: string(e.RespBody)},
				RedirectURL: firstHeader(e.RespHeaders, "Location"), HeadersSize: -1, BodySize: len(e.RespBody),
			},
			Timings: timings{Send: 0, Wait: e.Duration.Milliseconds(), Receive: 0},
		})
	}

	har := map[string]any{
		"log": map[string]any{
			"version": "1.2",
			"creator": map[string]string{"name": "vlgr-inspector", "version": version.Version},
			"entries": entries,
		},
	}
	attachment(w, "har", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(har)
}

// ─── Plain-text dump ─────────────────────────────────────────────────────────

func (d *Dashboard) handleExportText(w http.ResponseWriter, r *http.Request) {
	attachment(w, "txt", "text/plain; charset=utf-8")
	var b strings.Builder
	entries := d.snapshot()
	fmt.Fprintf(&b, "vlgr inspector dump — %d request(s) — %s\n", len(entries), time.Now().Format(time.RFC3339))
	for _, e := range entries {
		b.WriteString(strings.Repeat("=", 80) + "\n")
		tag := ""
		if e.Replayed {
			tag = "  [replay]"
		}
		fmt.Fprintf(&b, "#%d  %s  %s %s  ->  %d %s  (%d ms)%s\n",
			e.ID, e.Time.Format("2006-01-02 15:04:05.000"), e.Method, entryURL(e),
			e.Status, http.StatusText(e.Status), e.Duration.Milliseconds(), tag)
		b.WriteString(strings.Repeat("-", 80) + "\n")

		fmt.Fprintf(&b, "> %s %s\n", e.Method, e.Path)
		writeDumpHeaders(&b, "> ", e.ReqHeaders)
		if len(e.ReqBody) > 0 {
			fmt.Fprintf(&b, ">\n%s\n", string(e.ReqBody))
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "< HTTP %d %s\n", e.Status, http.StatusText(e.Status))
		writeDumpHeaders(&b, "< ", e.RespHeaders)
		if len(e.RespBody) > 0 {
			fmt.Fprintf(&b, "<\n%s\n", string(e.RespBody))
		}
		b.WriteString("\n")
	}
	w.Write([]byte(b.String()))
}

func writeDumpHeaders(b *strings.Builder, prefix string, h map[string][]string) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(b, "%s%s: %s\n", prefix, k, v)
		}
	}
}

func entryURL(e *ReqEntry) string {
	host := e.Host
	if host == "" {
		host = fmt.Sprintf("localhost:%d", e.LocalPort)
	}
	return "http://" + host + e.Path
}
