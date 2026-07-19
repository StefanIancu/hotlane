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

	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/pool"
	"github.com/StefanIancu/hotlane/internal/proxy"
)

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
	case "status", "rollback":
		log.Fatalf("hotlane %s: not implemented yet (see docs/mvp.md milestones)", os.Args[1])
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
		})
	})
	mux.HandleFunc("POST /v1/push", func(w http.ResponseWriter, r *http.Request) {
		diff, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := p.Fork(diff)
		if err != nil {
			log.Printf("push: %v", err)
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
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
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("hotlane push: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var res pool.ForkResult
	if err := json.Unmarshal(body, &res); err != nil {
		log.Fatalf("hotlane push: bad response: %v", err)
	}
	fmt.Printf("forked %s (v%d) at %s\n", res.Container, res.Version, res.Backend)
	fmt.Printf("  snapshot %dms | patch %dms | boot %dms | total %dms\n",
		res.SnapshotMs, res.PatchMs, res.BootMs, res.TotalMs)
}
