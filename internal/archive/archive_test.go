package archive

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/StefanIancu/hotlane/internal/config"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"started":"2026-07-19T14:03:22Z"}`, `{"started":"<ts>"}`},
		{`{"started":"2026-07-19 14:03:22.123+02:00"}`, `{"started":"<ts>"}`},
		{`Date: Sat, 19 Jul 2026 14:03:22 GMT`, `Date: <ts>`},
		{`{"req":"7f9c2ba4-e88f-11e9-a1b2-9cb6d0d493a1"}`, `{"req":"<uuid>"}`},
		{`trace 4bf92f3577b34da6a3ce929d0e0e4736 done`, `trace <hex> done`},
		{`{"ts":1784551402,"ms":1784551402123}`, `{"ts":<num>,"ms":<num>}`},
		// stays put: short numbers, version strings, plain words
		{`{"version":"1.2.3","port":3000,"items":42}`, `{"version":"1.2.3","port":3000,"items":42}`},
		{`hello from demo-app v2`, `hello from demo-app v2`},
	}
	for _, c := range cases {
		if got := normalize(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

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
