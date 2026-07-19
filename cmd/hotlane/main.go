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
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
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
	case "push", "status", "rollback":
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

	p := &pool.Pool{Cfg: cfg, Src: src}
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
			"app":     cfg.App,
			"live":    p.Live,
			"version": p.Version,
			"backend": front.Target(),
		})
	})

	go func() {
		log.Printf("hotlane %s: app traffic on %s -> %s", version, *proxyAddr, front.Target())
		log.Fatal(http.ListenAndServe(*proxyAddr, front))
	}()
	log.Printf("hotlane %s: API on %s (app=%q ring=%d)", version, *apiAddr, cfg.App, cfg.Ring)
	log.Fatal(http.ListenAndServe(*apiAddr, mux))
}
