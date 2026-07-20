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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/acme/autocert"

	"github.com/StefanIancu/hotlane/internal/archive"
	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/detect"
	"github.com/StefanIancu/hotlane/internal/docker"
	"github.com/StefanIancu/hotlane/internal/pool"
	"github.com/StefanIancu/hotlane/internal/replay"
	"github.com/StefanIancu/hotlane/internal/verify"
)

// pushResponse is the daemon's answer to a push: the fork, the verify
// verdict, and (on failure) the fork's dying words.
type pushResponse struct {
	*pool.ForkResult
	Verify   []verify.Result `json:"verify"`
	VerifyMs int64           `json:"verify_ms"`
	Promoted bool            `json:"promoted"`
	Replay   *replay.Result  `json:"replay,omitempty"`
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
	case "help", "-h", "--help":
		usage()
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
  HOTLANE_TOKEN   bearer token: required by clients when serve runs with -token
  HOTLANE_APP     app name on a multi-app daemon (-app flag wins; ./hotlane.yml's app: is the fallback)`)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, clientHint(err)
	}
	return resp, nil
}

// clientHint turns transport failures into the thing to actually do.
// "connection refused" on the default port means the daemon isn't
// running, which is the most common confusion in a first session.
func clientHint(err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return fmt.Errorf("%w\nno daemon is listening there. Start one on the app's host: hotlane serve\n"+
			"(remote daemon? point HOTLANE_DAEMON at it, e.g. HOTLANE_DAEMON=https://deploy.example.com)", err)
	case strings.Contains(s, "no such host"):
		return fmt.Errorf("%w\ncheck HOTLANE_DAEMON - that hostname does not resolve", err)
	default:
		return err
	}
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

// loopbackOnly reports whether a listen address can only be reached from
// this machine. An empty host (":7433") means every interface, which is
// the dangerous default this guards.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = strings.TrimSuffix(addr, ":")
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// dataRoot is where the daemon keeps per-app state: ~/.hotlane normally,
// /var/lib/hotlane when there is no home directory (systemd services with
// DynamicUser or no User= often run without $HOME).
func dataRoot() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".hotlane")
	}
	return "/var/lib/hotlane"
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "hotlane.yml", "path to hotlane.yml")
	appsDir := fs.String("apps", "", "directory of app configs (*.yml): serve every app in it, routing traffic by Host header")
	apiAddr := fs.String("addr", ":7433", "daemon API listen address")
	proxyAddr := fs.String("proxy", ":7480", "app traffic listen address")
	token := fs.String("token", os.Getenv("HOTLANE_TOKEN"), "bearer token required on the API (empty = open; keep the API loopback-only then)")
	tlsDomain := fs.String("tls-domain", "", "single-app: serve shared HTTPS on :443 with a Let's Encrypt certificate for this domain (requires -token; shorthand for domain: in the config plus -tls)")
	tlsOn := fs.Bool("tls", false, "serve shared HTTPS on :443 with Let's Encrypt certificates for every configured domain: (requires -token)")
	fs.Parse(args)

	if *appsDir != "" && *tlsDomain != "" {
		log.Fatal("hotlane: -tls-domain is single-app shorthand; with -apps, set domain: in each config and use -tls")
	}
	if (*tlsDomain != "" || *tlsOn) && *token == "" {
		log.Fatal("hotlane: TLS exposes the API publicly; a -token (or HOTLANE_TOKEN) is required with it")
	}
	// The API deploys code: reaching it is equivalent to running commands
	// on this host. Binding it anywhere but loopback without a token
	// would put an unauthenticated deploy endpoint on the network, so
	// refuse rather than warn.
	if *token == "" && !loopbackOnly(*apiAddr) {
		log.Fatalf("hotlane: -addr %q binds beyond loopback and no -token is set - that would expose an\n"+
			"unauthenticated deploy API (anyone who reaches it can run code on this host).\n"+
			"  either: hotlane serve -addr 127.0.0.1:7433        (local only, no token needed)\n"+
			"  or:     hotlane serve -token \"$(openssl rand -hex 24)\"", *apiAddr)
	}

	// One config (traditional) or a directory of them (multi-app).
	var cfgs []*config.Config
	if *appsDir != "" {
		var err error
		if cfgs, err = config.LoadDir(*appsDir); err != nil {
			log.Fatalf("hotlane: %v", err)
		}
	} else {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("hotlane: %v", err)
		}
		// The source checkout: src: from the config when set (Load already
		// resolved it against the config's directory), else the config's
		// directory itself - serve traditionally starts inside the repo.
		if cfg.Src == "" {
			if cfg.Src, err = filepath.Abs(filepath.Dir(*cfgPath)); err != nil {
				log.Fatalf("hotlane: %v", err)
			}
		}
		if *tlsDomain != "" {
			cfg.Domain = *tlsDomain
			*tlsOn = true
		}
		cfgs = []*config.Config{cfg}
	}

	// Fail on the real problem before booting anything: a missing or
	// unusable Docker is somebody's first thirty seconds with hotlane.
	if err := docker.Preflight(); err != nil {
		log.Fatalf("hotlane: %v", err)
	}

	root := dataRoot()
	var apps []*appRuntime
	for _, cfg := range cfgs {
		a, err := newAppRuntime(cfg, cfg.Src, root)
		if err != nil {
			log.Fatalf("hotlane: %s: %v", cfg.App, err)
		}
		apps = append(apps, a)
	}
	single := len(apps) == 1
	startDriftTicker(apps)

	// App traffic: a single app serves every request regardless of Host -
	// today's contract, unchanged. Multiple apps route by Host header;
	// unknown host is an explicit 421, never a fall-through to some
	// default app - serving app A's traffic to app B because of a typo'd
	// DNS record must be impossible.
	var appTraffic http.Handler
	if single {
		appTraffic = apps[0].trafficHandler()
	} else {
		hostTraffic := map[string]http.Handler{}
		for _, a := range apps {
			if a.cfg.Domain != "" {
				hostTraffic[a.cfg.Domain] = a.trafficHandler()
			}
		}
		appTraffic = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			h, ok := hostTraffic[host]
			if !ok {
				http.Error(w, "hotlane: no app for host "+host, http.StatusMisdirectedRequest)
				return
			}
			h.ServeHTTP(w, r)
		})
	}

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
	// The bare app-scoped paths: canonical surface for a single-app daemon
	// (full back-compat), 400 with directions on a multi-app one - loud,
	// not silent.
	docBase := "/-/v1"
	if !single {
		docBase = "/-/v1/apps/<app>"
	}
	handle("GET /v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"service": "hotlane",
			"version": version,
			"docs":    "https://hotlane.dev/llms-full.txt",
			"routes": []string{
				"GET  /-/healthz              liveness (no auth)",
				"GET  /-/v1/apps              apps served by this daemon",
				"GET  " + docBase + "/status            live version, ring, held forks, archive/drift, baseline_commit",
				"POST " + docBase + "/push              body=raw git diff -> fork, verify, promote; 422 on rejection",
				"POST " + docBase + "/test              body=raw git diff -> fork, verify, HOLD; returns X-Hotlane-Fork header",
				"POST " + docBase + "/promote           {version} -> flip traffic to a held fork",
				"POST " + docBase + "/discard           {version} -> destroy a held fork",
				"POST " + docBase + "/rollback          {version?} -> flip to a kept version",
				"POST " + docBase + "/drift-check       cold-boot clean image, diff behavior vs live",
				"GET  " + docBase + "/logs?tail=N       live container output",
			},
		})
	})
	handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// No app-name enumeration on the one unauthenticated route.
		if single {
			fmt.Fprintf(w, "ok app=%s version=%s\n", apps[0].cfg.App, version)
		} else {
			fmt.Fprintf(w, "ok apps=%d version=%s\n", len(apps), version)
		}
	})
	handle("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 0, len(apps))
		for _, a := range apps {
			out = append(out, map[string]any{
				"app":     a.cfg.App,
				"domain":  a.cfg.Domain,
				"version": a.pool.State().Version,
				"live":    a.pool.State().Live,
				"drift":   a.arch.Status().Drift,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	appRoutes := func(prefix string, a *appRuntime) {
		handle("GET "+prefix+"/status", a.handleStatus)
		handle("GET "+prefix+"/logs", a.handleLogs)
		handle("POST "+prefix+"/push", a.handlePush)
		handle("POST "+prefix+"/test", a.handleTest)
		handle("POST "+prefix+"/promote", a.handlePromote)
		handle("POST "+prefix+"/discard", a.handleDiscard)
		handle("POST "+prefix+"/rollback", a.handleRollback)
		handle("POST "+prefix+"/drift-check", a.handleDriftCheck)
	}
	for _, a := range apps {
		appRoutes("/v1/apps/"+a.cfg.App, a)
	}
	if single {
		appRoutes("/v1", apps[0])
	} else {
		multiOnly := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "hotlane: this daemon serves multiple apps; use /-/v1/apps/<app>/...", http.StatusBadRequest)
		}
		for _, pat := range []string{
			"GET /v1/status", "GET /v1/logs", "POST /v1/push", "POST /v1/test",
			"POST /v1/promote", "POST /v1/discard", "POST /v1/rollback", "POST /v1/drift-check",
		} {
			handle(pat, multiOnly)
		}
	}

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
		if single {
			log.Printf("hotlane %s: app traffic on %s -> %s", version, *proxyAddr, apps[0].front.Target())
		} else {
			log.Printf("hotlane %s: app traffic on %s (%d apps, routed by Host)", version, *proxyAddr, len(apps))
		}
		log.Fatal(http.ListenAndServe(*proxyAddr, appTraffic))
	}()

	if *tlsOn {
		// Shared HTTPS listener on :443: each APP owns https://its-domain/
		// (a browser typing the domain gets the app, with TLS, zero
		// config), while the daemon API lives under the /-/ prefix. The
		// private API listener keeps running unchanged. Certificates come
		// from Let's Encrypt via the TLS-ALPN challenge, one per domain.
		var domains []string
		for _, a := range apps {
			if a.cfg.Domain != "" {
				domains = append(domains, a.cfg.Domain)
			}
		}
		if len(domains) == 0 {
			log.Fatal("hotlane: -tls needs at least one app with domain: set")
		}
		mgr := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(domains...),
			Cache:      autocert.DirCache(filepath.Join(root, "autocert")),
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
			// Single app keeps the canonical-domain redirect; multi-app
			// redirects to the requested host (which :443 then routes).
			redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target := domains[0]
				if !single {
					target = r.Host
					if h, _, err := net.SplitHostPort(target); err == nil {
						target = h
					}
				}
				http.Redirect(w, r, "https://"+target+r.URL.RequestURI(), http.StatusMovedPermanently)
			})
			if err := http.ListenAndServe(":80", redirect); err != nil {
				log.Printf("hotlane: :80 redirect listener: %v", err)
			}
		}()
		go func() {
			if single {
				log.Printf("hotlane %s: app on https://%s/ + API on https://%s/-/ (ring=%d)", version, domains[0], domains[0], apps[0].cfg.Ring)
			} else {
				log.Printf("hotlane %s: %d apps on :443 (%s) + API under /-/", version, len(apps), strings.Join(domains, ", "))
			}
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()
	}
	if single {
		log.Printf("hotlane %s: API on %s (app=%q ring=%d)", version, *apiAddr, apps[0].cfg.App, apps[0].cfg.Ring)
	} else {
		log.Printf("hotlane %s: API on %s (%d apps)", version, *apiAddr, len(apps))
	}
	log.Fatal(http.ListenAndServe(*apiAddr, api))
}

// cmdLogs tails the live version's output through the daemon.
func cmdLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
	tail := fs.Int("n", 100, "number of log lines")
	fs.Parse(args)

	resp, err := appRequest("GET", *daemon, clientBase(*appName), fmt.Sprintf("/logs?tail=%d", *tail), "", nil)
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

// printReplay renders a replay verdict in push/test human output.
func printReplay(r *replay.Result) {
	if r == nil || (r.Replayed == 0 && !r.BudgetHit) {
		return
	}
	line := fmt.Sprintf("  replay %d/%d matched", r.Matched, r.Replayed)
	if r.Dynamic > 0 {
		line += fmt.Sprintf(" (%d dynamic, status-only)", r.Dynamic)
	}
	if r.BudgetHit {
		line += fmt.Sprintf(" [BUDGET EXPIRED - only %d of %d judged]", r.Replayed, r.Buffered)
	}
	fmt.Printf("%s (%dms, %d buffered)\n", line, r.Ms, r.Buffered)
	for _, m := range r.Mismatches {
		if m.Want == "" && m.Got != "" && m.GotStatus == 0 {
			fmt.Printf("  MISMATCH %s %s: %s\n", m.Method, m.Path, m.Got)
			continue
		}
		if m.WantStatus != m.GotStatus {
			fmt.Printf("  MISMATCH %s %s: live answered %d, fork answers %d\n", m.Method, m.Path, m.WantStatus, m.GotStatus)
			continue
		}
		fmt.Printf("  MISMATCH %s %s: live served %q, fork serves %q\n", m.Method, m.Path, m.Want, m.Got)
	}
	if r.Mismatched > len(r.Mismatches) {
		fmt.Printf("  ...and %d more mismatches (see -json)\n", r.Mismatched-len(r.Mismatches))
	}
}

// clientBase resolves the app-scoped API base for client commands: an
// explicit -app flag wins, then HOTLANE_APP (multi-app repos often keep
// their config in the daemon's -apps directory, not the checkout), then
// the app: named by the working directory's hotlane.yml, else the bare
// single-app path. Multi-app daemons reject bare paths with directions,
// so a client with none of these gets a loud, actionable error rather
// than the wrong app.
func clientBase(appFlag string) string {
	name := appFlag
	if name == "" {
		name = os.Getenv("HOTLANE_APP")
	}
	if name == "" {
		if c, err := config.Load("hotlane.yml"); err == nil {
			name = c.App
		}
	}
	if name == "" {
		return "/-/v1"
	}
	return "/-/v1/apps/" + name
}

// appRequest performs an app-scoped API call against base+sub. A 404
// from the namespaced path retries the bare path once: daemons predating
// the /apps namespace (<= 0.4.x) only serve the bare routes.
func appRequest(method, daemon, base, sub, contentType string, body []byte) (*http.Response, error) {
	reader := func() io.Reader {
		if body == nil {
			return nil
		}
		return bytes.NewReader(body)
	}
	resp, err := apiRequest(method, daemon+base+sub, contentType, reader())
	if err != nil || base == "/-/v1" || resp.StatusCode != http.StatusNotFound {
		return resp, err
	}
	resp.Body.Close()
	// A 404 on the namespaced path means one of two very different
	// things: the daemon predates the /apps namespace, or it knows the
	// namespace and this app name is not on it. Retrying blindly would
	// send the request to whatever single app the daemon does serve -
	// so `hotlane rollback -app typo` would roll back an unrelated
	// production app and report success. Ask the daemon which it is.
	if probe, perr := apiRequest("GET", daemon+"/-/v1/apps", "", nil); perr == nil {
		known := probe.StatusCode != http.StatusNotFound
		probe.Body.Close()
		if known {
			name := strings.TrimPrefix(base, "/-/v1/apps/")
			return nil, fmt.Errorf("this daemon does not serve an app named %q - check -app / HOTLANE_APP / the app: field in hotlane.yml (hotlane status -all lists them)", name)
		}
	}
	return apiRequest(method, daemon+"/-/v1"+sub, contentType, reader())
}

// computeDiffE builds the delta to send: from the daemon's baseline
// commit (or an explicit ref) to the working tree.
func computeDiffE(daemon, apiBase, from string) ([]byte, error) {
	base := from
	if base == "" {
		b, err := daemonBaselineE(daemon, apiBase)
		if err != nil {
			return nil, err
		}
		base = b
	}
	// No --relative: the daemon applies the patch at the repo ROOT of its
	// shadow tree, so paths must be repo-root-relative. With --relative,
	// running `hotlane push` from a subdirectory silently drops every
	// change outside that directory and rewrites the remaining paths -
	// deploying a partial changeset that reports success.
	// --binary: without it, a changed image/font/wasm becomes "Binary
	// files differ", which git apply refuses - rejecting the whole push,
	// including the text changes alongside it.
	diffArgs := []string{"diff", "HEAD", "--binary"}
	if base != "" {
		if exec.Command("git", "cat-file", "-e", base+"^{commit}").Run() != nil {
			return nil, fmt.Errorf("baseline commit %.12s is not in this clone - CI checkouts need history: use fetch-depth: 0 (or git fetch --unshallow) so the diff base exists", base)
		}
		diffArgs = []string{"diff", base, "--binary"}
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
func computeDiff(daemon, apiBase, from string) []byte {
	diff, err := computeDiffE(daemon, apiBase, from)
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
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
	from := fs.String("from", "", "git ref to diff from (default: dirty worktree diffs HEAD, clean worktree diffs the daemon's baseline commit)")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	base := clientBase(*appName)
	diff := computeDiff(*daemon, base, *from)

	resp, err := appRequest("POST", *daemon, base, "/push", "text/x-diff", diff)
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
	printReplay(res.Replay)
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
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
	from := fs.String("from", "", "git ref to diff from (default: the daemon's baseline commit)")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	base := clientBase(*appName)
	diff := computeDiff(*daemon, base, *from)
	resp, err := appRequest("POST", *daemon, base, "/test", "text/x-diff", diff)
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
	printReplay(r.Replay)
	fmt.Printf("HELD v%d for %s - live traffic untouched\n", r.Version, out.ExpiresIn)
	fmt.Printf("  poke it:    curl -H %q <your-app-url>/...\n", out.Header)
	fmt.Printf("  ship it:    hotlane promote %d\n", r.Version)
	fmt.Printf("  drop it:    hotlane discard %d\n", r.Version)
}

func cmdPromote(args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
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
	resp, err := appRequest("POST", *daemon, clientBase(*appName), "/promote", "application/json", body)
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
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
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
	resp, err := appRequest("POST", *daemon, clientBase(*appName), "/discard", "application/json", body)
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
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	all := fs.Bool("all", false, "list every app the daemon serves (multi-app daemons)")
	fs.Parse(args)

	if *all {
		resp, err := apiRequest("GET", *daemon+"/-/v1/apps", "", nil)
		if err != nil {
			log.Fatalf("hotlane status: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			log.Fatalf("hotlane status: daemon: %s: %s (a daemon older than 0.5 has no /-/v1/apps)", resp.Status, bytes.TrimSpace(body))
		}
		if *jsonOut {
			fmt.Println(string(bytes.TrimSpace(body)))
			return
		}
		var list []struct {
			App     string `json:"app"`
			Domain  string `json:"domain"`
			Version int    `json:"version"`
			Live    string `json:"live"`
			Drift   string `json:"drift"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			log.Fatalf("hotlane status: bad response: %v", err)
		}
		for _, e := range list {
			domain := e.Domain
			if domain == "" {
				domain = "-"
			}
			fmt.Printf("%-16s v%-4d %-28s drift %s\n", e.App, e.Version, domain, e.Drift)
		}
		return
	}

	base := clientBase(*appName)
	if *jsonOut {
		resp, err := appRequest("GET", *daemon, base, "/status", "", nil)
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
	{
		resp, err := appRequest("GET", *daemon, base, "/status", "", nil)
		if err != nil {
			log.Fatalf("hotlane: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			log.Fatalf("hotlane: daemon: %s: %s", resp.Status, bytes.TrimSpace(body))
		}
		if err := json.Unmarshal(body, &st); err != nil {
			log.Fatalf("hotlane: bad response: %v", err)
		}
	}
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
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
	jsonOut := fs.Bool("json", false, "print the daemon's raw JSON response")
	fs.Parse(args)

	resp, err := appRequest("POST", *daemon, clientBase(*appName), "/drift-check", "application/json", nil)
	if err != nil {
		log.Fatalf("hotlane drift: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Println(string(bytes.TrimSpace(body)))
		// A non-200 is a failed check, not a clean one: exiting 0 here
		// would make a 401 or a misrouted request look like "no drift"
		// to the CI job that gates on it.
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
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
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
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
	resp, err := appRequest("POST", *daemon, clientBase(*appName), "/rollback", "application/json", body)
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
func daemonBaselineE(daemon, apiBase string) (string, error) {
	var st struct {
		BaselineCommit string `json:"baseline_commit"`
	}
	resp, err := appRequest("GET", daemon, apiBase, "/status", "", nil)
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
