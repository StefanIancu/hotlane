package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/StefanIancu/hotlane/internal/archive"
	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/docker"
	"github.com/StefanIancu/hotlane/internal/notify"
	"github.com/StefanIancu/hotlane/internal/pool"
	"github.com/StefanIancu/hotlane/internal/proxy"
	"github.com/StefanIancu/hotlane/internal/replay"
	"github.com/StefanIancu/hotlane/internal/verify"
)

// appRuntime is one app's complete serving machinery: warm pool + ring,
// traffic proxy target, archivist, notifier, and the per-app push lock.
// cmdServe wires exactly one today; multi-app daemons (docs/multi-app.md)
// will wire one per config behind the shared listeners.
type appRuntime struct {
	cfg    *config.Config
	pool   *pool.Pool
	front  *proxy.Flipper
	arch   *archive.Archivist
	notif  *notify.Notifier
	buffer *replay.Buffer // live-traffic ring; nil when replay is off

	rebaseDepth int
	holdTTL     time.Duration

	// Pushes are serialized per app: forks share the shadow tree and the
	// version counter, and one verified promote at a time is the contract.
	pushMu sync.Mutex
}

// newAppRuntime boots one app: warm pool, proxy target, held-fork reaper,
// archivist (with the first-boot source snapshot), and the initial
// archive build. src is the app's source checkout; dataRoot the daemon's
// state directory (state lands under <dataRoot>/<app>).
func newAppRuntime(cfg *config.Config, src, dataRoot string) (*appRuntime, error) {
	a := &appRuntime{
		cfg:         cfg,
		pool:        &pool.Pool{Cfg: cfg, Src: src, DataDir: filepath.Join(dataRoot, cfg.App)},
		front:       proxy.New(),
		rebaseDepth: 40,
		holdTTL:     15 * time.Minute,
	}
	if v, err := strconv.Atoi(os.Getenv("HOTLANE_REBASE_DEPTH")); err == nil && v > 0 {
		a.rebaseDepth = v
	}
	if v, err := time.ParseDuration(os.Getenv("HOTLANE_HOLD_TTL")); err == nil && v > 0 {
		a.holdTTL = v
	}

	if err := a.pool.Ensure(); err != nil {
		return nil, fmt.Errorf("warm pool: %w", err)
	}
	st := a.pool.State()
	a.front.Set(st.Backend)
	a.pool.StartHeldReaper(&a.pushMu)

	a.notif = &notify.Notifier{URL: cfg.Notify, App: cfg.App}
	a.arch = archive.New(cfg, a.pool.DataDir, a.notif)
	if cfg.Replay.Enabled() {
		a.buffer = replay.NewBuffer(0)
		a.arch.ReplayEntries = func() ([]replay.Entry, int) {
			return a.buffer.Snapshot(cfg.Replay.Last), a.buffer.Len()
		}
	}
	// First boot: snapshot the checkout so a clean image exists from day
	// one. Restarts keep the existing snapshot - it holds the last
	// promoted source, while the checkout may be stale (pushes deliver
	// diffs and never touch it); overwriting it regressed the clean
	// image and false-positived every post-restart drift check.
	if !a.arch.HasSnapshot() {
		if err := a.arch.Snapshot(src); err != nil {
			log.Printf("hotlane: archive snapshot: %v", err)
		}
	}
	go a.arch.Archive(st.Version, a.liveBackend)
	return a, nil
}

// startDriftTicker runs the daemon's one periodic drift check. Apps
// check sequentially, not in parallel - each check cold-boots a
// container, and N apps doing that at the same instant would spike the
// box for no gain.
func startDriftTicker(apps []*appRuntime) {
	go func() {
		for range time.Tick(6 * time.Hour) {
			for _, a := range apps {
				a.arch.DriftCheck(a.pool.State().Backend)
			}
		}
	}()
}

// trafficHandler serves the app's public traffic: live by default; the
// X-Hotlane-Fork header routes to a held (test-mode) fork. Unknown fork =
// explicit error, never a silent fall-through to live - an agent must not
// believe it tested a fork when it actually hit production.
func (a *appRuntime) trafficHandler() http.Handler {
	live := http.Handler(a.front)
	if a.buffer != nil {
		// Only genuinely live-bound traffic is recorded: fork pokes are
		// routed away above, so they never pollute the replay buffer.
		live = replay.Capture(live, a.buffer, a.cfg.Replay.MethodSet(), a.cfg.Replay.Exclude)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fv := r.Header.Get("X-Hotlane-Fork"); fv != "" {
			if h := a.pool.HeldProxy(fv); h != nil {
				h.ServeHTTP(w, r)
				return
			}
			// Deliberately identical for "no such fork" and "wrong
			// token": this path is unauthenticated, so the response
			// must not confirm that a version exists.
			http.Error(w, "hotlane: no held fork for that X-Hotlane-Fork value (expired, promoted, discarded, or wrong token?)", http.StatusMisdirectedRequest)
			return
		}
		live.ServeHTTP(w, r)
	})
}

// liveBackend is read at drift-check time, not when the build was
// queued: a rollback during a multi-minute clean build would otherwise
// leave the archivist comparing against a stopped container's address,
// reporting false drift and forcing every later push through the clean
// image - silently discarding the warm caches.
func (a *appRuntime) liveBackend() string { return a.pool.State().Backend }

// resetBuffer drops the recorded slice on any traffic flip - see
// replay.Buffer.Reset for why stale recordings are poison.
func (a *appRuntime) resetBuffer() {
	if a.buffer != nil {
		a.buffer.Reset()
	}
}

// runReplay replays the buffered live slice against a verified fork.
// Nil when replay is off or nothing is buffered yet.
func (a *appRuntime) runReplay(backend string) *replay.Result {
	if a.buffer == nil {
		return nil
	}
	entries := a.buffer.Snapshot(a.cfg.Replay.Last)
	res := replay.Run(entries, a.buffer.Len(), backend, a.cfg.Replay.BudgetOrDefault())
	return &res
}

// forkBase picks what to fork from: the clean image instead of the warm
// chain when the chain is untrustworthy (drift) or too deep (Docker's
// overlayfs layer limit, ~125; every promoted fork adds one layer).
// logReason preserves push's log lines without adding new ones to test.
func (a *appRuntime) forkBase(logReason bool) string {
	// Both branches below need the clean image to actually exist. Drift
	// can be flagged before the first clean build finishes (a manual
	// drift-check at startup fails its cold boot), and forking from a
	// nonexistent image fails every push until the build lands.
	if a.arch.Drifted() && docker.ImageExists(a.arch.CleanImage()) {
		if logReason {
			log.Printf("push: forking from clean (drift recovery)")
		}
		return a.arch.CleanImage()
	}
	// Rebase on how far the CHAIN has grown, not on absolute image
	// depth. Docker's ~125 layer cap is what we are avoiding, but the
	// base image's own layers are a fixed cost the chain never adds to.
	// Measured absolutely, an app whose base image is already deep would
	// rebase on every single push - silently losing the warm caches that
	// make hotlane fast, with no diagnostic.
	clean := a.arch.CleanImage()
	if !docker.ImageExists(clean) {
		return ""
	}
	d, err := docker.LayerDepth(a.pool.State().Live)
	if err != nil {
		return ""
	}
	base, err := docker.ImageLayerDepth(clean)
	if err != nil {
		base = 0 // no reference point: fall back to absolute depth
	}
	if grown := d - base; grown >= a.rebaseDepth {
		if logReason {
			log.Printf("push: forking from clean (chain grew %d layers past %s; total depth %d)", grown, clean, d)
		}
		return clean
	}
	return ""
}

// ready adapts the verify hooks to the "is this container servable"
// signature Rollback/PromoteHeld gate on.
func (a *appRuntime) ready(container, backend string) error {
	results, ok := verify.Run(a.cfg, container, backend)
	if ok {
		return nil
	}
	for _, r := range results {
		if !r.OK {
			return fmt.Errorf("%s - %s", r.Hook, r.Detail)
		}
	}
	return fmt.Errorf("verify failed")
}

func (a *appRuntime) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := a.pool.State()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"app":             a.cfg.App,
		"live":            st.Live,
		"version":         st.Version,
		"backend":         a.front.Target(),
		"baseline_commit": st.Baseline,
		"last_fork":       st.LastFork,
		"ring":            a.pool.Ring(),
		"held":            a.pool.HeldList(),
		"archive":         a.arch.Status(),
		"replay":          a.replayStatus(),
	})
}

// replayStatus is the status block's replay summary.
func (a *appRuntime) replayStatus() map[string]any {
	if a.buffer == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":  true,
		"mode":     a.cfg.Replay.ModeOrDefault(),
		"buffered": a.buffer.Len(),
		"last":     a.cfg.Replay.Last,
	}
}

func (a *appRuntime) handleLogs(w http.ResponseWriter, r *http.Request) {
	tail := 100
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 && n <= 10000 {
			tail = n
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, docker.Logs(a.pool.State().Live, tail))
}

func (a *appRuntime) handleDriftCheck(w http.ResponseWriter, r *http.Request) {
	st := a.arch.DriftCheck(a.pool.State().Backend)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (a *appRuntime) handleRollback(w http.ResponseWriter, r *http.Request) {
	// Rollback flips traffic, so it must serialize with the other
	// flippers. Without this, a rollback landing while a push is in its
	// verify window is silently undone moments later by that push's
	// promote - the operator gets a success response and then keeps
	// serving the exact version they were escaping.
	a.pushMu.Lock()
	defer a.pushMu.Unlock()
	var req struct {
		Version int `json:"version"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	res, err := a.pool.Rollback(req.Version, a.ready)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	a.front.Set(res.Backend)
	a.resetBuffer() // recorded traffic described the version rolled away from
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (a *appRuntime) handlePush(w http.ResponseWriter, r *http.Request) {
	diff, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.pushMu.Lock()
	defer a.pushMu.Unlock()

	res, err := a.pool.Fork(diff, a.forkBase(true))
	if err != nil {
		log.Printf("push: %v", err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	out := pushResponse{ForkResult: res}

	vStart := time.Now()
	out.Verify, out.Promoted = verify.Run(a.cfg, res.Container, res.Backend)
	out.VerifyMs = time.Since(vStart).Milliseconds()
	out.TotalMs += out.VerifyMs

	w.Header().Set("Content-Type", "application/json")
	if !out.Promoted {
		out.Logs = a.pool.Discard(res)
		for _, v := range out.Verify {
			if !v.OK {
				a.notif.Send(notify.EventPushRejected,
					fmt.Sprintf("v%d: %s - %s", res.Version, v.Hook, v.Detail))
				break
			}
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(out)
		return
	}

	// Shadow testing: replay recorded live traffic at the verified fork
	// before it takes over. gate mode treats a mismatch exactly like a
	// failing verify hook; report mode promotes and tells the truth.
	if out.Replay = a.runReplay(res.Backend); out.Replay != nil && out.Replay.Mismatched > 0 {
		// Endpoint(), not Path: this goes to the webhook, and recorded
		// query strings carry user tokens and email addresses.
		detail := fmt.Sprintf("v%d: %d/%d replayed requests answered differently (e.g. %s)",
			res.Version, out.Replay.Mismatched, out.Replay.Replayed,
			out.Replay.Mismatches[0].Endpoint())
		a.notif.Send(notify.EventReplayMismatch, detail)
		if a.cfg.Replay.Gate() {
			out.Promoted = false
			out.Logs = a.pool.Discard(res)
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(out)
			return
		}
	}
	a.pool.Promote(res)
	a.front.Set(res.Backend)
	a.resetBuffer() // recorded traffic described the previous version
	if err := a.arch.Snapshot(a.pool.ShadowDir()); err != nil {
		log.Printf("push: archive snapshot: %v", err)
	} else {
		go a.arch.Archive(res.Version, a.liveBackend)
	}
	json.NewEncoder(w).Encode(out)
}

// handleTest is push's sibling: fork + verify, then HOLD instead of
// promote. The caller pokes the fork through the X-Hotlane-Fork header,
// then promotes/discards.
func (a *appRuntime) handleTest(w http.ResponseWriter, r *http.Request) {
	diff, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.pushMu.Lock()
	defer a.pushMu.Unlock()

	res, err := a.pool.Fork(diff, a.forkBase(false))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	out := pushResponse{ForkResult: res}
	vStart := time.Now()
	var ok bool
	out.Verify, ok = verify.Run(a.cfg, res.Container, res.Backend)
	out.VerifyMs = time.Since(vStart).Milliseconds()
	out.TotalMs += out.VerifyMs
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		out.Logs = a.pool.Discard(res)
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(out)
		return
	}
	// Held forks always get the replay report and never the gate: the
	// whole point of test is that a human or agent reads the evidence
	// and decides promote/discard themselves.
	out.Replay = a.runReplay(res.Backend)
	if out.Replay != nil && out.Replay.Mismatched > 0 {
		a.notif.Send(notify.EventReplayMismatch,
			fmt.Sprintf("held v%d: %d/%d replayed requests answered differently",
				res.Version, out.Replay.Mismatched, out.Replay.Replayed))
	}
	if err := a.pool.Hold(res, a.holdTTL); err != nil {
		a.pool.Discard(res)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	held := a.pool.Held(res.Version)
	json.NewEncoder(w).Encode(map[string]any{
		"fork":       out,
		"held":       true,
		"expires_in": a.holdTTL.String(),
		"header":     "X-Hotlane-Fork: " + held.Header(),
	})
}

func (a *appRuntime) handlePromote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.pushMu.Lock()
	defer a.pushMu.Unlock()
	srcDir, res, err := a.pool.PromoteHeld(req.Version, a.ready)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	a.front.Set(res.Backend)
	a.resetBuffer() // recorded traffic described the previous version
	if err := a.arch.Snapshot(srcDir); err != nil {
		log.Printf("promote: archive snapshot: %v", err)
	} else {
		go a.arch.Archive(res.Version, a.liveBackend)
	}
	os.RemoveAll(srcDir)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (a *appRuntime) handleDiscard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.pushMu.Lock()
	defer a.pushMu.Unlock()
	if err := a.pool.DiscardHeld(req.Version); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
