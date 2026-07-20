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
	"github.com/StefanIancu/hotlane/internal/replay"
	"github.com/StefanIancu/hotlane/internal/respdiff"
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
	// ReplayEntries supplies the recorded live-traffic slice (entries +
	// buffered count) for drift checks; nil when replay is off. Phase 2
	// of docs/traffic-replay.md: the cold boot must answer the traffic
	// users actually sent, not just the paths named in verify hooks.
	ReplayEntries func() ([]replay.Entry, int)

	mu sync.Mutex // guards status only - never held across docker work
	// checkMu serializes drift checks with each other (they share one
	// container name) WITHOUT blocking status reads or the push path.
	checkMu        sync.Mutex
	status         Status
	pendingVersion int    // a promote arrived mid-build; run again for this version
	pendingBackend string // ...against this live backend
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

// HasSnapshot reports whether a source snapshot already exists. The
// snapshot is written at every promote, so across a daemon restart it
// holds the last PROMOTED source - which is the truth to rebuild from.
// The working tree the daemon was started from may be stale (pushes
// deliver source as diffs and never touch the checkout), so restart
// paths must not overwrite an existing snapshot with it.
func (a *Archivist) HasSnapshot() bool {
	_, err := os.Stat(a.srcDir())
	return err == nil
}

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
// backend. Meant to be called in a goroutine after promote. Overlapping
// calls collapse: a promote landing mid-build queues exactly one follow-up
// run for the newest version - dropped runs would leave a stale clean
// image and false-positive drift verdicts under rapid pushes.
func (a *Archivist) Archive(version int, liveBackend string) {
	a.mu.Lock()
	if a.status.Building {
		a.pendingVersion, a.pendingBackend = version, liveBackend
		a.mu.Unlock()
		return
	}
	a.status.Building = true
	a.mu.Unlock()

	for {
		if err := a.build(); err != nil {
			a.mu.Lock()
			a.status.Detail = "clean build failed: " + err.Error()
			a.mu.Unlock()
			log.Printf("archive: %v", err)
			a.Notifier.Send(notify.EventCleanBuildFailed, err.Error())
		} else {
			a.mu.Lock()
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
			// Only drift-check when no newer promote is queued: under
			// rapid pushes the live app is already versions ahead of
			// this build, and comparing them reads as drift, wrongly
			// sending the next push through a needless clean rebase.
			a.mu.Lock()
			stale := a.pendingVersion != 0
			a.mu.Unlock()
			if stale {
				log.Printf("archive: skipping drift check for v%d (newer promote queued)", version)
			} else {
				a.DriftCheck(liveBackend)
			}
		}

		a.mu.Lock()
		if a.pendingVersion != 0 {
			version, liveBackend = a.pendingVersion, a.pendingBackend
			a.pendingVersion = 0
			a.mu.Unlock()
			continue
		}
		a.status.Building = false
		a.mu.Unlock()
		return
	}
}

// buildSlots caps concurrent clean builds across every app on the
// daemon: one Docker daemon serves them all, and N archivists building
// at once starve the push path's own Docker work. Two slots let a
// second app make progress without a build free-for-all.
var buildSlots = make(chan struct{}, 2)

// build generates a Dockerfile from hotlane.yml and does the cold build.
func (a *Archivist) build() error {
	// Build from a private copy. clean-src is rewritten by every promote
	// (Snapshot does RemoveAll + cp -R), so building straight out of it
	// races the next push: the tree can vanish or be half-copied
	// mid-build, producing an empty or torn "clean" image that then
	// false-positives drift or becomes the base for drift recovery.
	a.mu.Lock()
	snap := a.srcDir()
	src := filepath.Join(a.DataDir, "clean-build")
	if _, err := os.Stat(snap); err != nil {
		a.mu.Unlock()
		return fmt.Errorf("no source snapshot yet: %w", err)
	}
	if err := os.RemoveAll(src); err != nil {
		a.mu.Unlock()
		return err
	}
	if out, err := exec.Command("cp", "-R", snap, src).CombinedOutput(); err != nil {
		a.mu.Unlock()
		return fmt.Errorf("staging clean build: %v: %s", err, out)
	}
	a.mu.Unlock()

	df := fmt.Sprintf("FROM %s\nWORKDIR %s\nCOPY . %s\n", a.Cfg.Image, a.Cfg.Workdir, a.Cfg.Workdir)
	if a.Cfg.Build != "" {
		df += fmt.Sprintf("RUN %s\n", a.Cfg.Build)
	}
	df += fmt.Sprintf("CMD [\"sh\", \"-c\", %q]\n", a.Cfg.RunCmd)
	if err := writeNoFollow(filepath.Join(src, "Dockerfile"), []byte(df)); err != nil {
		return err
	}
	// The snapshot may carry the app repo's .git; keep it out of the image.
	if err := writeNoFollow(filepath.Join(src, ".dockerignore"), []byte(".git\nDockerfile\n")); err != nil {
		return err
	}
	buildSlots <- struct{}{}
	defer func() { <-buildSlots }()
	return docker.Build(src, a.CleanImage())
}

// writeNoFollow writes a file into the snapshot without ever following a
// symlink at that path. The snapshot is built from a pushed diff, and
// `git apply` will happily create `Dockerfile` as a symlink to anywhere
// on the host - writing through it would let a push truncate arbitrary
// files as the daemon user, which drives Docker and is usually root.
func writeNoFollow(path string, data []byte) error {
	// Remove unlinks the symlink itself, never its target.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// DriftCheck cold-boots the clean image and compares its behavior with the
// live warm instance: every verify hook must pass against the cold boot,
// and every http hook's response body must match what live serves. It is
// deliberately behavior-based - filesystems are allowed to differ, what
// the app does is not.
func (a *Archivist) DriftCheck(liveBackend string) Status {
	// Only one drift check at a time (they share a container name), but
	// the status mutex stays free: holding it across a cold boot froze
	// GET /-/v1/status AND every push - forkBase calls Drifted() inside
	// the push lock - for the whole check, every six hours.
	a.checkMu.Lock()
	defer a.checkMu.Unlock()

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
		} else if a.ReplayEntries != nil {
			entries, buffered := a.ReplayEntries()
			detail = replayDrift(entries, buffered, addr, a.Cfg.Replay.BudgetOrDefault())
		}
	}
	docker.Remove(name)

	a.mu.Lock()
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

	out := a.status
	a.mu.Unlock()

	// Notify on transitions only: a 6-hourly check in a persistent state
	// should ping once when it breaks and once when it heals, not every
	// run - alert fatigue is how notifications get ignored.
	switch {
	case out.Drift == DriftDrifted && prev != DriftDrifted:
		a.Notifier.Send(notify.EventDriftDetected, detail+"\nnext push will rebuild from "+a.CleanImage())
	case out.Drift == DriftClean && prev == DriftDrifted:
		a.Notifier.Send(notify.EventDriftHealed, "")
	}
	return out
}

// replayDrift replays the recorded live slice against the cold boot:
// behavioral drift coverage across the endpoints users actually
// exercise, not just the ones named in config. Returns "" when the cold
// boot answers recent real traffic the way live did.
func replayDrift(entries []replay.Entry, buffered int, coldAddr string, budget time.Duration) string {
	if len(entries) == 0 {
		return ""
	}
	res := replay.Run(entries, buffered, coldAddr, budget)
	if res.Mismatched == 0 {
		return ""
	}
	// This string is logged AND sent to the notify webhook, so it must
	// carry no recorded traffic content - no bodies, no query strings.
	// The endpoint and the counts are enough to act on; the bodies stay
	// behind the authenticated API (`hotlane push`/`test` output).
	m := res.Mismatches[0]
	if m.WantStatus != m.GotStatus && m.GotStatus != 0 {
		return fmt.Sprintf("replayed traffic differs on %s: live answered %d, clean build answers %d (%d/%d replayed requests differ)",
			m.Endpoint(), m.WantStatus, m.GotStatus, res.Mismatched, res.Replayed)
	}
	return fmt.Sprintf("replayed traffic differs on %s: response body differs from what live served (%d/%d replayed requests differ)",
		m.Endpoint(), res.Mismatched, res.Replayed)
}

// compareResponses diffs the behavior of every http verify hook path
// between the cold boot and live. Returns "" when they match.
//
// Bodies are compared after normalization (timestamps, UUIDs, hex ids,
// epoch-scale numbers become placeholders), and each instance is sampled
// twice: whatever still differs between two requests to the SAME server
// is dynamic content, which cannot be evidence of drift either way - for
// such paths only the status code is compared.
func compareResponses(cfg *config.Config, coldAddr, liveAddr string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, h := range cfg.Verify {
		if h.HTTP == "" {
			continue
		}
		path := strings.TrimSpace(strings.SplitN(h.HTTP, "==", 2)[0])
		cold, cerr := sample(client, coldAddr, path)
		live, lerr := sample(client, liveAddr, path)
		if cerr != nil || lerr != nil {
			return fmt.Sprintf("comparing %s: cold=%v live=%v", path, cerr, lerr)
		}
		if cold.status != live.status {
			return fmt.Sprintf("behavior differs on %s: clean build answers %d, live answers %d",
				path, cold.status, live.status)
		}
		if cold.dynamic || live.dynamic {
			log.Printf("archive: drift check: %s is dynamic (differs between two requests to the same instance); comparing status only", path)
			continue
		}
		if cold.body != live.body {
			return fmt.Sprintf("behavior differs on %s: clean build serves %q, live serves %q",
				path, respdiff.Truncate(cold.body, 120), respdiff.Truncate(live.body, 120))
		}
	}
	return ""
}

// sampled is one path's observed behavior: two requests, normalized.
type sampled struct {
	status  int
	body    string // first normalized body
	dynamic bool   // normalized bodies differed between the two requests
}

func sample(c *http.Client, addr, path string) (sampled, error) {
	s1, b1, err := get(c, addr, path)
	if err != nil {
		return sampled{}, err
	}
	s2, b2, err := get(c, addr, path)
	if err != nil {
		return sampled{}, err
	}
	n1, n2 := respdiff.Normalize(b1), respdiff.Normalize(b2)
	return sampled{status: s1, body: n1, dynamic: n1 != n2 || s1 != s2}, nil
}

func get(c *http.Client, addr, path string) (int, string, error) {
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(body), nil
}
