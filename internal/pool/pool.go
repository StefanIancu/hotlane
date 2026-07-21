// Package pool manages the warm pool: the live, running instance of an app
// that forks are taken from and traffic is routed to. Ensure (M1) adopts or
// creates the live instance; Fork (M2) turns a git delta into a booted,
// side-lined instance carrying the live version's warm state.
package pool

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/docker"
)

// maxHeld caps concurrently held forks: each holds a running container.
const maxHeld = 3

// Pool tracks the live version of one app.
type Pool struct {
	Cfg     *config.Config
	Src     string // app source directory (where hotlane.yml lives)
	DataDir string // daemon state: pristine + shadow source copies

	mu       sync.Mutex
	next     int      // next version number to assign
	stopping sync.Map // container name -> chan struct{}, closed when its stop completes

	Live     string // live container name
	Backend  string // loopback host:port the proxy targets
	Version  int
	LastFork *ForkResult

	held map[int]*HeldFork // test-mode forks awaiting promote/discard

	// BaselineCommit is the git commit the pristine snapshot was taken at
	// (empty if the source is not a git checkout). Diffs are applied
	// against pristine, so this is the commit CI clients diff from.
	BaselineCommit string
}

// ForkResult describes a booted fork and where the time went. The phase
// timings are the product being measured - keep them honest.
type ForkResult struct {
	Container  string `json:"container"`
	Backend    string `json:"backend"`
	Version    int    `json:"version"`
	FromClean  bool   `json:"from_clean,omitempty"` // forked from the archivist's clean image, not the live chain
	SnapshotMs int64  `json:"snapshot_ms"`
	PatchMs    int64  `json:"patch_ms"`
	BootMs     int64  `json:"boot_ms"`
	TotalMs    int64  `json:"total_ms"`
}

func containerName(app string, version int) string {
	return fmt.Sprintf("hotlane-%s-v%d", app, version)
}

func (p *Pool) imageRef(version int) string {
	return fmt.Sprintf("hotlane-%s:v%d", p.Cfg.App, version)
}

// command is what the container runs: the incremental build (when
// configured) chained before the run command, so every container boot
// rebuilds against its own warm caches.
func (p *Pool) command() string {
	if p.Cfg.Build != "" {
		return p.Cfg.Build + " && " + p.Cfg.RunCmd
	}
	return p.Cfg.RunCmd
}

// Ensure makes sure a live instance exists and is reachable: adopt a running
// hotlane container for this app if one exists (daemon restarts must not
// touch serving traffic), otherwise create the baseline from source.
func (p *Pool) Ensure() error {
	if err := p.ensurePristine(); err != nil {
		return err
	}
	// A held fork is a RUNNING container with this app's label, so it
	// would otherwise look like the newest live version and get adopted -
	// putting unpromoted, untested code in front of users on a restart.
	// The held marker survives the daemon, so reap them before adopting.
	p.reapHeldOnStart()
	// Prefer the version this daemon last put live. "Newest running
	// container" is only the live one after a PROMOTE; after a ROLLBACK
	// the newest running container is the version being rolled away
	// from, still inside its stop grace. Adopting that one succeeds and
	// then Docker finishes stopping it - leaving the proxy pointed at a
	// dead backend, permanently.
	if name := p.recordedLive(); name != "" {
		if err := p.adopt(name); err == nil {
			return nil
		}
		log.Printf("pool: recorded live %s is not adoptable, falling back to the newest running container", name)
	}
	running, err := docker.Running(p.Cfg.App)
	if err != nil {
		return err
	}
	if len(running) > 0 {
		// Newest running version: a daemon restart can land inside the
		// previous version's stop grace, when both old and new are
		// briefly running - the newest is the one promote made live.
		return p.adopt(running[0])
	}
	return p.createBaseline()
}

func (p *Pool) adopt(name string) error {
	addr, err := docker.HostAddr(name, p.Cfg.Port)
	if err != nil {
		return fmt.Errorf("adopting %s: %w", name, err)
	}
	version := 0
	if v, err := docker.Label(name, docker.LabelVersion); err == nil {
		version, _ = strconv.Atoi(v)
	}
	if err := waitReady(addr, 15*time.Second); err != nil {
		return fmt.Errorf("adopting %s: %w", name, err)
	}
	p.Live, p.Backend, p.Version = name, addr, version
	// Record the adoption: the fallback path (newest running container)
	// can pick a container the marker never named, and the live history
	// must list everything that served - the orphan reaper destroys
	// running containers it has never heard of.
	p.recordLive(name)
	// Versions are never reused, so the counter must clear every
	// container that exists - not just the adopted one. After a rollback
	// the live container is NOT the highest version (v4 is stopped in
	// the ring while v3 serves), and resuming at live+1 would collide
	// with v4's container name on the next push.
	p.next = version + 1
	if h := p.highestVersion(); h >= p.next {
		p.next = h + 1
	}
	// The persisted counter outlives every container, so a reaped fork
	// or a pruned ring entry cannot hand its number out twice.
	if n := p.recordedNext(); n > p.next {
		p.next = n
	}
	p.recordNext(p.next)
	log.Printf("pool: adopted %s (v%d) at %s, next version v%d", name, version, addr, p.next)
	return nil
}

// highestVersion is the largest version number any container for this
// app still carries, in any state.
func (p *Pool) highestVersion() int {
	infos, err := docker.All(p.Cfg.App)
	if err != nil {
		return 0
	}
	high := 0
	for _, in := range infos {
		if v := versionFromName(in.Name); v > high {
			high = v
		}
	}
	return high
}

func (p *Pool) createBaseline() error {
	version := 1
	name := containerName(p.Cfg.App, version)
	if docker.Exists(name) {
		// Stopped leftover from a previous run: baseline is rebuilt from
		// source anyway, so replace it rather than resurrect stale state.
		log.Printf("pool: removing stale container %s", name)
		if err := docker.Remove(name); err != nil {
			return err
		}
	}

	log.Printf("pool: creating baseline %s from %s", name, p.Cfg.Image)
	labels := map[string]string{
		docker.LabelApp:     p.Cfg.App,
		docker.LabelVersion: strconv.Itoa(version),
	}
	if err := docker.Create(name, p.Cfg.Image, p.Cfg.Workdir, p.Cfg.Port, p.command(), labels); err != nil {
		return err
	}
	if err := docker.CopyIn(name, p.Src, p.Cfg.Workdir); err != nil {
		docker.Remove(name)
		return err
	}
	if err := docker.Start(name); err != nil {
		docker.Remove(name)
		return err
	}

	addr, err := docker.HostAddr(name, p.Cfg.Port)
	if err != nil {
		return err
	}
	if err := waitReady(addr, 60*time.Second); err != nil {
		return fmt.Errorf("baseline %s: %w", name, err)
	}
	p.Live, p.Backend, p.Version = name, addr, version
	p.recordLive(name)
	p.next = version + 1
	p.recordNext(p.next)
	log.Printf("pool: baseline %s ready at %s", name, addr)
	return nil
}

// Fork snapshots the live instance, applies the submitted diff, and boots
// the result on its own loopback port. The diff is cumulative against the
// source tree as it was when the baseline was created (a working-tree
// `git diff HEAD` from a clean checkout matches that contract).
//
// A non-empty baseImage overrides the warm snapshot: the fork builds from
// that image instead of committing the live container. This is the drift
// recovery path - forking from the archivist's clean image resets the
// warm chain to a known-good state.
func (p *Pool) Fork(diff []byte, baseImage string) (*ForkResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Live == "" {
		return nil, fmt.Errorf("no live instance to fork")
	}
	start := time.Now()
	res := &ForkResult{Version: p.next}
	p.next++
	p.recordNext(p.next)
	name := containerName(p.Cfg.App, res.Version)
	res.Container = name

	// Snapshot: the fork's image is the live container's filesystem, warm
	// caches included - unless a clean base was requested.
	imageRef := p.imageRef(res.Version)
	committed := false
	if baseImage != "" {
		// The fork must own a per-version reference to its base. The
		// archivist rebuilds the :clean tag on every promote, and on
		// Docker's containerd image store (the default on fresh installs
		// since Docker 28) the untagged previous image gets garbage
		// collected - after which every `docker commit` of this container
		// fails with "content digest not found", permanently breaking
		// pushes after any drift recovery or auto-rebase. The version tag
		// pins the base for exactly as long as the version lives; prune
		// and Discard already remove it.
		if err := docker.TagImage(baseImage, imageRef); err != nil {
			return nil, fmt.Errorf("pinning clean base for v%d: %w", res.Version, err)
		}
		committed = true
		res.FromClean = true
		log.Printf("pool: forking v%d from clean image %s (drift recovery)", res.Version, baseImage)
	} else if err := docker.Commit(p.Live, imageRef); err != nil {
		return nil, err
	} else {
		committed = true
	}
	res.SnapshotMs = time.Since(start).Milliseconds()

	// Every failure below this point must take the snapshot image with
	// it. A rejected diff that leaves its image behind lets a loop of
	// malformed pushes fill the disk with full filesystem copies -
	// nothing else ever collects them (the ring only prunes promoted
	// versions).
	ok := false
	defer func() {
		if !ok && committed {
			docker.RemoveImage(imageRef)
		}
	}()

	// Patch: reset the shadow tree to pristine and apply the diff there,
	// host-side, so the container image never needs git installed.
	patchStart := time.Now()
	shadow, err := p.resetShadow()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(diff)) > 0 {
		apply := exec.Command("git", "apply", "--whitespace=nowarn")
		apply.Dir = shadow
		apply.Stdin = bytes.NewReader(diff)
		if out, err := apply.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("applying diff: %v: %s", err, out)
		}
	}
	res.PatchMs = time.Since(patchStart).Milliseconds()

	// Boot: same shape as the baseline, but from the snapshot image and
	// with the patched tree overlaid.
	bootStart := time.Now()
	labels := map[string]string{
		docker.LabelApp:     p.Cfg.App,
		docker.LabelVersion: strconv.Itoa(res.Version),
	}
	if err := docker.Create(name, imageRef, p.Cfg.Workdir, p.Cfg.Port, p.command(), labels); err != nil {
		return nil, err
	}
	if err := docker.CopyIn(name, shadow, p.Cfg.Workdir); err != nil {
		docker.Remove(name)
		return nil, err
	}
	if err := docker.Start(name); err != nil {
		docker.Remove(name)
		return nil, err
	}
	addr, err := docker.HostAddr(name, p.Cfg.Port)
	if err != nil {
		docker.Remove(name)
		return nil, err
	}
	if err := waitReady(addr, 60*time.Second); err != nil {
		docker.Remove(name)
		return nil, fmt.Errorf("fork %s: %w", name, err)
	}
	res.Backend = addr
	res.BootMs = time.Since(bootStart).Milliseconds()
	res.TotalMs = time.Since(start).Milliseconds()
	p.LastFork = res
	log.Printf("pool: fork %s ready at %s (snapshot %dms, patch %dms, boot %dms, total %dms)",
		name, addr, res.SnapshotMs, res.PatchMs, res.BootMs, res.TotalMs)
	ok = true // the image now belongs to the fork; Discard/prune own it
	return res, nil
}

// Promote makes a verified fork the live version. The previous live
// container is stopped in the background (it keeps its filesystem and
// becomes a ring entry) so the flip itself costs nothing; the ring is
// pruned to size afterwards.
func (p *Pool) Promote(res *ForkResult) (old string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old = p.Live
	p.Live, p.Backend, p.Version = res.Container, res.Backend, res.Version
	p.recordLive(res.Container)
	if old != "" && old != res.Container {
		p.stopAsync(old, p.prune)
	}
	log.Printf("pool: promoted %s (v%d), superseded %s", res.Container, res.Version, old)
	return old
}

// stopAsync stops a superseded container in the background so flips stay
// instant, and records the in-flight stop: anything that wants to bring
// that container back (rollback) must wait for the stop to land first, or
// the delayed SIGKILL murders the freshly re-promoted live version.
func (p *Pool) stopAsync(name string, after func()) {
	ch := make(chan struct{})
	p.stopping.Store(name, ch)
	go func() {
		if err := docker.Stop(name, 5); err != nil {
			log.Printf("pool: stopping superseded %s: %v", name, err)
		}
		p.stopping.Delete(name)
		close(ch)
		if after != nil {
			after()
		}
	}()
}

// waitStopped blocks until any in-flight stop of name has completed.
// The pending stop is hastened with a kill: the caller wants this
// container back right now and is about to restart it anyway, so waiting
// out a SIGTERM grace period buys nothing.
func (p *Pool) waitStopped(name string) {
	if ch, ok := p.stopping.Load(name); ok {
		docker.Kill(name)
		<-ch.(chan struct{})
	}
}

// RingEntry is one kept version.
type RingEntry struct {
	Version   int    `json:"version"`
	Container string `json:"container"`
	Status    string `json:"status"`
	Live      bool   `json:"live"`
}

// Ring lists all kept versions, newest first, live included.
func (p *Pool) Ring() []RingEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ring()
}

// ring is Ring without locking, for callers already holding p.mu.
func (p *Pool) ring() []RingEntry {
	infos, err := docker.All(p.Cfg.App)
	if err != nil {
		log.Printf("pool: listing ring: %v", err)
	}
	entries := make([]RingEntry, 0, len(infos))
	for _, in := range infos {
		v := versionFromName(in.Name)
		if v == 0 {
			continue
		}
		// A held fork is a running, labelled container, but it is NOT a
		// ring entry: it was never promoted and never served traffic.
		// Leaving it here let `rollback` flip live traffic onto
		// unverified code, and let prune force-remove a fork someone was
		// still testing.
		if _, held := p.held[v]; held {
			continue
		}
		entries = append(entries, RingEntry{
			Version:   v,
			Container: in.Name,
			Status:    in.Status,
			Live:      in.Name == p.Live,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Version > entries[j].Version })
	return entries
}

func versionFromName(name string) int {
	i := strings.LastIndex(name, "-v")
	if i < 0 {
		return 0
	}
	v, _ := strconv.Atoi(name[i+2:])
	return v
}

// prune removes the least-recently-live versions beyond the configured
// ring size, containers and snapshot images both (unbounded version
// stacking is how hosts run out of disk). Retention follows the live
// history, not version numbers: after a rollback the version that was
// just serving is lower numbered than the one it displaced, and pruning
// "lowest N" would destroy the only known-good rollback target while
// keeping the bad build.
func (p *Pool) prune() {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := make([]RingEntry, 0)
	for _, e := range p.ring() {
		if !e.Live {
			entries = append(entries, e)
		}
	}
	kept := 0
	for _, e := range pruneOrder(entries, p.liveHistory()) {
		if kept < p.Cfg.Ring {
			kept++
			continue
		}
		log.Printf("pool: pruning %s (ring=%d)", e.Container, p.Cfg.Ring)
		if err := docker.Remove(e.Container); err != nil {
			log.Printf("pool: pruning %s: %v", e.Container, err)
			continue
		}
		// Best effort: v1 has no snapshot image (baseline runs on cfg.Image).
		docker.RemoveImage(p.imageRef(e.Version))
	}
}

// pruneOrder ranks ring entries by how recently they were live, most
// recent first. Entries never recorded in the history (daemons upgraded
// mid-ring) sort after all recorded ones, newest version first - the
// pre-history behavior.
func pruneOrder(entries []RingEntry, history []int) []RingEntry {
	rank := make(map[int]int, len(history))
	for i, v := range history {
		rank[v] = i + 1 // 0 is reserved for "never recorded"
	}
	out := append([]RingEntry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].Version], rank[out[j].Version]
		if ri > 0 && rj > 0 {
			return ri < rj // both recorded: most recently live first
		}
		if ri != rj {
			return ri > 0 // recorded beats never-recorded
		}
		return out[i].Version > out[j].Version
	})
	return out
}

// RollbackResult describes a completed rollback.
type RollbackResult struct {
	Container string `json:"container"`
	Backend   string `json:"backend"`
	Version   int    `json:"version"`
	Restarted bool   `json:"restarted"`
	TotalMs   int64  `json:"total_ms"`
}

// Rollback flips traffic to a kept version; version 0 means the newest
// ring entry older than live. A stopped entry is started first (its boot
// command reruns against its own warm filesystem). The ready callback
// gates the flip: TCP-accept is not app readiness (docker's port proxy
// accepts before the app listens), so callers pass their real check -
// on failure the rollback aborts and the current version keeps serving.
func (p *Pool) Rollback(version int, ready func(container, backend string) error) (*RollbackResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	start := time.Now()

	var target *RingEntry
	for _, e := range p.ring() {
		e := e
		if version == 0 && !e.Live && e.Version < p.Version {
			target = &e
			break // ring is newest-first: first older entry is the previous version
		}
		if version != 0 && e.Version == version {
			target = &e
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("no ring entry to roll back to (want v%d)", version)
	}
	if target.Live {
		return nil, fmt.Errorf("v%d is already live", target.Version)
	}

	res := &RollbackResult{Container: target.Container, Version: target.Version}
	// If the target was just superseded its stop may still be in flight;
	// let it land so we restart cleanly instead of adopting a dying
	// container. Blocks the pool for at most the stop grace period.
	p.waitStopped(target.Container)
	if !docker.IsRunning(target.Container) {
		res.Restarted = true
		if err := docker.Start(target.Container); err != nil {
			return nil, err
		}
	}
	addr, err := docker.HostAddr(target.Container, p.Cfg.Port)
	if err != nil {
		return nil, err
	}
	if err := waitReady(addr, 30*time.Second); err != nil {
		return nil, fmt.Errorf("rollback %s: %w", target.Container, err)
	}
	if ready != nil {
		if err := ready(target.Container, addr); err != nil {
			return nil, fmt.Errorf("rollback %s: not ready: %w", target.Container, err)
		}
	}

	old := p.Live
	p.Live, p.Backend, p.Version = target.Container, addr, target.Version
	p.recordLive(target.Container)
	if old != "" {
		p.stopAsync(old, nil)
	}
	res.Backend = addr
	res.TotalMs = time.Since(start).Milliseconds()
	log.Printf("pool: rolled back to %s (v%d) at %s in %dms (restarted=%v)",
		target.Container, target.Version, addr, res.TotalMs, res.Restarted)
	return res, nil
}

// Discard destroys a failed fork (container and snapshot image - failed
// pushes must not stack images on disk) and returns its last log lines
// for the pusher.
func (p *Pool) Discard(res *ForkResult) string {
	logs := docker.Logs(res.Container, 50)
	if err := docker.Remove(res.Container); err != nil {
		log.Printf("pool: discarding %s: %v", res.Container, err)
	}
	if err := docker.RemoveImage(p.imageRef(res.Version)); err != nil {
		log.Printf("pool: discarding image %s: %v", p.imageRef(res.Version), err)
	}
	log.Printf("pool: discarded failed fork %s", res.Container)
	return logs
}

func (p *Pool) pristineDir() string { return filepath.Join(p.DataDir, "pristine") }

// ensurePristine snapshots the source tree the first time the daemon sees
// this app, and records the git commit that snapshot was taken at. Diffs
// are applied against this copy, so serve should be started from a clean
// checkout.
func (p *Pool) ensurePristine() error {
	commitFile := filepath.Join(p.DataDir, "baseline-commit")
	if _, err := os.Stat(p.pristineDir()); err == nil {
		if raw, err := os.ReadFile(commitFile); err == nil {
			p.BaselineCommit = strings.TrimSpace(string(raw))
		}
		return nil
	}
	if err := os.MkdirAll(p.DataDir, 0o755); err != nil {
		return err
	}
	if err := copyTree(p.Src, p.pristineDir()); err != nil {
		return err
	}
	if out, err := exec.Command("git", "-C", p.Src, "rev-parse", "HEAD").Output(); err == nil {
		p.BaselineCommit = strings.TrimSpace(string(out))
		if err := os.WriteFile(commitFile, []byte(p.BaselineCommit+"\n"), 0o644); err != nil {
			return err
		}
		log.Printf("pool: baseline commit %s", p.BaselineCommit[:12])
	}
	return nil
}

// HeldFork is a booted, verified fork that is NOT serving traffic: the
// caller pokes it (via the X-Hotlane-Fork header), then promotes or
// discards it. Its source tree is retained so a later promote archives
// the right code even if newer pushes reset the shadow in between.
type HeldFork struct {
	Result    *ForkResult
	ExpiresAt time.Time
	SrcDir    string
	// Token makes the fork's address unguessable. Held forks are reached
	// on the PUBLIC app listener (the whole point: poke them from a
	// browser or an agent without credentials), so a bare version number
	// would let anyone on the internet read unreleased code by counting
	// to fifty.
	Token string
	proxy *httputil.ReverseProxy
}

// Header is the value a caller must send as X-Hotlane-Fork to reach
// this fork.
func (h *HeldFork) Header() string {
	return fmt.Sprintf("%d-%s", h.Result.Version, h.Token)
}

// Hold registers a just-forked instance as held. Call under the same
// serialization as Fork, before any newer fork resets the shadow.
func (p *Pool) Hold(res *ForkResult, ttl time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = map[int]*HeldFork{}
	}
	if len(p.held) >= maxHeld {
		return fmt.Errorf("%d forks already held - promote or discard one first", maxHeld)
	}
	src := filepath.Join(p.DataDir, "held", fmt.Sprintf("v%d", res.Version))
	if err := os.RemoveAll(src); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		return err
	}
	if err := copyTree(p.ShadowDir(), src); err != nil {
		return err
	}
	if err := os.WriteFile(p.heldMarker(res.Version), nil, 0o644); err != nil {
		return err
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return fmt.Errorf("generating fork token: %w", err)
	}
	p.held[res.Version] = &HeldFork{
		Result:    res,
		ExpiresAt: time.Now().Add(ttl),
		SrcDir:    src,
		Token:     hex.EncodeToString(tok),
		proxy:     httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: res.Backend}),
	}
	log.Printf("pool: holding fork v%d at %s until %s", res.Version, res.Backend, p.held[res.Version].ExpiresAt.Format(time.RFC3339))
	return nil
}

// HeldProxy resolves an X-Hotlane-Fork header value ("<version>-<token>")
// to a held fork's reverse proxy, or nil. The token is compared in
// constant time: this runs on the unauthenticated public traffic path,
// so it is the only thing standing between a stranger and unreleased
// code.
func (p *Pool) HeldProxy(header string) http.Handler {
	version, token, ok := strings.Cut(strings.TrimSpace(header), "-")
	if !ok || token == "" {
		return nil
	}
	v, err := strconv.Atoi(version)
	if err != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.held[v]
	if !ok {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) != 1 {
		return nil
	}
	return h.proxy
}

// State is a consistent snapshot of the pool's mutable serving state.
// The fields below are written under p.mu by Fork/Promote/Rollback while
// HTTP handlers read them concurrently, so readers outside this package
// must go through State - reading the fields directly is a data race,
// and a torn string read hands back garbage or crashes the daemon.
type State struct {
	Live     string
	Backend  string
	Version  int
	LastFork *ForkResult
	Baseline string
}

// State returns the current serving state. Never call it from a method
// that already holds p.mu.
func (p *Pool) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return State{
		Live:     p.Live,
		Backend:  p.Backend,
		Version:  p.Version,
		LastFork: p.LastFork,
		Baseline: p.BaselineCommit,
	}
}

// nextMarker persists the version counter. Deriving it from surviving
// containers is not enough: reaping a held fork, discarding a rejected
// push, or pruning the ring all lower the maximum, and the counter then
// hands out a number that was already used. Versions are an audit
// trail - the archivist tags registry images <ref>:vN - so reuse
// silently overwrites the image for a different build.
func (p *Pool) nextMarker() string { return filepath.Join(p.DataDir, "next-version") }

func (p *Pool) recordNext(v int) {
	if err := os.WriteFile(p.nextMarker(), []byte(strconv.Itoa(v)), 0o644); err != nil {
		log.Printf("pool: recording next version: %v", err)
	}
}

func (p *Pool) recordedNext() int {
	b, err := os.ReadFile(p.nextMarker())
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return v
}

// liveMarker records which container this daemon last flipped traffic
// to, so a restart resumes the same one rather than guessing from
// container state.
func (p *Pool) liveMarker() string { return filepath.Join(p.DataDir, "live-container") }

// recordLive persists the current live container. Called on every flip.
func (p *Pool) recordLive(name string) {
	if err := os.WriteFile(p.liveMarker(), []byte(name), 0o644); err != nil {
		log.Printf("pool: recording live container: %v", err)
	}
	if v := versionFromName(name); v > 0 {
		p.recordHistory(v)
	}
}

// historyMarker persists the order versions were live in, most recent
// first, one version per line. Prune retains by this order, not by
// version number: after a rollback the known-good version is LOWER
// numbered than the bad one it displaced, and retaining "highest N"
// would prune exactly the version worth keeping.
func (p *Pool) historyMarker() string { return filepath.Join(p.DataDir, "live-history") }

// maxHistory caps the history file; anything past it is long pruned.
const maxHistory = 100

func (p *Pool) recordHistory(version int) {
	hist := append([]int{version}, p.liveHistory()...)
	seen := make(map[int]bool, len(hist))
	var b strings.Builder
	kept := 0
	for _, v := range hist {
		if seen[v] || kept >= maxHistory {
			continue
		}
		seen[v] = true
		kept++
		fmt.Fprintf(&b, "%d\n", v)
	}
	if err := os.WriteFile(p.historyMarker(), []byte(b.String()), 0o644); err != nil {
		log.Printf("pool: recording live history: %v", err)
	}
}

// liveHistory returns the recorded live order, most recent first.
func (p *Pool) liveHistory() []int {
	b, err := os.ReadFile(p.historyMarker())
	if err != nil {
		return nil
	}
	var hist []int
	for _, line := range strings.Split(string(b), "\n") {
		if v, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && v > 0 {
			hist = append(hist, v)
		}
	}
	return hist
}

// recordedLive returns the last recorded live container if it still
// exists, starting it if it was stopped (a rollback target legitimately
// sits stopped in the ring). Empty when there is nothing usable.
func (p *Pool) recordedLive() string {
	b, err := os.ReadFile(p.liveMarker())
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(b))
	if name == "" || !docker.Exists(name) {
		return ""
	}
	if !docker.IsRunning(name) {
		if err := docker.Start(name); err != nil {
			return ""
		}
	}
	return name
}

// heldMarker is the on-disk record that a version is held and has never
// served traffic. Written when the fork is held, removed BEFORE promote
// flips traffic to it - so a crash mid-promote leaves no marker and the
// (verified, now-live) container is adopted normally, while a crash
// while merely holding leaves the marker and the fork gets reaped. A
// crash in the gap - marker gone, flip not yet made - leaves a running
// fork with neither marker nor history entry; the orphan reaper in
// reapHeldOnStart collects those.
func (p *Pool) heldMarker(version int) string {
	return filepath.Join(p.DataDir, "held", fmt.Sprintf("v%d.held", version))
}

// reapHeldOnStart destroys forks that were held when the daemon stopped.
// They are unreachable anyway - their access tokens lived only in memory
// - so the only thing they could still do is be mistaken for live.
func (p *Pool) reapHeldOnStart() {
	// A crash during a drift check leaves hotlane-<app>-drift running.
	// It carries its own label so the pool never adopts it, but it is a
	// full instance of the app - background workers, cron, queue
	// consumers and all - running against shared state with nobody
	// watching. Nothing else collects it until the next check, up to six
	// hours later.
	if drift := "hotlane-" + p.Cfg.App + "-drift"; docker.Exists(drift) {
		log.Printf("pool: removing orphaned drift container %s (left by a crash mid-check)", drift)
		docker.Remove(drift)
	}

	markers, _ := filepath.Glob(filepath.Join(p.DataDir, "held", "v*.held"))
	for _, m := range markers {
		var v int
		if _, err := fmt.Sscanf(filepath.Base(m), "v%d.held", &v); err != nil {
			continue
		}
		name := containerName(p.Cfg.App, v)
		if docker.Exists(name) {
			log.Printf("pool: reaping fork v%d held when the daemon stopped (never promoted, never served traffic)", v)
			docker.Remove(name)
			docker.RemoveImage(p.imageRef(v))
		}
		os.Remove(m)
		os.RemoveAll(filepath.Join(p.DataDir, "held", fmt.Sprintf("v%d", v)))
	}

	// A crash between held-marker removal and the traffic flip
	// (PromoteHeld's window) leaves the fork RUNNING with no marker:
	// not held, not live, never served - the marker loop above misses
	// it and it pollutes the ring forever. A crash during a push's
	// verify leaves the same signature. The live history lists every
	// version that ever served, so any running container absent from
	// it is such an orphan. An empty history means this daemon
	// predates the record - skip rather than guess.
	hist := p.liveHistory()
	if len(hist) == 0 {
		return
	}
	served := make(map[int]bool, len(hist))
	for _, v := range hist {
		served[v] = true
	}
	// Belt and braces: never touch the recorded live container, even if
	// the history file was lost or hand-edited.
	liveName := ""
	if b, err := os.ReadFile(p.liveMarker()); err == nil {
		liveName = strings.TrimSpace(string(b))
	}
	running, _ := docker.Running(p.Cfg.App)
	for _, name := range running {
		v := versionFromName(name)
		if v == 0 || served[v] || name == liveName {
			continue
		}
		log.Printf("pool: reaping orphaned fork %s (running but never served traffic - left by a crash)", name)
		docker.Remove(name)
		docker.RemoveImage(p.imageRef(v))
		os.RemoveAll(filepath.Join(p.DataDir, "held", fmt.Sprintf("v%d", v)))
	}
}

// Held returns a held fork by version, or nil.
func (p *Pool) Held(version int) *HeldFork {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held[version]
}

// HeldList summarizes held forks for status.
func (p *Pool) HeldList() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.held))
	for v, h := range p.held {
		out = append(out, map[string]any{
			"version":    v,
			"backend":    h.Result.Backend,
			"expires_at": h.ExpiresAt.UTC().Format(time.RFC3339),
			// The status endpoint is authenticated, so returning the
			// header here is how an agent that lost the test output
			// recovers its fork address.
			"header": h.Header(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["version"].(int) < out[j]["version"].(int) })
	return out
}

// PromoteHeld flips traffic to a held fork after the ready check passes.
// Returns the retained source dir; the caller archives from it and then
// removes it.
func (p *Pool) PromoteHeld(version int, ready func(container, backend string) error) (string, *ForkResult, error) {
	p.mu.Lock()
	h, ok := p.held[version]
	p.mu.Unlock()
	if !ok {
		return "", nil, fmt.Errorf("no held fork v%d (expired, promoted, or discarded?)", version)
	}
	if ready != nil {
		if err := ready(h.Result.Container, h.Result.Backend); err != nil {
			return "", nil, fmt.Errorf("held fork v%d not ready: %w", version, err)
		}
	}
	// Clear the marker before the flip: from here on this container may
	// be serving traffic, so a crash must leave it adoptable.
	os.Remove(p.heldMarker(version))
	p.Promote(h.Result)
	p.mu.Lock()
	delete(p.held, version)
	p.mu.Unlock()
	return h.SrcDir, h.Result, nil
}

// DiscardHeld destroys a held fork.
func (p *Pool) DiscardHeld(version int) error {
	p.mu.Lock()
	h, ok := p.held[version]
	if ok {
		delete(p.held, version)
	}
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("no held fork v%d", version)
	}
	p.Discard(h.Result)
	os.Remove(p.heldMarker(version))
	os.RemoveAll(h.SrcDir)
	return nil
}

// StartHeldReaper discards held forks past their TTL.
// StartHeldReaper expires held forks. It takes the caller's push lock
// before discarding: PromoteHeld runs its verify gate with p.mu
// RELEASED, so an unsynchronized reaper could rm -f a fork mid-promote
// and leave the proxy pointed at a container that no longer exists.
func (p *Pool) StartHeldReaper(pushLock sync.Locker) {
	go func() {
		for range time.Tick(30 * time.Second) {
			p.mu.Lock()
			var expired []int
			for v, h := range p.held {
				if time.Now().After(h.ExpiresAt) {
					expired = append(expired, v)
				}
			}
			p.mu.Unlock()
			for _, v := range expired {
				pushLock.Lock()
				// Re-check under the push lock: a promote may have
				// claimed it while we waited.
				p.mu.Lock()
				_, still := p.held[v]
				p.mu.Unlock()
				if still {
					log.Printf("pool: held fork v%d expired, discarding", v)
					p.DiscardHeld(v)
				}
				pushLock.Unlock()
			}
		}
	}()
}

// ShadowDir is where the last fork's patched source tree lives; the
// archivist snapshots it at promote time.
func (p *Pool) ShadowDir() string { return filepath.Join(p.DataDir, "shadow") }

// resetShadow rebuilds the shadow tree from pristine and returns its path.
func (p *Pool) resetShadow() (string, error) {
	shadow := p.ShadowDir()
	if err := os.RemoveAll(shadow); err != nil {
		return "", err
	}
	return shadow, copyTree(p.pristineDir(), shadow)
}

func copyTree(src, dst string) error {
	if out, err := exec.Command("cp", "-R", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("copying %s -> %s: %v: %s", src, dst, err, out)
	}
	return nil
}

// waitReady polls until the backend accepts TCP connections.
func waitReady(addr string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("not accepting connections on %s after %s", addr, budget)
}
