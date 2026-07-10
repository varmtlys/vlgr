package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDashboard_RecordRingBuffer(t *testing.T) {
	d := NewDashboard("127.0.0.1:0")
	for i := 0; i < dashMaxEntries+50; i++ {
		d.record(&ReqEntry{Method: "GET", Path: "/", Time: time.Now()})
	}
	d.mu.Lock()
	n := len(d.entries)
	firstID := d.entries[0].ID
	d.mu.Unlock()
	if n != dashMaxEntries {
		t.Errorf("ring buffer size: want %d, got %d", dashMaxEntries, n)
	}
	// Oldest entries are dropped, so the first retained ID is > 50.
	if firstID <= 50 {
		t.Errorf("expected oldest entries dropped, first ID = %d", firstID)
	}
}

func TestDashboard_RecordHTTPFields(t *testing.T) {
	d := NewDashboard("127.0.0.1:0")
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
	d := NewDashboard("127.0.0.1:0")
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
