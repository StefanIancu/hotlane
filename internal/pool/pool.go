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
	"strconv"
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

	mu   sync.Mutex
	next int // next version number to assign

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
	imageRef := fmt.Sprintf("hotlane-%s:v%d", p.Cfg.App, res.Version)
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
