package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboard_RecordRingBuffer(t *testing.T) {
	const limit = 50
	d := NewDashboard("127.0.0.1:0", limit)
	for i := 0; i < limit+50; i++ {
		d.record(&ReqEntry{Method: "GET", Path: "/", Time: time.Now()})
	}
	d.mu.Lock()
	n := len(d.entries)
	firstID := d.entries[0].ID
	d.mu.Unlock()
	if n != limit {
		t.Errorf("ring buffer size: want %d, got %d", limit, n)
	}
	// Oldest entries are dropped, so the first retained ID is > 50.
	if firstID <= 50 {
		t.Errorf("expected oldest entries dropped, first ID = %d", firstID)
	}
}

func TestClampInspectLimit(t *testing.T) {
	cases := map[int]int{
		0:      defaultInspectLimit,
		-5:     defaultInspectLimit,
		1:      1,
		1000:   1000,
		100000: 100000,
		250000: maxInspectLimit,
	}
	for in, want := range cases {
		if got := clampInspectLimit(in); got != want {
			t.Errorf("clampInspectLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestDashboard_RecordHTTPFields(t *testing.T) {
	d := NewDashboard("127.0.0.1:0", 1000)
	d.recordHTTP("POST", "/submit", 3000,
		map[string][]string{"Host": {"api.example.com"}}, []byte("payload"), false,
		201, map[string][]string{"Content-Type": {"application/json"}}, []byte("{}"),
		time.Now().Add(-5*time.Millisecond))

	e := d.find(1)
	if e == nil {
		t.Fatal("entry not recorded")
	}
	if e.Method != "POST" || e.Path != "/submit" || e.Status != 201 {
		t.Errorf("unexpected entry: %+v", e)
	}
	if e.Host != "api.example.com" {
		t.Errorf("host: want api.example.com, got %q", e.Host)
	}
	if string(e.ReqBody) != "payload" {
		t.Errorf("req body: got %q", e.ReqBody)
	}
	if e.Duration <= 0 {
		t.Errorf("duration should be positive, got %v", e.Duration)
	}
}

func TestDashboard_ListNewestFirst(t *testing.T) {
	d := NewDashboard("127.0.0.1:0", 1000)
	d.record(&ReqEntry{Method: "GET", Path: "/a", Time: time.Now()})
	d.record(&ReqEntry{Method: "GET", Path: "/b", Time: time.Now()})

	rec := httptest.NewRecorder()
	d.handleList(rec, httptest.NewRequest(http.MethodGet, "/api/requests", nil))
	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	var out []jsonEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 || out[0].Path != "/b" {
		t.Errorf("expected newest-first, got %+v", out)
	}
	// Summary view must not leak bodies/headers.
	if out[0].ReqHeaders != nil || out[0].ReqBody != "" {
		t.Errorf("summary should omit headers/body")
	}
}

func TestDashboard_BodyCap(t *testing.T) {
	big := make([]byte, dashBodyCap+100)
	if got := len(capBody(big)); got != dashBodyCap {
		t.Errorf("capBody: want %d, got %d", dashBodyCap, got)
	}
}

func sampleDashboard() *Dashboard {
	d := NewDashboard("127.0.0.1:0", 1000)
	d.recordHTTP("POST", "/submit", 3000,
		map[string][]string{"Host": {"api.example.com"}, "Content-Type": {"text/plain"}},
		[]byte("req-body"), false,
		200, map[string][]string{"Content-Type": {"application/json"}}, []byte(`{"ok":true}`),
		time.Now().Add(-7*time.Millisecond))
	return d
}

func TestDashboard_ExportHAR(t *testing.T) {
	d := sampleDashboard()
	rec := httptest.NewRecorder()
	d.handleExportHAR(rec, httptest.NewRequest(http.MethodGet, "/api/export.har", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".har") {
		t.Errorf("missing .har attachment header: %q", cd)
	}

	var har struct {
		Log struct {
			Version string `json:"version"`
			Entries []struct {
				Request  struct{ Method, URL string }
				Response struct {
					Status  int
					Content struct{ Text string }
				}
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &har); err != nil {
		t.Fatalf("HAR is not valid JSON: %v", err)
	}
	if har.Log.Version != "1.2" {
		t.Errorf("HAR version: %q", har.Log.Version)
	}
	if len(har.Log.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(har.Log.Entries))
	}
	e := har.Log.Entries[0]
	if e.Request.Method != "POST" || e.Response.Status != 200 {
		t.Errorf("unexpected HAR entry: %+v", e)
	}
	if e.Request.URL != "http://api.example.com/submit" {
		t.Errorf("HAR url: %q", e.Request.URL)
	}
	if e.Response.Content.Text != `{"ok":true}` {
		t.Errorf("HAR response body: %q", e.Response.Content.Text)
	}
}

func TestDashboard_ExportText(t *testing.T) {
	d := sampleDashboard()
	rec := httptest.NewRecorder()
	d.handleExportText(rec, httptest.NewRequest(http.MethodGet, "/api/export.txt", nil))

	body := rec.Body.String()
	for _, want := range []string{"POST http://api.example.com/submit", "> Content-Type: text/plain", "req-body", "< HTTP 200 OK", `{"ok":true}`} {
		if !strings.Contains(body, want) {
			t.Errorf("text dump missing %q\n---\n%s", want, body)
		}
	}
}
