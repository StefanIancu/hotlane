package replay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url string, hdr map[string]string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func captureServer(b *Buffer, methods map[string]bool, exclude []string, h http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(Capture(h, b, methods, exclude))
}

func TestCaptureRecordsEligibleExchanges(t *testing.T) {
	b := NewBuffer(8)
	srv := captureServer(b, map[string]bool{"GET": true}, []string{"/metrics"}, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	})
	defer srv.Close()

	get(t, srv.URL+"/a", map[string]string{"Authorization": "Bearer tok"})
	get(t, srv.URL+"/metrics", nil) // excluded
	req, _ := http.NewRequest("POST", srv.URL+"/a", strings.NewReader("x"))
	resp, _ := http.DefaultClient.Do(req) // method not allowed for capture
	resp.Body.Close()

	if b.Len() != 1 {
		t.Fatalf("buffered = %d, want 1", b.Len())
	}
	e := b.Snapshot(1)[0]
	if e.Method != "GET" || e.Path != "/a" || string(e.RespBody) != "hello /a" || e.Status != 200 {
		t.Errorf("entry = %+v", e)
	}
	if e.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("auth header not kept for replay")
	}
}

func TestBufferRingEviction(t *testing.T) {
	b := NewBuffer(3)
	for i := 0; i < 5; i++ {
		b.add(Entry{Path: fmt.Sprintf("/%d", i)})
	}
	if b.Len() != 3 {
		t.Fatalf("len = %d", b.Len())
	}
	snap := b.Snapshot(3)
	if snap[0].Path != "/2" || snap[2].Path != "/4" {
		t.Errorf("snapshot = %v, want oldest-first /2../4", []string{snap[0].Path, snap[1].Path, snap[2].Path})
	}
	if got := b.Snapshot(2); got[0].Path != "/3" || got[1].Path != "/4" {
		t.Errorf("snapshot(2) = %v, want newest two oldest-first", []string{got[0].Path, got[1].Path})
	}
}

// record fills a buffer by driving real requests through Capture.
func record(t *testing.T, b *Buffer, h http.HandlerFunc, paths ...string) {
	t.Helper()
	srv := captureServer(b, map[string]bool{"GET": true}, nil, h)
	defer srv.Close()
	for _, p := range paths {
		get(t, srv.URL+p, nil)
	}
}

func TestRunMatchesIdenticalFork(t *testing.T) {
	b := NewBuffer(8)
	h := func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "body of %s", r.URL.Path) }
	record(t, b, h, "/a", "/b")
	fork := httptest.NewServer(http.HandlerFunc(h))
	defer fork.Close()

	res := Run(b.Snapshot(2), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Replayed != 2 || res.Matched != 2 || res.Mismatched != 0 {
		t.Errorf("res = %+v", res)
	}
}

func TestRunFlagsChangedBody(t *testing.T) {
	b := NewBuffer(8)
	record(t, b, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "old") }, "/page")
	fork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "NEW") }))
	defer fork.Close()

	res := Run(b.Snapshot(1), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Mismatched != 1 || len(res.Mismatches) != 1 {
		t.Fatalf("res = %+v", res)
	}
	m := res.Mismatches[0]
	if m.Path != "/page" || m.Want != "old" || m.Got != "NEW" {
		t.Errorf("mismatch = %+v", m)
	}
}

func TestRunFlagsChangedStatus(t *testing.T) {
	b := NewBuffer(8)
	record(t, b, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }, "/x")
	fork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fork.Close()

	res := Run(b.Snapshot(1), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Mismatched != 1 || res.Mismatches[0].GotStatus != 500 {
		t.Errorf("res = %+v", res)
	}
}

func TestRunSelfDynamicComparesStatusOnly(t *testing.T) {
	// live served a counter: two buffered answers for /n differ, so the
	// fork's different counter value cannot be evidence against it.
	b := NewBuffer(8)
	n := 0
	record(t, b, func(w http.ResponseWriter, r *http.Request) { n++; fmt.Fprintf(w, "count %d", n) }, "/n", "/n")
	fork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "count 999") }))
	defer fork.Close()

	res := Run(b.Snapshot(2), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Mismatched != 0 || res.Dynamic != 2 {
		t.Errorf("res = %+v", res)
	}
}

func TestRunToleratesTimestamps(t *testing.T) {
	// stable-per-instance timestamp: normalized before comparison.
	b := NewBuffer(8)
	record(t, b, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"up":true,"since":"2026-07-19T14:00:01Z"}`)
	}, "/health")
	fork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"up":true,"since":"2026-07-19T18:30:59Z"}`)
	}))
	defer fork.Close()

	res := Run(b.Snapshot(1), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Mismatched != 0 || res.Matched != 1 {
		t.Errorf("res = %+v", res)
	}
}

func TestRunBudget(t *testing.T) {
	b := NewBuffer(64)
	record(t, b, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") },
		"/1", "/2", "/3", "/4", "/5", "/6", "/7", "/8")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer slow.Close()

	res := Run(b.Snapshot(8), b.Len(), strings.TrimPrefix(slow.URL, "http://"), 300*time.Millisecond)
	if !res.BudgetHit {
		t.Errorf("budget not reported: %+v", res)
	}
	if res.Mismatched != 0 {
		t.Errorf("budget expiry counted as mismatch: %+v", res)
	}
}
