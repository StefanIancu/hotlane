// Package pool manages the warm pool: the live, running instance of an app
// that forks are taken from and traffic is routed to. Ensure (M1) adopts or
// creates the live instance; Fork (M2) turns a git delta into a booted,
// side-lined instance carrying the live version's warm state.
package pool

import (
	"bytes"
	"fmt"
	"log"
	"net"
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
}

// ForkResult describes a booted fork and where the time went. The phase
// timings are the product being measured - keep them honest.
type ForkResult struct {
	Container  string `json:"container"`
	Backend    string `json:"backend"`
	Version    int    `json:"version"`
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
	running, err := docker.Running(p.Cfg.App)
	if err != nil {
		return err
	}
	if len(running) > 0 {
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
	p.next = version + 1
	log.Printf("pool: adopted %s (v%d) at %s", name, version, addr)
	return nil
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
	p.next = version + 1
	log.Printf("pool: baseline %s ready at %s", name, addr)
	return nil
}

// Fork snapshots the live instance, applies the submitted diff, and boots
// the result on its own loopback port. The diff is cumulative against the
// source tree as it was when the baseline was created (a working-tree
// `git diff HEAD` from a clean checkout matches that contract).
func (p *Pool) Fork(diff []byte) (*ForkResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Live == "" {
		return nil, fmt.Errorf("no live instance to fork")
	}
	start := time.Now()
	res := &ForkResult{Version: p.next}
	p.next++
	name := containerName(p.Cfg.App, res.Version)
	res.Container = name

	// Snapshot: the fork's image is the live container's filesystem, warm
	// caches included.
	imageRef := p.imageRef(res.Version)
	if err := docker.Commit(p.Live, imageRef); err != nil {
		return nil, err
	}
	res.SnapshotMs = time.Since(start).Milliseconds()

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

// prune removes the oldest non-live versions beyond the configured ring
// size, containers and snapshot images both (unbounded version stacking is
// how hosts run out of disk).
func (p *Pool) prune() {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := 0
	for _, e := range p.ring() {
		if e.Live {
			continue
		}
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
// command reruns against its own warm filesystem).
func (p *Pool) Rollback(version int) (*RollbackResult, error) {
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

	old := p.Live
	p.Live, p.Backend, p.Version = target.Container, addr, target.Version
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
// this app. Diffs are applied against this copy, so serve should be started
// from a clean checkout.
func (p *Pool) ensurePristine() error {
	if _, err := os.Stat(p.pristineDir()); err == nil {
		return nil
	}
	if err := os.MkdirAll(p.DataDir, 0o755); err != nil {
		return err
	}
	return copyTree(p.Src, p.pristineDir())
}

// resetShadow rebuilds the shadow tree from pristine and returns its path.
func (p *Pool) resetShadow() (string, error) {
	shadow := filepath.Join(p.DataDir, "shadow")
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
