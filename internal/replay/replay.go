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
	"compress/gzip"
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
	// Host is the Host header as the client sent it. net/http promotes
	// it out of r.Header into r.Host, so cloning the header map alone
	// loses it - and a replayed request would then carry the fork's
	// loopback host:port, changing behavior for any app that reads Host
	// (absolute URLs, tenant routing).
	Host   string
	Header http.Header
	Body   []byte

	// At is when the exchange was recorded. Comparison uses it as
	// evidence spacing: two identical bodies recorded over a second
	// apart prove a path static in a way two same-tick samples cannot.
	At time.Time

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
		// Compressed bytes are useless to compare directly - the volatile
		// patterns never match and the diff prints binary garbage - but an
		// app with standard gzip middleware compresses nearly EVERYTHING,
		// and skipping those responses empties the buffer: the gate then
		// passes vacuously on an app it never actually probed. gzip is
		// stdlib, so those bodies are stored decompressed instead; rarer
		// encodings (br, zstd) stay uncapturable.
		enc := w.Header().Get("Content-Encoding")
		if bodyOverflow || rec.overflow || rec.hijacked || (enc != "" && enc != "gzip") {
			return
		}
		respBody := append([]byte(nil), rec.body.Bytes()...)
		if enc == "gzip" {
			plain, err := gunzip(respBody, bodyCap)
			if err != nil {
				return // truncated or corrupt stream: not a usable probe
			}
			respBody = plain
		}
		if r.Method == http.MethodHead {
			// net/http discards handler body writes on HEAD - the
			// recorder tees what the handler wrote, not what the client
			// received. Keeping it would make every replayed HEAD a
			// guaranteed body mismatch against the fork's (empty) answer.
			respBody = nil
		}
		b.add(Entry{
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			Host:         r.Host,
			Header:       r.Header.Clone(),
			Body:         append([]byte(nil), reqBody.Bytes()...),
			At:           time.Now(),
			Status:       rec.status,
			RespBody:     respBody,
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
// The categories are disjoint: Replayed = Matched + Dynamic + Mismatched.
// A self-dynamic entry whose status agrees counts as Dynamic only, never
// Matched - agents sum these numbers, and a total exceeding Replayed
// reads as broken arithmetic.
type Result struct {
	Replayed   int        `json:"replayed"`
	Matched    int        `json:"matched"` // fully compared, agreed
	Dynamic    int        `json:"dynamic"` // status-only paths (self-dynamic in the buffer), status agreed
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
	// Per-request cap follows the run budget: hard-coding it lower
	// would permanently fail any endpoint slower than the hard-code
	// even when the operator raised budget: exactly to accommodate it.
	client := &http.Client{
		Timeout:       budget,
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

	// A body mismatch can be the buffer's fault rather than the fork's:
	// dynamicPaths only marks a path self-dynamic when the buffer holds
	// two samples with different bodies, so a path recorded once - or a
	// seconds-grained counter recorded twice within the same tick -
	// arrives here with a stale recorded body that reads as drift.
	// Before letting such a mismatch stand, probe the path twice on the
	// fork itself, spaced apart: content that moves under the fork's own
	// feet cannot be evidence against it. Strictly a tiebreak for paths
	// the buffer holds no real evidence on: a path live served the same
	// body for across a second or more is proven static, and a fork
	// answering it nondeterministically (a dropped sort, a half-warmed
	// cache) is a regression the probe must not excuse. One probe pair
	// per path, safe methods only - re-issuing a recorded mutation is
	// not ours to decide.
	static := staticProven(entries)
	confirmed := map[string]bool{}
	for i, v := range verdicts {
		if !v.bodyMis || !safeMethod(entries[i].Method) {
			continue
		}
		key := entries[i].Method + " " + entries[i].Path
		if static[key] {
			continue
		}
		dyn, seen := confirmed[key]
		if !seen {
			if ctx.Err() != nil {
				continue
			}
			dyn = selfDynamicNow(ctx, client, backend, entries[i])
			confirmed[key] = dyn
		}
		if dyn {
			verdicts[i] = verdict{ok: true, dyn: true}
		}
	}

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
	bodyMis bool // the mismatch is body-level: status and contract headers agreed
	mis     *Mismatch
}

func buildReq(ctx context.Context, backend string, e Entry) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, e.Method, "http://"+backend+e.Path, bytes.NewReader(e.Body))
	if err != nil {
		return nil, err
	}
	req.Header = e.Header.Clone()
	if e.Host != "" {
		req.Host = e.Host // replay the Host the client sent, not the fork's hostport
	}
	return req, nil
}

func replayOne(ctx context.Context, client *http.Client, backend string, e Entry, dyn bool) (v verdict) {
	req, err := buildReq(ctx, backend, e)
	if err != nil {
		return v
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return v // budget expired mid-flight: not the fork's fault
		}
		v.mis = &Mismatch{Method: e.Method, Path: e.Path, WantStatus: e.Status, Got: "request failed: " + err.Error()}
		return v
	}
	defer resp.Body.Close()
	body := readCapped(resp)

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
	// Compare Location NORMALIZED, exactly as it is displayed. Raw
	// comparison made a volatile redirect target (/login?state=<uuid>)
	// a permanent mismatch whose report showed Want == Got - both sides
	// normalized to the identical placeholder - an undiagnosable
	// standing gate failure.
	if got := resp.Header.Get("Location"); e.RespLocation != "" && respdiff.Normalize(got) != respdiff.Normalize(e.RespLocation) {
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
		v.bodyMis = true
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

// confirmDelay spaces the two probes of a suspect path. Just over a
// second: the finest granularity time-driven content moves at in
// practice is a seconds counter, and two requests inside the same tick
// look static.
var confirmDelay = 1100 * time.Millisecond

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// selfDynamicNow reports whether the fork's own answer to e moves
// between two probes spaced confirmDelay apart. Only body movement at
// the recorded status is exculpatory - a status flap between probes is
// instability, not dynamism, and status is always compared even for
// dynamic paths. Probe errors, wrong statuses, and a budget expiry
// mid-wait report false: an unconfirmed mismatch stands.
func selfDynamicNow(ctx context.Context, client *http.Client, backend string, e Entry) bool {
	s1, b1, err := probe(ctx, client, backend, e)
	if err != nil || s1 != e.Status {
		return false
	}
	select {
	case <-time.After(confirmDelay):
	case <-ctx.Done():
		return false
	}
	s2, b2, err := probe(ctx, client, backend, e)
	if err != nil || s2 != e.Status {
		return false
	}
	return b1 != b2
}

// probe answers one request as a status and normalized body.
func probe(ctx context.Context, client *http.Client, backend string, e Entry) (int, string, error) {
	req, err := buildReq(ctx, backend, e)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	return resp.StatusCode, respdiff.Normalize(string(readCapped(resp))), nil
}

// readCapped reads a response body up to the cap, decompressing gzip -
// recorded bodies are stored decompressed, so comparisons must see the
// fork's the same way. An undecodable stream falls back to the raw
// bytes and lets the mismatch show.
func readCapped(resp *http.Response) []byte {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap+1))
	if resp.Header.Get("Content-Encoding") == "gzip" {
		if plain, err := gunzip(body, bodyCap); err == nil {
			return plain
		}
	}
	return body
}

// gunzip decompresses at most max bytes; larger or corrupt streams are
// not usable probes.
func gunzip(b []byte, max int) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, fmt.Errorf("decompressed body exceeds %d bytes", max)
	}
	return out, nil
}

// staticProven finds probes the buffer positively attests are static:
// at least two samples of the same request, identical normalized
// bodies, recorded over a second apart. Content moving at any
// realistic granularity would have differed across that span, so a
// fork answering such a path differently earned its mismatch and the
// confirm probes must not get a chance to excuse it.
func staticProven(entries []Entry) map[string]bool {
	type span struct {
		first, last time.Time
		n           int
	}
	seen := map[string]*span{}
	for _, e := range entries {
		key := e.Method + " " + e.Path
		s, ok := seen[key]
		if !ok {
			seen[key] = &span{first: e.At, last: e.At, n: 1}
			continue
		}
		s.n++
		if e.At.Before(s.first) {
			s.first = e.At
		}
		if e.At.After(s.last) {
			s.last = e.At
		}
	}
	out := map[string]bool{}
	for key, s := range seen {
		// Identical bodies is implied for the keys that reach the
		// confirm pass: differing bodies already landed in dynamicPaths.
		out[key] = s.n >= 2 && s.last.Sub(s.first) >= time.Second
	}
	return out
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
