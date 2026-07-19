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
	"sync"
	"time"

	"github.com/StefanIancu/hotlane/internal/archive"
	"github.com/StefanIancu/hotlane/internal/config"
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

const version = "0.0.1-dev"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "push":
		cmdPush(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "rollback":
		cmdRollback(os.Args[2:])
	case "drift":
		cmdDrift(os.Args[2:])
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
  serve     run the daemon on this host
  push      send the current git delta to the daemon
  status    show live version, ring, and last verify results
  rollback  flip the proxy to a previous version
  drift     cold-boot the clean image and diff its behavior against live
  version   print version`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "hotlane.yml", "path to hotlane.yml")
	apiAddr := fs.String("addr", ":7433", "daemon API listen address")
	proxyAddr := fs.String("proxy", ":7480", "app traffic listen address")
	fs.Parse(args)

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

	notif := &notify.Notifier{URL: cfg.Notify, App: cfg.App}
	arch := archive.New(cfg, p.DataDir, notif)
	// Archive the starting state so a clean image exists from day one; on
	// adopt this is the working tree, which matches the baseline contract
	// (serve starts from a clean checkout).
	if err := arch.Snapshot(src); err != nil {
		log.Printf("hotlane: archive snapshot: %v", err)
	} else {
		go arch.Archive(p.Version, p.Backend)
	}
	go func() {
		for range time.Tick(6 * time.Hour) {
			arch.DriftCheck(p.Backend)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok app=%s version=%s\n", cfg.App, version)
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"app":       cfg.App,
			"live":      p.Live,
			"version":   p.Version,
			"backend":   front.Target(),
			"last_fork": p.LastFork,
			"ring":      p.Ring(),
			"archive":   arch.Status(),
		})
	})
	mux.HandleFunc("POST /v1/drift-check", func(w http.ResponseWriter, r *http.Request) {
		st := arch.DriftCheck(p.Backend)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /v1/rollback", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Version int `json:"version"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		res, err := p.Rollback(req.Version)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		front.Set(res.Backend)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	// Pushes are serialized: forks share the shadow tree and the version
	// counter, and one verified promote at a time is the contract.
	var pushMu sync.Mutex
	mux.HandleFunc("POST /v1/push", func(w http.ResponseWriter, r *http.Request) {
		diff, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pushMu.Lock()
		defer pushMu.Unlock()

		// Drift recovery: while the app is flagged drifted, forks build
		// from the clean image instead of the warm chain.
		base := ""
		if arch.Drifted() {
			base = arch.CleanImage()
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

	go func() {
		log.Printf("hotlane %s: app traffic on %s -> %s", version, *proxyAddr, front.Target())
		log.Fatal(http.ListenAndServe(*proxyAddr, front))
	}()
	log.Printf("hotlane %s: API on %s (app=%q ring=%d)", version, *apiAddr, cfg.App, cfg.Ring)
	log.Fatal(http.ListenAndServe(*apiAddr, mux))
}

// cmdPush sends the working tree's cumulative delta (git diff HEAD, paths
// relative to the current directory) to the daemon. New files must be
// tracked (`git add -N`) to appear in the diff.
func cmdPush(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	daemon := fs.String("daemon", "http://127.0.0.1:7433", "daemon API base URL")
	fs.Parse(args)

	diffCmd := exec.Command("git", "diff", "HEAD", "--relative")
	diffCmd.Stderr = os.Stderr
	diff, err := diffCmd.Output()
	if err != nil {
		log.Fatalf("hotlane push: git diff: %v", err)
	}
	if len(bytes.TrimSpace(diff)) == 0 {
		log.Printf("hotlane push: no local changes; forking current state anyway")
	}

	resp, err := http.Post(*daemon+"/v1/push", "text/x-diff", bytes.NewReader(diff))
	if err != nil {
		log.Fatalf("hotlane push: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var res pushResponse
	if err := json.Unmarshal(body, &res); err != nil {
		log.Fatalf("hotlane push: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	fmt.Printf("fork %s (v%d): snapshot %dms | patch %dms | boot %dms | verify %dms\n",
		res.Container, res.Version, res.SnapshotMs, res.PatchMs, res.BootMs, res.VerifyMs)
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

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	daemon := fs.String("daemon", "http://127.0.0.1:7433", "daemon API base URL")
	fs.Parse(args)

	var st struct {
		App      string           `json:"app"`
		Live     string           `json:"live"`
		Version  int              `json:"version"`
		Backend  string           `json:"backend"`
		Ring     []pool.RingEntry `json:"ring"`
		LastFork *pool.ForkResult `json:"last_fork"`
		Archive  archive.Status   `json:"archive"`
	}
	getJSON(*daemon+"/v1/status", &st)
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
	daemon := fs.String("daemon", "http://127.0.0.1:7433", "daemon API base URL")
	fs.Parse(args)

	resp, err := http.Post(*daemon+"/v1/drift-check", "application/json", nil)
	if err != nil {
		log.Fatalf("hotlane drift: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
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
	daemon := fs.String("daemon", "http://127.0.0.1:7433", "daemon API base URL")
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
	resp, err := http.Post(*daemon+"/v1/rollback", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("hotlane rollback: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
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

func getJSON(url string, v any) {
	resp, err := http.Get(url)
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
