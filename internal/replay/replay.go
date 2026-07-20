// Package replay is shadow testing built into the deploy: it records a
// rolling slice of live traffic - including the response live actually
// served - and replays that slice against a fork before promotion,
// diffing the fork's answers against the recorded ones. Live gets zero
// extra load; the fork gets interrogated by the app's own recent users.
//
// The buffer is memory-only and dies with the process: it holds real
// user data (headers included - stripping auth would make every replay
// a 401) and must never touch disk.
package replay

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/StefanIancu/hotlane/internal/respdiff"
)

// bodyCap bounds stored request and response bodies; larger exchanges
// are simply not captured (a 100MB download is not a useful probe).
const bodyCap = 64 << 10

// Entry is one recorded exchange: the request as it arrived and the
// response live served for it.
type Entry struct {
	Method string
	Path   string // includes query string
	Header http.Header
	Body   []byte

	Status   int
	RespBody []byte
	// RespType and RespLocation are the response headers that carry
	// behavioral contract. A fork that turns JSON into HTML, or redirects
	// somewhere new, is a regression - and a 3xx body is usually empty,
	// so the body diff alone is vacuously equal. Other headers (Date,
	// Set-Cookie) are volatile by nature and deliberately not compared.
	RespType     string
	RespLocation string
}

// Buffer is a fixed-capacity ring of recent live exchanges.
type Buffer struct {
	mu      sync.Mutex
	cap     int
	entries []Entry
	next    int
	full    bool
}

func NewBuffer(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 512
	}
	return &Buffer{cap: capacity, entries: make([]Entry, capacity)}
}

func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[b.next] = e
	b.next = (b.next + 1) % b.cap
	if b.next == 0 {
		b.full = true
	}
}

// Reset empties the buffer. Call on every traffic flip (promote,
// rollback): recorded exchanges describe the version that was serving
// when they were captured, and replaying them against a successor's
// fork - or a drift check's cold boot - would compare the future
// against a stale past and false-positive.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Clear the entries, don't just rewind: they hold real user headers
	// (Authorization, Cookie) and bodies, and a quiet app would otherwise
	// keep a full ring of them resident - visible in a core dump or swap
	// long after the traffic they describe.
	for i := range b.entries {
		b.entries[i] = Entry{}
	}
	b.next, b.full = 0, false
}

// Len is how many exchanges are currently buffered.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		return b.cap
	}
	return b.next
}

// Snapshot returns up to n of the newest entries, oldest-first.
func (b *Buffer) Snapshot(n int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	size := b.next
	if b.full {
		size = b.cap
	}
	if n > size {
		n = size
	}
	out := make([]Entry, 0, n)
	for i := size - n; i < size; i++ {
		idx := i
		if b.full {
			idx = (b.next + b.cap - size + i) % b.cap
		}
		out = append(out, b.entries[idx])
	}
	return out
}

// recorder captures a response as it streams to the real client.
//
// It MUST forward Flush and Hijack. ReverseProxy type-asserts
// http.Flusher to stream text/event-stream, and http.Hijacker to switch
// protocols for websockets - so a recorder that only wraps Write turns
// every websocket into a 502 and makes SSE streams deliver nothing until
// the connection closes. Enabling replay must never change what the app
// can serve.
type recorder struct {
	http.ResponseWriter
	status   int
	body     bytes.Buffer
	overflow bool
	hijacked bool
	encoded  bool // Content-Encoding set: bytes are compressed, useless to compare
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("replay: underlying ResponseWriter is not a Hijacker")
	}
	r.hijacked = true // connection leaves HTTP; nothing to record
	return h.Hijack()
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	// A compressed body normalizes to nothing useful: the volatile
	// patterns never match, so an identical response with one timestamp
	// inside reads as a mismatch, and the diff prints binary garbage.
	r.encoded = r.ResponseWriter.Header().Get("Content-Encoding") != ""
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.body.Len()+len(p) <= bodyCap {
		r.body.Write(p)
	} else {
		r.overflow = true
	}
	return r.ResponseWriter.Write(p)
}

// Capture wraps a live-traffic handler, recording eligible exchanges
// into the buffer. Fork pokes (X-Hotlane-Fork) never reach this handler
// - the caller routes them before live traffic.
func Capture(next http.Handler, b *Buffer, methods map[string]bool, exclude []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !methods[r.Method] || excluded(r.URL.Path, exclude) {
			next.ServeHTTP(w, r)
			return
		}
		// Tee the request body up to the cap; oversized requests are
		// forwarded untouched but not captured.
		var reqBody bytes.Buffer
		bodyOverflow := false
		if r.Body != nil {
			r.Body = teeBody(r.Body, &reqBody, &bodyOverflow)
		}
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if bodyOverflow || rec.overflow || rec.hijacked || rec.encoded {
			return
		}
		b.add(Entry{
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			Header:       r.Header.Clone(),
			Body:         append([]byte(nil), reqBody.Bytes()...),
			Status:       rec.status,
			RespBody:     append([]byte(nil), rec.body.Bytes()...),
			RespType:     w.Header().Get("Content-Type"),
			RespLocation: w.Header().Get("Location"),
		})
	})
}

func excluded(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// teeBody copies up to bodyCap of a request body as it is consumed.
func teeBody(rc io.ReadCloser, dst *bytes.Buffer, overflow *bool) io.ReadCloser {
	return &teeReadCloser{rc: rc, dst: dst, overflow: overflow}
}

type teeReadCloser struct {
	rc       io.ReadCloser
	dst      *bytes.Buffer
	overflow *bool
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		if t.dst.Len()+n <= bodyCap {
			t.dst.Write(p[:n])
		} else {
			*t.overflow = true
		}
	}
	return n, err
}

func (t *teeReadCloser) Close() error { return t.rc.Close() }

// Mismatch is one replayed exchange the fork answered differently.
type Mismatch struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	WantStatus int    `json:"want_status"`
	GotStatus  int    `json:"got_status"`
	Want       string `json:"want,omitempty"` // normalized, truncated
	Got        string `json:"got,omitempty"`
}

// Endpoint names the mismatching request without its query string. Use
// this - never Path, Want or Got - for anything that leaves the
// authenticated API: logs, webhooks, drift details. Recorded traffic is
// real user traffic, and query strings carry tokens and email addresses
// as routinely as bodies carry personal data.
func (m Mismatch) Endpoint() string {
	return m.Method + " " + pathOnly(m.Path)
}

// Incomplete reports that the run did not judge every entry - the
// budget expired first. Not the same as "no mismatches": a fork that
// hangs produces zero mismatches, so gate mode must treat this as a
// failure rather than a clean sheet.
func (r *Result) Incomplete() bool { return r.BudgetHit }

// Result is one replay run's verdict, attached to push/test responses.
type Result struct {
	Replayed   int        `json:"replayed"`
	Matched    int        `json:"matched"`
	Dynamic    int        `json:"dynamic"` // status-only paths (self-dynamic in the buffer)
	Mismatched int        `json:"mismatched"`
	Buffered   int        `json:"buffered"`
	BudgetHit  bool       `json:"budget_hit,omitempty"`
	Ms         int64      `json:"ms"`
	Mismatches []Mismatch `json:"mismatches,omitempty"` // capped at 5
}

// Run replays entries against the fork backend and diffs each answer
// against the recorded live one. Paths whose normalized live bodies
// differ WITHIN the buffer are self-dynamic: those compare status only
// - content that varies between two live requests cannot be evidence
// against the fork.
func Run(entries []Entry, buffered int, backend string, budget time.Duration) Result {
	start := time.Now()
	res := Result{Buffered: buffered}
	if len(entries) == 0 {
		return res
	}

	dynamic := dynamicPaths(entries)

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	// Never follow redirects: the recorded entry IS the 3xx, and
	// following it would compare whatever the redirect lands on instead
	// - so a fork that changes where /go sends users would be judged on
	// the destination page rather than the redirect itself.
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	verdicts := make([]verdict, len(entries))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, e := range entries {
		if ctx.Err() != nil {
			res.BudgetHit = true
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			verdicts[i] = replayOne(ctx, client, backend, e, dynamic[e.Method+" "+e.Path])
		}(i, e)
	}
	wg.Wait()

	for _, v := range verdicts {
		if v.mis == nil && !v.ok && !v.dyn {
			continue // never ran (budget)
		}
		res.Replayed++
		switch {
		case v.mis != nil:
			res.Mismatched++
			if len(res.Mismatches) < 5 {
				res.Mismatches = append(res.Mismatches, *v.mis)
			}
		case v.dyn:
			res.Dynamic++
			res.Matched++
		default:
			res.Matched++
		}
	}
	if res.Replayed < len(entries) {
		res.BudgetHit = true
	}
	res.Ms = time.Since(start).Milliseconds()
	return res
}

// verdict is one replayed entry's outcome; the zero value means the
// entry never ran (budget expiry) and is excluded from the tally.
type verdict struct {
	ok, dyn bool
	mis     *Mismatch
}

func replayOne(ctx context.Context, client *http.Client, backend string, e Entry, dyn bool) (v verdict) {
	req, err := http.NewRequestWithContext(ctx, e.Method, "http://"+backend+e.Path, bytes.NewReader(e.Body))
	if err != nil {
		return v
	}
	req.Header = e.Header.Clone()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return v // budget expired mid-flight: not the fork's fault
		}
		v.mis = &Mismatch{Method: e.Method, Path: e.Path, WantStatus: e.Status, Got: "request failed: " + err.Error()}
		return v
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap+1))

	if resp.StatusCode != e.Status {
		v.mis = &Mismatch{Method: e.Method, Path: e.Path, WantStatus: e.Status, GotStatus: resp.StatusCode}
		return v
	}
	if got := resp.Header.Get("Content-Type"); e.RespType != "" && got != e.RespType {
		v.mis = &Mismatch{
			Method: e.Method, Path: e.Path,
			WantStatus: e.Status, GotStatus: resp.StatusCode,
			Want: "Content-Type: " + e.RespType, Got: "Content-Type: " + got,
		}
		return v
	}
	if got := resp.Header.Get("Location"); e.RespLocation != "" && got != e.RespLocation {
		v.mis = &Mismatch{
			Method: e.Method, Path: e.Path,
			WantStatus: e.Status, GotStatus: resp.StatusCode,
			Want: "Location: " + respdiff.Normalize(e.RespLocation), Got: "Location: " + respdiff.Normalize(got),
		}
		return v
	}
	if dyn {
		v.ok, v.dyn = true, true
		return v
	}
	want, got := respdiff.Normalize(string(e.RespBody)), respdiff.Normalize(string(body))
	if want != got {
		v.mis = &Mismatch{
			Method: e.Method, Path: e.Path,
			WantStatus: e.Status, GotStatus: resp.StatusCode,
			Want: respdiff.Truncate(want, 120), Got: respdiff.Truncate(got, 120),
		}
		return v
	}
	v.ok = true
	return v
}

// dynamicPaths finds probes whose normalized live bodies differ across
// repeats of the SAME request. The key is the full path INCLUDING the
// query: /items?page=1 and /items?page=2 are different questions, and
// treating them as one endpoint marks it dynamic and silently exempts
// every paginated or filtered route from body comparison.
func dynamicPaths(entries []Entry) map[string]bool {
	seen := map[string]string{}
	dyn := map[string]bool{}
	for _, e := range entries {
		key := e.Method + " " + e.Path
		n := respdiff.Normalize(string(e.RespBody))
		if prev, ok := seen[key]; ok && prev != n {
			dyn[key] = true
		}
		seen[key] = n
	}
	return dyn
}

// pathOnly strips the query string: /items?page=2 and /items?page=3 are
// the same endpoint for dynamism purposes but distinct replayed probes.
func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
