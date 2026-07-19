// hotlane: validation-first deployment. See README.md for the model.
//
// Subcommands:
//
//	serve     run the daemon on this host (M1: warm pool + traffic proxy)
//	push      send the current git delta to the daemon (M2)
//	status    show live version, ring, and last verify results (M4)
//	rollback  flip the proxy to a previous ring entry (M4)
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/StefanIancu/hotlane/internal/archive"
	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/detect"
	"github.com/StefanIancu/hotlane/internal/docker"
	"github.com/StefanIancu/hotlane/internal/notify"
	"github.com/StefanIancu/hotlane/internal/pool"
	"github.com/StefanIancu/hotlane/internal/proxy"
	"github.com/StefanIancu/hotlane/internal/verify"
)

// pushResponse is the daemon's answer to a push: the fork, the verify
// verdict, and (on failure) the fork's dying words.
type pushResponse struct {
	*pool.ForkResult
	Verify   []verify.Result `json:"verify"`
	VerifyMs int64           `json:"verify_ms"`
	Promoted bool            `json:"promoted"`
	Logs     string          `json:"logs,omitempty"`
}

// version is stamped by goreleaser at release time (-X main.version=...).
var version = "0.0.1-dev"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "push":
		cmdPush(os.Args[2:])
	case "test":
		cmdTest(os.Args[2:])
	case "promote":
		cmdPromote(os.Args[2:])
	case "discard":
		cmdDiscard(os.Args[2:])
	case "logs":
		cmdLogs(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "rollback":
		cmdRollback(os.Args[2:])
	case "drift":
		cmdDrift(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("hotlane", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: hotlane <command>

commands:
  init      detect the app in the current directory and write hotlane.yml
  serve     run the daemon on this host
  push      send the current git delta to the daemon
  test      like push, but HOLD the verified fork instead of promoting:
            poke it via the X-Hotlane-Fork header, then promote/discard
  promote   flip traffic to a held fork (hotlane promote <version>)
  discard   destroy a held fork (hotlane discard <version>)
  status    show live version, ring, and last verify results
  rollback  flip the proxy to a previous version
  logs      tail the live version's output
  drift     cold-boot the clean image and diff its behavior against live
  mcp       serve hotlane as MCP tools over stdio (for AI agents)
  version   print version

most commands accept -json for machine-readable output

environment:
  HOTLANE_DAEMON  default daemon URL for client commands (default http://127.0.0.1:7433)
  HOTLANE_TOKEN   bearer token: required by clients when serve runs with -token`)
}

// daemonDefault is the client-side daemon URL: flag > env > localhost.
func daemonDefault() string {
	if v := os.Getenv("HOTLANE_DAEMON"); v != "" {
		return v
	}
	return "http://127.0.0.1:7433"
}

// apiRequest performs a daemon API call, attaching the bearer token from
// HOTLANE_TOKEN when set.
func apiRequest(method, url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if tok := os.Getenv("HOTLANE_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return http.DefaultClient.Do(req)
}

// cmdInit writes a starter hotlane.yml based on what the repo looks like.
func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing hotlane.yml")
	fs.Parse(args)

	if _, err := os.Stat("hotlane.yml"); err == nil && !*force {
		log.Fatal("hotlane init: hotlane.yml already exists (use -force to overwrite)")
	}
	g := detect.Detect(".")
	if err := os.WriteFile("hotlane.yml", []byte(g.YAML()), 0o644); err != nil {
		log.Fatalf("hotlane init: %v", err)
	}
	fmt.Printf("detected %s\nwrote hotlane.yml (app=%s image=%s port=%d)\n", g.Framework, g.App, g.Image, g.Port)
	fmt.Println("review it - especially the verify hooks - then run: hotlane serve")
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "hotlane.yml", "path to hotlane.yml")
	apiAddr := fs.String("addr", ":7433", "daemon API listen address")
	proxyAddr := fs.String("proxy", ":7480", "app traffic listen address")
	token := fs.String("token", os.Getenv("HOTLANE_TOKEN"), "bearer token required on the API (empty = open; keep the API loopback-only then)")
	tlsDomain := fs.String("tls-domain", "", "serve the API over HTTPS with an auto-provisioned Let's Encrypt certificate for this domain (API defaults to :443; requires -token)")
	fs.Parse(args)

	if *tlsDomain != "" && *token == "" {
		log.Fatal("hotlane: -tls-domain exposes the API publicly; a -token (or HOTLANE_TOKEN) is required with it")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("hotlane: %v", err)
	}
	src, err := filepath.Abs(filepath.Dir(*cfgPath))
	if err != nil {
		log.Fatalf("hotlane: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("hotlane: %v", err)
	}
	p := &pool.Pool{Cfg: cfg, Src: src, DataDir: filepath.Join(home, ".hotlane", cfg.App)}
	if err := p.Ensure(); err != nil {
		log.Fatalf("hotlane: warm pool: %v", err)
	}

	front := proxy.New()
	front.Set(p.Backend)
	p.StartHeldReaper()

	// App traffic handler: live by default; the X-Hotlane-Fork header
	// routes to a held (test-mode) fork. Unknown fork = explicit error,
	// never a silent fall-through to live - an agent must not believe it
	// tested a fork when it actually hit production.
	appTraffic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fv := r.Header.Get("X-Hotlane-Fork"); fv != "" {
			n, err := strconv.Atoi(fv)
			if err == nil {
				if h := p.HeldProxy(n); h != nil {
					h.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, "hotlane: no held fork "+fv+" (expired, promoted, or discarded?)", http.StatusMisdirectedRequest)
			return
		}
		front.ServeHTTP(w, r)
	})

	notif := &notify.Notifier{URL: cfg.Notify, App: cfg.App}
	arch := archive.New(cfg, p.DataDir, notif)
	// First boot: snapshot the checkout so a clean image exists from day
	// one. Restarts keep the existing snapshot - it holds the last
	// promoted source, while the checkout may be stale (pushes deliver
	// diffs and never touch it); overwriting it regressed the clean
	// image and false-positived every post-restart drift check.
	if !arch.HasSnapshot() {
		if err := arch.Snapshot(src); err != nil {
			log.Printf("hotlane: archive snapshot: %v", err)
		}
	}
	go arch.Archive(p.Version, p.Backend)
	go func() {
		for range time.Tick(6 * time.Hour) {
			arch.DriftCheck(p.Backend)
		}
	}()

	mux := http.NewServeMux()
	// handle registers every API route twice: bare (the private API port)
	// and under /-/ (the shared TLS listener, where the app owns every
	// other path). /-/ is the GitLab-style instance-route prefix - obscure
	// enough that user apps won't collide with it.
	handle := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /-"+path, h)
	}
	handle("GET /v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"service": "hotlane",
			"version": version,
			"docs":    "https://hotlane.dev/llms-full.txt",
			"routes": []string{
				"GET  /-/healthz              liveness (no auth)",
				"GET  /-/v1/status            live version, ring, held forks, archive/drift, baseline_commit",
				"POST /-/v1/push              body=raw git diff -> fork, verify, promote; 422 on rejection",
				"POST /-/v1/test              body=raw git diff -> fork, verify, HOLD; returns X-Hotlane-Fork header",
				"POST /-/v1/promote           {version} -> flip traffic to a held fork",
				"POST /-/v1/discard           {version} -> destroy a held fork",
				"POST /-/v1/rollback          {version?} -> flip to a kept version",
				"POST /-/v1/drift-check       cold-boot clean image, diff behavior vs live",
				"GET  /-/v1/logs?tail=N       live container output",
			},
		})
	})
	handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok app=%s version=%s\n", cfg.App, version)
	})
	handle("GET /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		tail := 100
		if t := r.URL.Query().Get("tail"); t != "" {
			if n, err := strconv.Atoi(t); err == nil && n > 0 && n <= 10000 {
				tail = n
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, docker.Logs(p.Live, tail))
	})
	handle("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"app":             cfg.App,
			"live":            p.Live,
			"version":         p.Version,
			"backend":         front.Target(),
			"baseline_commit": p.BaselineCommit,
			"last_fork":       p.LastFork,
			"ring":            p.Ring(),
			"archive":         arch.Status(),
		})
	})
	handle("POST /v1/drift-check", func(w http.ResponseWriter, r *http.Request) {
		st := arch.DriftCheck(p.Backend)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	handle("POST /v1/rollback", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Version int `json:"version"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		ready := func(container, backend string) error {
			results, ok := verify.Run(cfg, container, backend)
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
		res, err := p.Rollback(req.Version, ready)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		front.Set(res.Backend)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	// Deep commit chains eventually hit Docker's overlayfs layer limit
	// (~125): every promoted fork adds one layer. Past rebaseDepth, forks
	// rebase onto the archivist's clean image, resetting the chain - the
	// same mechanics as drift recovery. Env-tunable for tests.
	rebaseDepth := 40
	if v, err := strconv.Atoi(os.Getenv("HOTLANE_REBASE_DEPTH")); err == nil && v > 0 {
		rebaseDepth = v
	}

	holdTTL := 15 * time.Minute
	if v, err := time.ParseDuration(os.Getenv("HOTLANE_HOLD_TTL")); err == nil && v > 0 {
		holdTTL = v
	}

	// Pushes are serialized: forks share the shadow tree and the version
	// counter, and one verified promote at a time is the contract.
	var pushMu sync.Mutex
	handle("POST /v1/push", func(w http.ResponseWriter, r *http.Request) {
		diff, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pushMu.Lock()
		defer pushMu.Unlock()

		// Fork from the clean image instead of the warm chain when the
		// chain is untrustworthy (drift) or too deep (layer limit).
		base := ""
		if arch.Drifted() {
			base = arch.CleanImage()
			log.Printf("push: forking from clean (drift recovery)")
		} else if d, err := docker.LayerDepth(p.Live); err == nil && d >= rebaseDepth && docker.ImageExists(arch.CleanImage()) {
			base = arch.CleanImage()
			log.Printf("push: forking from clean (layer rebase at depth %d)", d)
		}
		res, err := p.Fork(diff, base)
		if err != nil {
			log.Printf("push: %v", err)
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		out := pushResponse{ForkResult: res}

		vStart := time.Now()
		out.Verify, out.Promoted = verify.Run(cfg, res.Container, res.Backend)
		out.VerifyMs = time.Since(vStart).Milliseconds()
		out.TotalMs += out.VerifyMs

		w.Header().Set("Content-Type", "application/json")
		if !out.Promoted {
			out.Logs = p.Discard(res)
			for _, v := range out.Verify {
				if !v.OK {
					notif.Send(notify.EventPushRejected,
						fmt.Sprintf("v%d: %s - %s", res.Version, v.Hook, v.Detail))
					break
				}
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(out)
			return
		}
		p.Promote(res)
		front.Set(res.Backend)
		if err := arch.Snapshot(p.ShadowDir()); err != nil {
			log.Printf("push: archive snapshot: %v", err)
		} else {
			go arch.Archive(res.Version, res.Backend)
		}
		json.NewEncoder(w).Encode(out)
	})

	// test: fork + verify, then HOLD instead of promote. The caller pokes
	// the fork through the X-Hotlane-Fork header, then promotes/discards.
	handle("POST /v1/test", func(w http.ResponseWriter, r *http.Request) {
		diff, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pushMu.Lock()
		defer pushMu.Unlock()

		base := ""
		if arch.Drifted() {
			base = arch.CleanImage()
		} else if d, err := docker.LayerDepth(p.Live); err == nil && d >= rebaseDepth && docker.ImageExists(arch.CleanImage()) {
			base = arch.CleanImage()
		}
		res, err := p.Fork(diff, base)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		out := pushResponse{ForkResult: res}
		vStart := time.Now()
		var ok bool
		out.Verify, ok = verify.Run(cfg, res.Container, res.Backend)
		out.VerifyMs = time.Since(vStart).Milliseconds()
		out.TotalMs += out.VerifyMs
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			out.Logs = p.Discard(res)
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(out)
			return
		}
		if err := p.Hold(res, holdTTL); err != nil {
			p.Discard(res)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"fork":       out,
			"held":       true,
			"expires_in": holdTTL.String(),
			"header":     fmt.Sprintf("X-Hotlane-Fork: %d", res.Version),
		})
	})
	handle("POST /v1/promote", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Version int `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ready := func(container, backend string) error {
			results, ok := verify.Run(cfg, container, backend)
			if ok {
				return nil
			}
			for _, rr := range results {
				if !rr.OK {
					return fmt.Errorf("%s - %s", rr.Hook, rr.Detail)
				}
			}
			return fmt.Errorf("verify failed")
		}
		pushMu.Lock()
		defer pushMu.Unlock()
		srcDir, res, err := p.PromoteHeld(req.Version, ready)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		front.Set(res.Backend)
		if err := arch.Snapshot(srcDir); err != nil {
			log.Printf("promote: archive snapshot: %v", err)
		} else {
			go arch.Archive(res.Version, res.Backend)
		}
		os.RemoveAll(srcDir)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	handle("POST /v1/discard", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Version int `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pushMu.Lock()
		defer pushMu.Unlock()
		if err := p.DiscardHeld(req.Version); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Auth wraps the whole API except the liveness probe. The app-traffic
	// proxy is intentionally untouched - it serves the public app.
	var api http.Handler = mux
	if *token != "" {
		want := []byte("Bearer " + *token)
		api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("Authorization"))
			healthz := r.URL.Path == "/healthz" || r.URL.Path == "/-/healthz"
			if !healthz && subtle.ConstantTimeCompare(got, want) != 1 {
				http.Error(w, "unauthorized (set HOTLANE_TOKEN)", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		})
	} else {
		log.Printf("hotlane: API is UNAUTHENTICATED - fine on loopback, set -token before exposing it")
	}

	go func() {
		log.Printf("hotlane %s: app traffic on %s -> %s", version, *proxyAddr, front.Target())
		log.Fatal(http.ListenAndServe(*proxyAddr, appTraffic))
	}()

	if *tlsDomain != "" {
		// Shared HTTPS listener on :443: the APP owns https://domain/ (a
		// browser typing the domain gets the app, with TLS, zero config),
		// while the daemon API lives under the /-/ prefix. The private
		// API listener keeps running unchanged. Certificates come from
		// Let's Encrypt via the TLS-ALPN challenge.
		mgr := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(*tlsDomain),
			Cache:      autocert.DirCache(filepath.Join(home, ".hotlane", "autocert")),
		}
		shared := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/-" || strings.HasPrefix(r.URL.Path, "/-/") {
				api.ServeHTTP(w, r)
				return
			}
			appTraffic.ServeHTTP(w, r)
		})
		srv := &http.Server{Addr: ":443", Handler: shared, TLSConfig: mgr.TLSConfig()}
		go func() {
			// Best-effort http->https redirect; a busy :80 is not fatal.
			redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+*tlsDomain+r.URL.RequestURI(), http.StatusMovedPermanently)
			})
			if err := http.ListenAndServe(":80", redirect); err != nil {
				log.Printf("hotlane: :80 redirect listener: %v", err)
			}
		}()
		go func() {
			log.Printf("hotlane %s: app on https://%s/ + API on https://%s/-/ (ring=%d)", version, *tlsDomain, *tlsDomain, cfg.Ring)
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()
	}
	log.Printf("hotlane %s: API on %s (app=%q ring=%d)", version, *apiAddr, cfg.App, cfg.Ring)
	log.Fatal(http.ListenAndServe(*apiAddr, api))
}

// cmdLogs tails the live version's output through the daemon.
func cmdLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	tail := fs.Int("n", 100, "number of log lines")
	fs.Parse(args)

	resp, err := apiRequest("GET", fmt.Sprintf("%s/-/v1/logs?tail=%d", *daemon, *tail), "", nil)
	if err != nil {
		log.Fatalf("hotlane logs: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("hotlane logs: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	os.Stdout.Write(body)
}

// computeDiffE builds the delta to send: from the daemon's baseline
// commit (or an explicit ref) to the working tree.
func computeDiffE(daemon, from string) ([]byte, error) {
	base := from
	if base == "" {
		b, err := daemonBaselineE(daemon)
		if err != nil {
			return nil, err
		}
		base = b
	}
	diffArgs := []string{"diff", "HEAD", "--relative"}
	if base != "" {
		if exec.Command("git", "cat-file", "-e", base+"^{commit}").Run() != nil {
			return nil, fmt.Errorf("baseline commit %.12s is not in this clone - CI checkouts need history: use fetch-depth: 0 (or git fetch --unshallow) so the diff base exists", base)
		}
		diffArgs = []string{"diff", base, "--relative"}
	}
	diffCmd := exec.Command("git", diffArgs...)
	diffCmd.Stderr = os.Stderr
	diff, err := diffCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	return diff, nil
}

// computeDiff is computeDiffE with CLI ergonomics (fatal on error).
func computeDiff(daemon, from string) []byte {
	diff, err := computeDiffE(daemon, from)
	if err != nil {
		log.Fatalf("hotlane: %v", err)
	}
	if len(bytes.TrimSpace(diff)) == 0 {
		log.Printf("hotlane: no changes vs the daemon's baseline; forking current state anyway")
	}
	return diff
}

// cmdPush sends the delta to the daemon. Two modes:
//
//   - dirty worktree (the local dev loop): git diff HEAD --relative, i.e.
//     your uncommitted changes. New files need `git add -N` to appear.
//   - clean worktree (the CI loop): diff from the daemon's baseline commit
//     to HEAD, so a fresh checkout of a newer commit deploys that commit.
//     Override the base with -from.
func cmdPush(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	from := fs.String("from", "", "git ref to diff from (default: dirty worktree diffs HEAD, clean worktree diffs the daemon's baseline commit)")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	diff := computeDiff(*daemon, *from)

	resp, err := apiRequest("POST", *daemon+"/-/v1/push", "text/x-diff", bytes.NewReader(diff))
	if err != nil {
		log.Fatalf("hotlane push: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Println(string(bytes.TrimSpace(body)))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	var res pushResponse
	if err := json.Unmarshal(body, &res); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			log.Fatalf("hotlane push: daemon: %s: %s\nhint: a 404 from a -tls-domain daemon usually means this CLI is older than the daemon and hit the app instead of the API (the API moved under /-/ in v0.2.0) - upgrade: curl -fsSL https://hotlane.dev/install.sh | sh", resp.Status, bytes.TrimSpace(body))
		}
		log.Fatalf("hotlane push: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	fmt.Printf("fork %s (v%d): snapshot %dms | patch %dms | boot %dms | verify %dms\n",
		res.Container, res.Version, res.SnapshotMs, res.PatchMs, res.BootMs, res.VerifyMs)
	if res.FromClean {
		fmt.Println("  (rebased from the clean image)")
	}
	for _, v := range res.Verify {
		mark := "ok  "
		if !v.OK {
			mark = "FAIL"
		}
		fmt.Printf("  %s %s (%dms)", mark, v.Hook, v.Ms)
		if v.Detail != "" {
			fmt.Printf(" - %s", v.Detail)
		}
		fmt.Println()
	}
	if !res.Promoted {
		if res.Logs != "" {
			fmt.Printf("--- fork logs ---\n%s\n", res.Logs)
		}
		log.Fatalf("push REJECTED after %dms: fork destroyed, live version untouched", res.TotalMs)
	}
	fmt.Printf("PROMOTED v%d live in %dms\n", res.Version, res.TotalMs)
}

// cmdTest forks + verifies like push, but holds the fork for inspection
// instead of promoting it.
func cmdTest(args []string) {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	from := fs.String("from", "", "git ref to diff from (default: the daemon's baseline commit)")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	diff := computeDiff(*daemon, *from)
	resp, err := apiRequest("POST", *daemon+"/-/v1/test", "text/x-diff", bytes.NewReader(diff))
	if err != nil {
		log.Fatalf("hotlane test: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Println(string(bytes.TrimSpace(body)))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	var out struct {
		Fork      pushResponse `json:"fork"`
		Held      bool         `json:"held"`
		ExpiresIn string       `json:"expires_in"`
		Header    string       `json:"header"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !out.Held {
		var rej pushResponse
		if json.Unmarshal(body, &rej) == nil && len(rej.Verify) > 0 {
			for _, v := range rej.Verify {
				mark := "ok  "
				if !v.OK {
					mark = "FAIL"
				}
				fmt.Printf("  %s %s (%dms)\n", mark, v.Hook, v.Ms)
			}
			if rej.Logs != "" {
				fmt.Printf("--- fork logs ---\n%s\n", rej.Logs)
			}
			log.Fatalf("test REJECTED: fork destroyed, live version untouched")
		}
		log.Fatalf("hotlane test: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	r := out.Fork
	fmt.Printf("fork %s (v%d): snapshot %dms | patch %dms | boot %dms | verify %dms\n",
		r.Container, r.Version, r.SnapshotMs, r.PatchMs, r.BootMs, r.VerifyMs)
	fmt.Printf("HELD v%d for %s - live traffic untouched\n", r.Version, out.ExpiresIn)
	fmt.Printf("  poke it:    curl -H %q <your-app-url>/...\n", out.Header)
	fmt.Printf("  ship it:    hotlane promote %d\n", r.Version)
	fmt.Printf("  drop it:    hotlane discard %d\n", r.Version)
}

func cmdPromote(args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)
	if fs.NArg() != 1 {
		log.Fatal("usage: hotlane promote <version>")
	}
	v, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		log.Fatalf("hotlane promote: version must be a number, got %q", fs.Arg(0))
	}
	body, _ := json.Marshal(map[string]int{"version": v})
	resp, err := apiRequest("POST", *daemon+"/-/v1/promote", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("hotlane promote: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Println(string(bytes.TrimSpace(out)))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("hotlane promote: daemon: %s: %s", resp.Status, bytes.TrimSpace(out))
	}
	fmt.Printf("PROMOTED v%d live - it is byte-identical to what you tested\n", v)
}

func cmdDiscard(args []string) {
	fs := flag.NewFlagSet("discard", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)
	if fs.NArg() != 1 {
		log.Fatal("usage: hotlane discard <version>")
	}
	v, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		log.Fatalf("hotlane discard: version must be a number, got %q", fs.Arg(0))
	}
	body, _ := json.Marshal(map[string]int{"version": v})
	resp, err := apiRequest("POST", *daemon+"/-/v1/discard", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("hotlane discard: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		if resp.StatusCode == http.StatusNoContent {
			fmt.Printf("{\"discarded\":%d}\n", v)
			return
		}
		fmt.Println(string(bytes.TrimSpace(out)))
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusNoContent {
		log.Fatalf("hotlane discard: daemon: %s: %s", resp.Status, bytes.TrimSpace(out))
	}
	fmt.Printf("discarded held fork v%d - live traffic never knew\n", v)
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	if *jsonOut {
		resp, err := apiRequest("GET", *daemon+"/-/v1/status", "", nil)
		if err != nil {
			log.Fatalf("hotlane status: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Println(string(bytes.TrimSpace(body)))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	var st struct {
		App      string           `json:"app"`
		Live     string           `json:"live"`
		Version  int              `json:"version"`
		Backend  string           `json:"backend"`
		Ring     []pool.RingEntry `json:"ring"`
		LastFork *pool.ForkResult `json:"last_fork"`
		Archive  archive.Status   `json:"archive"`
	}
	getJSON(*daemon+"/-/v1/status", &st)
	fmt.Printf("app:  %s\n", st.App)
	fmt.Printf("live: v%d (%s) -> %s\n", st.Version, st.Live, st.Backend)
	fmt.Println("ring:")
	for _, e := range st.Ring {
		mark := " "
		if e.Live {
			mark = "*"
		}
		fmt.Printf("  %s v%-3d %-24s %s\n", mark, e.Version, e.Container, e.Status)
	}
	if st.LastFork != nil {
		fmt.Printf("last fork: v%d (snapshot %dms | patch %dms | boot %dms | total %dms)\n",
			st.LastFork.Version, st.LastFork.SnapshotMs, st.LastFork.PatchMs,
			st.LastFork.BootMs, st.LastFork.TotalMs)
	}
	drift := st.Archive.Drift
	if st.Archive.Building {
		drift += " (clean build in progress)"
	}
	fmt.Printf("archive: %s v%d, drift %s", st.Archive.Image, st.Archive.LastVersion, drift)
	if st.Archive.Detail != "" {
		fmt.Printf(" - %s", st.Archive.Detail)
	}
	fmt.Println()
}

// cmdDrift asks the daemon to cold-boot the clean image and diff its
// behavior against live, right now.
func cmdDrift(args []string) {
	fs := flag.NewFlagSet("drift", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	resp, err := apiRequest("POST", *daemon+"/-/v1/drift-check", "application/json", nil)
	if err != nil {
		log.Fatalf("hotlane drift: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Println(string(bytes.TrimSpace(body)))
		var st archive.Status
		if json.Unmarshal(body, &st) == nil && st.Drift == archive.DriftDrifted {
			os.Exit(1)
		}
		return
	}
	var st archive.Status
	if err := json.Unmarshal(body, &st); err != nil {
		log.Fatalf("hotlane drift: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	if st.Drift == archive.DriftClean {
		fmt.Printf("CLEAN: cold boot of %s behaves like live (checked %s)\n", st.Image, st.CheckedAt)
		return
	}
	log.Fatalf("DRIFTED: %s\nnext push will rebuild from %s", st.Detail, st.Image)
}

// cmdRollback flips traffic to a previous version: `hotlane rollback` for
// the one before live, `hotlane rollback 3` for v3 specifically.
func cmdRollback(args []string) {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	req := struct {
		Version int `json:"version"`
	}{}
	if fs.NArg() > 0 {
		v, err := strconv.Atoi(fs.Arg(0))
		if err != nil {
			log.Fatalf("hotlane rollback: version must be a number, got %q", fs.Arg(0))
		}
		req.Version = v
	}
	body, _ := json.Marshal(req)
	resp, err := apiRequest("POST", *daemon+"/-/v1/rollback", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("hotlane rollback: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Println(string(bytes.TrimSpace(out)))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("hotlane rollback: daemon: %s: %s", resp.Status, bytes.TrimSpace(out))
	}
	var res pool.RollbackResult
	if err := json.Unmarshal(out, &res); err != nil {
		log.Fatalf("hotlane rollback: bad response: %v", err)
	}
	how := "was still running"
	if res.Restarted {
		how = "restarted from ring"
	}
	fmt.Printf("ROLLED BACK to v%d (%s, %s) in %dms\n", res.Version, res.Container, how, res.TotalMs)
}

// worktreeDirty reports whether the current checkout has uncommitted
// tracked changes.
func worktreeDirty() bool {
	out, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output()
	return err != nil || len(bytes.TrimSpace(out)) > 0
}

// daemonBaselineE asks the daemon which commit its pristine snapshot
// was taken at. Empty when unknown (non-git source dir).
func daemonBaselineE(daemon string) (string, error) {
	var st struct {
		BaselineCommit string `json:"baseline_commit"`
	}
	resp, err := apiRequest("GET", daemon+"/-/v1/status", "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	json.Unmarshal(body, &st)
	return st.BaselineCommit, nil
}

func getJSON(url string, v any) {
	resp, err := apiRequest("GET", url, "", nil)
	if err != nil {
		log.Fatalf("hotlane: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("hotlane: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	if err := json.Unmarshal(body, v); err != nil {
		log.Fatalf("hotlane: bad response: %v", err)
	}
}
