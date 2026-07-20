package replay

import (
	"fmt"
	"io"
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

func TestResetEmptiesAndRefills(t *testing.T) {
	b := NewBuffer(3)
	for i := 0; i < 5; i++ {
		b.add(Entry{Path: fmt.Sprintf("/%d", i)})
	}
	b.Reset()
	if b.Len() != 0 || len(b.Snapshot(3)) != 0 {
		t.Fatalf("reset left %d entries", b.Len())
	}
	b.add(Entry{Path: "/new"})
	if b.Len() != 1 || b.Snapshot(1)[0].Path != "/new" {
		t.Errorf("refill after reset broken: len=%d", b.Len())
	}
}

func TestCaptureAndReplayBodiedRequest(t *testing.T) {
	// The POST opt-in: the request body must be tee'd at capture and
	// faithfully re-sent at replay.
	b := NewBuffer(8)
	echo := func(w http.ResponseWriter, r *http.Request) {
		in, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "got:%s", in)
	}
	srv := captureServer(b, map[string]bool{"POST": true}, nil, echo)
	resp, err := http.Post(srv.URL+"/submit", "text/plain", strings.NewReader("payload-1"))
	if err != nil {
		t.Fatal(err)
	}
	live, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	srv.Close()
	if string(live) != "got:payload-1" {
		t.Fatalf("live response corrupted by capture tee: %q", live)
	}
	e := b.Snapshot(1)[0]
	if string(e.Body) != "payload-1" || string(e.RespBody) != "got:payload-1" {
		t.Fatalf("entry = body %q resp %q", e.Body, e.RespBody)
	}

	// Identical fork: body-dependent response must match.
	fork := httptest.NewServer(http.HandlerFunc(echo))
	defer fork.Close()
	res := Run(b.Snapshot(1), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Matched != 1 || res.Mismatched != 0 {
		t.Errorf("res = %+v", res)
	}

	// A fork that mishandles the body must mismatch.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "got:")
	}))
	defer broken.Close()
	res = Run(b.Snapshot(1), b.Len(), strings.TrimPrefix(broken.URL, "http://"), time.Second)
	if res.Mismatched != 1 {
		t.Errorf("body-dropping fork not flagged: %+v", res)
	}
}

func TestCaptureSkipsOversizedResponse(t *testing.T) {
	b := NewBuffer(8)
	big := strings.Repeat("x", bodyCap+100)
	srv := captureServer(b, map[string]bool{"GET": true}, nil, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, big)
	})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/big")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) != len(big) {
		t.Fatalf("client got %d bytes, want %d - capture must never truncate real responses", len(body), len(big))
	}
	if b.Len() != 0 {
		t.Errorf("oversized exchange was buffered")
	}
}

func TestCaptureSkipsOversizedRequestBody(t *testing.T) {
	b := NewBuffer(8)
	var received int
	srv := captureServer(b, map[string]bool{"POST": true}, nil, func(w http.ResponseWriter, r *http.Request) {
		in, _ := io.ReadAll(r.Body)
		received = len(in)
		fmt.Fprint(w, "ok")
	})
	defer srv.Close()
	big := strings.Repeat("y", bodyCap+100)
	resp, err := http.Post(srv.URL+"/upload", "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if received != len(big) {
		t.Fatalf("app received %d bytes, want %d - capture must never eat request bytes", received, len(big))
	}
	if b.Len() != 0 {
		t.Errorf("oversized request was buffered")
	}
}

// BenchmarkCapture measures what the recorder adds to every live
// request when replay is enabled - the hot-path tax of the feature.
func BenchmarkCapture(b *testing.B) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from demo-app")
	})
	for _, on := range []bool{false, true} {
		name := "bare"
		h := http.Handler(handler)
		if on {
			name = "captured"
			h = Capture(handler, NewBuffer(512), map[string]bool{"GET": true}, nil)
		}
		b.Run(name, func(b *testing.B) {
			req := httptest.NewRequest("GET", "/", nil)
			for i := 0; i < b.N; i++ {
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
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

// Reset must clear recorded user data, not merely rewind the ring:
// entries hold Authorization/Cookie headers and response bodies.
func TestResetClearsRecordedData(t *testing.T) {
	b := NewBuffer(4)
	for i := 0; i < 4; i++ {
		b.add(Entry{Path: "/p", Header: http.Header{"Authorization": []string{"Bearer supersecret"}},
			Body: []byte("req-secret"), RespBody: []byte("resp-secret")})
	}
	b.Reset()
	for i, e := range b.entries {
		if e.Header != nil || e.Body != nil || e.RespBody != nil || e.Path != "" {
			t.Errorf("slot %d still holds user data after Reset: %+v", i, e)
		}
	}
}

// Anything leaving the authenticated API must not carry query strings -
// recorded ones hold tokens and email addresses.
func TestMismatchEndpointStripsQuery(t *testing.T) {
	m := Mismatch{Method: "GET", Path: "/reset?token=abc123&email=alice@corp.com"}
	if got := m.Endpoint(); got != "GET /reset" {
		t.Errorf("Endpoint() = %q, want %q", got, "GET /reset")
	}
}

// Enabling replay must not change what the app can serve. The recorder
// wraps the ResponseWriter, and ReverseProxy type-asserts Flusher (to
// stream SSE) and Hijacker (to upgrade websockets) - so a recorder that
// only wraps Write turns every websocket into a 502 and freezes SSE.
func TestCapturePreservesFlusherAndHijacker(t *testing.T) {
	var sawFlusher, sawHijacker bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		_, sawHijacker = w.(http.Hijacker)
		fmt.Fprint(w, "ok")
	})
	srv := httptest.NewServer(Capture(h, NewBuffer(4), map[string]bool{"GET": true}, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !sawFlusher {
		t.Error("wrapped writer lost http.Flusher - SSE streams would buffer until close")
	}
	if !sawHijacker {
		t.Error("wrapped writer lost http.Hijacker - websocket upgrades would 502")
	}
}

// Compressed bodies defeat normalization: the volatile patterns never
// match, so an identical response with a timestamp inside reads as a
// mismatch and the diff prints binary garbage.
func TestCaptureSkipsEncodedResponses(t *testing.T) {
	b := NewBuffer(4)
	srv := httptest.NewServer(Capture(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		w.Write([]byte("\x1f\x8b\x08garbage"))
	}), b, map[string]bool{"GET": true}, nil))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/")
	if resp != nil {
		resp.Body.Close()
	}
	if b.Len() != 0 {
		t.Errorf("compressed response was buffered; it would false-positive the gate")
	}
}

// Query-distinct probes are different questions. Keying dynamism on the
// path alone let one paginated endpoint exempt itself from body
// comparison entirely.
func TestDynamicKeyingIsPerFullPath(t *testing.T) {
	b := NewBuffer(8)
	record(t, b, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "page %s", r.URL.Query().Get("page"))
	}, "/items?page=1", "/items?page=2")
	fork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "REGRESSED")
	}))
	defer fork.Close()
	res := Run(b.Snapshot(2), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Mismatched != 2 {
		t.Errorf("total body regression hidden by dynamic suppression: %+v", res)
	}
}

// A fork that turns JSON into HTML, or redirects somewhere new, is a
// regression - and a 3xx body is usually empty, so the body diff alone
// would call it identical.
func TestRunFlagsResponseHeaderChanges(t *testing.T) {
	b := NewBuffer(8)
	record(t, b, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}, "/api")
	fork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer fork.Close()
	res := Run(b.Snapshot(1), b.Len(), strings.TrimPrefix(fork.URL, "http://"), time.Second)
	if res.Mismatched != 1 || !strings.Contains(res.Mismatches[0].Got, "text/html") {
		t.Errorf("content-type regression not flagged: %+v", res)
	}

	b2 := NewBuffer(8)
	// Record without following the redirect, so the buffer holds the 3xx
	// itself rather than whatever it points at.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	srv2 := captureServer(b2, map[string]bool{"GET": true}, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/dashboard")
		w.WriteHeader(302)
	})
	resp2, err := noFollow.Get(srv2.URL + "/go")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	srv2.Close()
	fork2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/login")
		w.WriteHeader(302)
	}))
	defer fork2.Close()
	res2 := Run(b2.Snapshot(1), b2.Len(), strings.TrimPrefix(fork2.URL, "http://"), time.Second)
	if res2.Mismatched != 1 {
		t.Errorf("redirect target change not flagged (empty bodies match): %+v", res2)
	}
}
