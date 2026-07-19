// Package verify runs the configured checks against a fork before any
// traffic can reach it. A fork that fails here is destroyed, so hooks are
// the only gate between a push and production - default budgets err on the
// side of catching slow boots rather than failing fast.
package verify

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/docker"
)

const (
	httpBudget = 15 * time.Second
	runBudget  = 60 * time.Second
)

// Result is the outcome of one hook.
type Result struct {
	Hook   string `json:"hook"`
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms"`
	Detail string `json:"detail,omitempty"`
}

// Run executes all hooks in order against the fork. It returns every
// result (later hooks still run after a failure, so one push surfaces all
// problems) and whether the fork passed overall.
func Run(cfg *config.Config, container, backend string) ([]Result, bool) {
	results := make([]Result, 0, len(cfg.Verify))
	pass := true
	for _, h := range cfg.Verify {
		var r Result
		switch {
		case h.HTTP != "":
			r = httpHook(h.HTTP, backend, budget(h, httpBudget))
		case h.Run != "":
			r = runHook(h.Run, container, cfg.Workdir, budget(h, runBudget))
		}
		results = append(results, r)
		pass = pass && r.OK
	}
	return results, pass
}

// budget is the hook's own timeout if set, else the built-in default.
func budget(h config.VerifyHook, def time.Duration) time.Duration {
	if h.Timeout > 0 {
		return time.Duration(h.Timeout)
	}
	return def
}

// httpHook checks a hook of the form "/path == 200". It polls until the
// expected status appears or the budget runs out: a fork whose process is
// up but whose app is still warming counts as not-yet, not failed.
func httpHook(spec, backend string, budget time.Duration) Result {
	start := time.Now()
	res := Result{Hook: "http: " + spec}
	path, want, err := parseHTTPSpec(spec)
	if err != nil {
		res.Detail = err.Error()
		res.Ms = time.Since(start).Milliseconds()
		return res
	}

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(budget)
	last := "no response"
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + backend + path)
		if err != nil {
			last = err.Error()
		} else {
			resp.Body.Close()
			last = fmt.Sprintf("got %d", resp.StatusCode)
			if resp.StatusCode == want {
				res.OK = true
				res.Ms = time.Since(start).Milliseconds()
				return res
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	res.Detail = fmt.Sprintf("want %d, %s after %s", want, last, budget)
	res.Ms = time.Since(start).Milliseconds()
	return res
}

func parseHTTPSpec(spec string) (path string, status int, err error) {
	parts := strings.SplitN(spec, "==", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf(`malformed hook (want "/path == 200")`)
	}
	path = strings.TrimSpace(parts[0])
	status, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || !strings.HasPrefix(path, "/") {
		return "", 0, fmt.Errorf(`malformed hook (want "/path == 200")`)
	}
	return path, status, nil
}

// runHook executes a script inside the fork; exit 0 passes.
func runHook(script, container, workdir string, budget time.Duration) Result {
	start := time.Now()
	res := Result{Hook: "run: " + script}
	out, err := docker.Exec(container, workdir, script, budget)
	res.Ms = time.Since(start).Milliseconds()
	if err != nil {
		res.Detail = strings.TrimSpace(err.Error() + ": " + out)
		return res
	}
	res.OK = true
	return res
}
