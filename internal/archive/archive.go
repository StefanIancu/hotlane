// Package archive is the trust layer: the warm fork chain is a cache, and
// the archivist is its validation. After each promote it produces the
// reproducible artifact classical CI/CD would have built - asynchronously,
// off the push critical path - and periodically cold-boots that clean image
// to diff its behavior against the live warm instance. Divergence flags the
// app red and the next push rebuilds from the clean image, resetting the
// chain to a known-good state.
package archive

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/docker"
	"github.com/StefanIancu/hotlane/internal/notify"
	"github.com/StefanIancu/hotlane/internal/verify"
)

// Drift states.
const (
	DriftUnknown = "unknown" // no check has run against the current chain
	DriftClean   = "clean"
	DriftDrifted = "drifted"
)

// Status is the archivist's public state, surfaced in /v1/status.
type Status struct {
	Image       string `json:"image"`
	LastVersion int    `json:"last_version"`
	Building    bool   `json:"building"`
	Drift       string `json:"drift"`
	Detail      string `json:"detail,omitempty"`
	CheckedAt   string `json:"checked_at,omitempty"`
}

// Archivist owns the clean image and the drift verdict for one app.
type Archivist struct {
	Cfg      *config.Config
	DataDir  string
	Notifier *notify.Notifier

	mu     sync.Mutex
	status Status
}

func New(cfg *config.Config, dataDir string, n *notify.Notifier) *Archivist {
	return &Archivist{
		Cfg:      cfg,
		DataDir:  dataDir,
		Notifier: n,
		status:   Status{Image: "hotlane-" + cfg.App + ":clean", Drift: DriftUnknown},
	}
}

// CleanImage is the local tag of the reproducible image.
func (a *Archivist) CleanImage() string { return "hotlane-" + a.Cfg.App + ":clean" }

// Status returns a copy of the current state.
func (a *Archivist) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// Drifted reports whether the last drift check found divergence.
func (a *Archivist) Drifted() bool { return a.Status().Drift == DriftDrifted }

func (a *Archivist) srcDir() string { return filepath.Join(a.DataDir, "clean-src") }

// Snapshot copies the just-promoted source tree so the async build works
// from exactly what was pushed, no matter what later pushes do to the
// shadow. Call synchronously at promote time; it is a small local copy.
func (a *Archivist) Snapshot(src string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.RemoveAll(a.srcDir()); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "-R", src, a.srcDir()).CombinedOutput(); err != nil {
		return fmt.Errorf("archive snapshot: %v: %s", err, out)
	}
	return nil
}

// Archive builds the clean image from the last snapshot, pushes it to the
// configured registry if any, and runs a drift check against the live
// backend. Meant to be called in a goroutine after promote; overlapping
// calls collapse (the newest snapshot wins the next run).
func (a *Archivist) Archive(version int, liveBackend string) {
	a.mu.Lock()
	if a.status.Building {
		a.mu.Unlock()
		return
	}
	a.status.Building = true
	a.mu.Unlock()

	err := a.build()
	a.mu.Lock()
	a.status.Building = false
	if err != nil {
		a.status.Detail = "clean build failed: " + err.Error()
		a.mu.Unlock()
		log.Printf("archive: %v", err)
		a.Notifier.Send(notify.EventCleanBuildFailed, err.Error())
		return
	}
	a.status.LastVersion = version
	a.mu.Unlock()
	log.Printf("archive: clean image %s built for v%d", a.CleanImage(), version)

	if a.Cfg.Archive != "" {
		ref := fmt.Sprintf("%s:v%d", a.Cfg.Archive, version)
		if err := docker.TagImage(a.CleanImage(), ref); err == nil {
			if err := docker.Push(ref); err != nil {
				log.Printf("archive: pushing %s: %v (is docker logged in?)", ref, err)
			} else {
				log.Printf("archive: pushed %s", ref)
			}
		}
	}

	a.DriftCheck(liveBackend)
}

// build generates a Dockerfile from hotlane.yml and does the cold build.
func (a *Archivist) build() error {
	a.mu.Lock()
	src := a.srcDir()
	a.mu.Unlock()
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("no source snapshot yet: %w", err)
	}

	df := fmt.Sprintf("FROM %s\nWORKDIR %s\nCOPY . %s\n", a.Cfg.Image, a.Cfg.Workdir, a.Cfg.Workdir)
	if a.Cfg.Build != "" {
		df += fmt.Sprintf("RUN %s\n", a.Cfg.Build)
	}
	df += fmt.Sprintf("CMD [\"sh\", \"-c\", %q]\n", a.Cfg.RunCmd)
	if err := os.WriteFile(filepath.Join(src, "Dockerfile"), []byte(df), 0o644); err != nil {
		return err
	}
	// The snapshot may carry the app repo's .git; keep it out of the image.
	if err := os.WriteFile(filepath.Join(src, ".dockerignore"), []byte(".git\nDockerfile\n"), 0o644); err != nil {
		return err
	}
	return docker.Build(src, a.CleanImage())
}

// DriftCheck cold-boots the clean image and compares its behavior with the
// live warm instance: every verify hook must pass against the cold boot,
// and every http hook's response body must match what live serves. It is
// deliberately behavior-based - filesystems are allowed to differ, what
// the app does is not.
func (a *Archivist) DriftCheck(liveBackend string) Status {
	a.mu.Lock()
	defer a.mu.Unlock()

	name := "hotlane-" + a.Cfg.App + "-drift"
	docker.Remove(name)
	// The drift container gets its own label namespace so the pool never
	// adopts it as a serving version.
	labels := map[string]string{"hotlane.drift": a.Cfg.App}
	detail := ""

	err := docker.Create(name, a.CleanImage(), a.Cfg.Workdir, a.Cfg.Port, a.Cfg.RunCmd, labels)
	if err == nil {
		err = docker.Start(name)
	}
	var addr string
	if err == nil {
		addr, err = docker.HostAddr(name, a.Cfg.Port)
	}
	if err != nil {
		detail = "cold boot failed: " + err.Error()
	} else {
		results, pass := verify.Run(a.Cfg, name, addr)
		if !pass {
			for _, r := range results {
				if !r.OK {
					detail = fmt.Sprintf("cold boot fails verify: %s (%s)", r.Hook, r.Detail)
					break
				}
			}
		} else if diff := compareResponses(a.Cfg, addr, liveBackend); diff != "" {
			detail = diff
		}
	}
	docker.Remove(name)

	prev := a.status.Drift
	a.status.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	if detail == "" {
		a.status.Drift = DriftClean
		a.status.Detail = ""
		log.Printf("archive: drift check CLEAN (cold boot behaves like live)")
	} else {
		a.status.Drift = DriftDrifted
		a.status.Detail = detail
		log.Printf("archive: drift check DRIFTED: %s", detail)
	}

	// Notify on transitions only: a 6-hourly check in a persistent state
	// should ping once when it breaks and once when it heals, not every
	// run - alert fatigue is how notifications get ignored.
	switch {
	case a.status.Drift == DriftDrifted && prev != DriftDrifted:
		a.Notifier.Send(notify.EventDriftDetected, detail+"\nnext push will rebuild from "+a.CleanImage())
	case a.status.Drift == DriftClean && prev == DriftDrifted:
		a.Notifier.Send(notify.EventDriftHealed, "")
	}
	return a.status
}

// compareResponses diffs the body and status of every http verify hook
// path between the cold boot and live. Returns "" when they match.
func compareResponses(cfg *config.Config, coldAddr, liveAddr string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, h := range cfg.Verify {
		if h.HTTP == "" {
			continue
		}
		path := strings.TrimSpace(strings.SplitN(h.HTTP, "==", 2)[0])
		cold, cerr := get(client, coldAddr, path)
		live, lerr := get(client, liveAddr, path)
		if cerr != nil || lerr != nil {
			return fmt.Sprintf("comparing %s: cold=%v live=%v", path, cerr, lerr)
		}
		if cold != live {
			return fmt.Sprintf("behavior differs on %s: clean build serves %q, live serves %q",
				path, truncate(cold, 120), truncate(live, 120))
		}
	}
	return ""
}

func get(c *http.Client, addr, path string) (string, error) {
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d %s", resp.StatusCode, body), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
