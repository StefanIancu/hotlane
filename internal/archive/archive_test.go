package archive

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/replay"
)

// serve returns an httptest server whose body comes from fn on each request.
func serve(fn func(n int) string) *httptest.Server {
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		fmt.Fprint(w, fn(n))
	}))
}

func addr(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

func cfgWith(paths ...string) *config.Config {
	c := &config.Config{}
	for _, p := range paths {
		c.Verify = append(c.Verify, config.VerifyHook{HTTP: p + " == 200"})
	}
	return c
}

func TestCompareIdenticalStatic(t *testing.T) {
	cold := serve(func(int) string { return "hello" })
	defer cold.Close()
	live := serve(func(int) string { return "hello" })
	defer live.Close()
	if d := compareResponses(cfgWith("/"), addr(cold), addr(live)); d != "" {
		t.Errorf("unexpected drift: %s", d)
	}
}

func TestCompareRealDrift(t *testing.T) {
	cold := serve(func(int) string { return "hello" })
	defer cold.Close()
	live := serve(func(int) string { return "TAMPERED" })
	defer live.Close()
	if d := compareResponses(cfgWith("/"), addr(cold), addr(live)); !strings.Contains(d, "behavior differs") {
		t.Errorf("tampering not detected: %q", d)
	}
}

func TestCompareToleratesTimestamps(t *testing.T) {
	// per-process start time: stable within an instance, different between
	// cold and live - the classic false positive
	cold := serve(func(int) string { return `{"up":true,"since":"2026-07-19T14:00:01Z"}` })
	defer cold.Close()
	live := serve(func(int) string { return `{"up":true,"since":"2026-07-12T09:31:47Z"}` })
	defer live.Close()
	if d := compareResponses(cfgWith("/health"), addr(cold), addr(live)); d != "" {
		t.Errorf("timestamp read as drift: %s", d)
	}
}

func TestCompareSkipsSelfDynamicBody(t *testing.T) {
	// request counter: differs between two requests to the same instance,
	// so the body cannot be drift evidence
	cold := serve(func(n int) string { return fmt.Sprintf("count %d", n) })
	defer cold.Close()
	live := serve(func(n int) string { return fmt.Sprintf("count %d", n+7000) })
	defer live.Close()
	if d := compareResponses(cfgWith("/"), addr(cold), addr(live)); d != "" {
		t.Errorf("self-dynamic body read as drift: %s", d)
	}
}

// recordVia drives requests through a replay.Capture server to build a
// realistic recorded slice.
func recordVia(t *testing.T, h http.HandlerFunc, paths ...string) []replay.Entry {
	t.Helper()
	b := replay.NewBuffer(16)
	srv := httptest.NewServer(replay.Capture(h, b, map[string]bool{"GET": true}, nil))
	defer srv.Close()
	for _, p := range paths {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	return b.Snapshot(len(paths))
}

func TestReplayDriftCleanWhenColdMatchesRecorded(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "page %s", r.URL.Path) }
	entries := recordVia(t, h, "/a", "/b")
	cold := httptest.NewServer(http.HandlerFunc(h))
	defer cold.Close()
	if d := replayDrift(entries, len(entries), addr(cold), time.Second); d != "" {
		t.Errorf("unexpected drift: %s", d)
	}
}

func TestReplayDriftFlagsNonHookPathDivergence(t *testing.T) {
	// The phase-2 point: a path no verify hook names, but users hit it -
	// the cold boot answering it differently IS drift.
	entries := recordVia(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "live truth") }, "/unhooked")
	cold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "TAMPERED") }))
	defer cold.Close()
	d := replayDrift(entries, 1, addr(cold), time.Second)
	if !strings.Contains(d, "replayed traffic differs on GET /unhooked") {
		t.Errorf("divergence not flagged: %q", d)
	}
}

func TestReplayDriftEmptySliceIsClean(t *testing.T) {
	if d := replayDrift(nil, 0, "127.0.0.1:1", time.Second); d != "" {
		t.Errorf("empty slice produced drift: %q", d)
	}
}

func TestCompareStatusStillMattersOnDynamicPath(t *testing.T) {
	cold := serve(func(n int) string { return fmt.Sprintf("count %d", n) })
	defer cold.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer live.Close()
	if d := compareResponses(cfgWith("/"), addr(cold), addr(live)); !strings.Contains(d, "answers") {
		t.Errorf("status divergence not detected: %q", d)
	}
}

// A pushed diff can make `Dockerfile` a symlink pointing anywhere on the
// host; git apply permits it and cp -R preserves it. Writing through it
// would let any push truncate arbitrary files as the daemon user.
func TestWriteNoFollowRefusesToWriteThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "HOST_SECRET")
	if err := os.WriteFile(target, []byte("original host content"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "snapshot")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(src, "Dockerfile")
	if err := os.Symlink(target, planted); err != nil {
		t.Fatal(err)
	}

	if err := writeNoFollow(planted, []byte("FROM scratch\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	host, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(host) != "original host content" {
		t.Errorf("HOST FILE OVERWRITTEN through the symlink: %q", host)
	}
	got, err := os.ReadFile(planted)
	if err != nil || string(got) != "FROM scratch\n" {
		t.Errorf("Dockerfile not written in place: %q %v", got, err)
	}
	if fi, err := os.Lstat(planted); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Error("path is still a symlink after write")
	}
}

// Drift details are logged and POSTed to the notify webhook, so they
// must never carry recorded user traffic.
func TestReplayDriftDetailCarriesNoUserContent(t *testing.T) {
	secretBody := `{"email":"alice@corp.com","balance":1420}`
	entries := recordVia(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, secretBody) },
		"/api/me?token=SECRETQUERYTOKEN&email=alice@corp.com")
	cold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"email":"bob@corp.com","balance":0}`)
	}))
	defer cold.Close()

	detail := replayDrift(entries, 1, addr(cold), time.Second)
	if detail == "" {
		t.Fatal("expected drift to be reported")
	}
	for _, leak := range []string{"alice@corp.com", "bob@corp.com", "1420", "SECRETQUERYTOKEN", "token="} {
		if strings.Contains(detail, leak) {
			t.Errorf("drift detail leaks %q into logs/webhook:\n%s", leak, detail)
		}
	}
	if !strings.Contains(detail, "/api/me") {
		t.Errorf("detail should still name the endpoint:\n%s", detail)
	}
}
