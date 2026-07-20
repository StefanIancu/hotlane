// Package detect guesses a working hotlane.yml from what's in a repo.
// Heuristics, not magic: the output is a starting point with the guesses
// commented, and `hotlane serve` validates whatever the user keeps.
package detect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Guess is a suggested configuration.
type Guess struct {
	App   string
	Image string
	Build string
	Run   string
	Port  int
	// VerifyTimeout overrides the 15s http-hook default for stacks that
	// compile at boot. The warm path is fast (the build cache is
	// inherited from the live container), but the COLD path - the
	// archivist's drift check, and any fork from the clean image - starts
	// with no cache. Left at the default, a Go app fails its own drift
	// check, gets flagged drifted, and then every push forks from clean
	// and fails the same way: a self-sustaining rejection loop on a
	// perfectly healthy app.
	VerifyTimeout string
	Framework     string // human label for the init message
}

// Detect inspects dir and returns its best guess.
func Detect(dir string) *Guess {
	g := &Guess{App: appName(dir)}
	switch {
	case exists(dir, "package.json"):
		detectNode(dir, g)
	case exists(dir, "requirements.txt") || exists(dir, "pyproject.toml"):
		detectPython(dir, g)
	case exists(dir, "go.mod"):
		g.Framework = "Go"
		g.Image = "golang:1.24-alpine"
		g.Run = "go run ."
		g.Port = 8080
		g.VerifyTimeout = "120s"
	default:
		g.Framework = "unknown"
		g.Image = "alpine:3.20"
		g.Run = "echo 'edit hotlane.yml: set your run command' && sleep infinity"
		g.Port = 8080
	}
	return g
}

func detectNode(dir string, g *Guess) {
	g.Framework = "Node.js"
	g.Image = "node:22-alpine"
	g.Port = firstPort(dir, []string{"server.js", "index.js", "app.js", "src/server.ts", "src/index.ts"}, 3000)

	var pkg struct {
		Main            string            `json:"main"`
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err == nil {
		json.Unmarshal(raw, &pkg)
	}
	_, ts := pkg.Dependencies["typescript"]
	_, tsDev := pkg.DevDependencies["typescript"]
	if ts || tsDev {
		g.Framework = "Node.js (TypeScript)"
	}

	g.Build = "npm install --no-audit --no-fund"
	if _, ok := pkg.Scripts["build"]; ok {
		g.Build += " && npm run build"
		g.Framework += " with build step"
	}
	switch {
	case pkg.Scripts["start"] != "":
		g.Run = "npm start"
	case pkg.Main != "":
		g.Run = "node " + pkg.Main
	default:
		g.Run = "node index.js"
	}
}

func detectPython(dir string, g *Guess) {
	g.Image = "python:3.12-slim"
	g.Port = 8000

	deps := ""
	if raw, err := os.ReadFile(filepath.Join(dir, "requirements.txt")); err == nil {
		deps = strings.ToLower(string(raw))
		g.Build = "pip install -r requirements.txt"
	} else {
		deps = readLower(dir, "pyproject.toml")
		g.Build = "pip install ."
	}

	module := "main"
	for _, cand := range []string{"main.py", "app.py", "server.py"} {
		if exists(dir, cand) {
			module = strings.TrimSuffix(cand, ".py")
			break
		}
	}

	switch {
	case strings.Contains(deps, "fastapi"):
		g.Framework = "FastAPI"
		g.Run = fmt.Sprintf("uvicorn %s:app --host 0.0.0.0 --port 8000", module)
	case strings.Contains(deps, "flask"):
		g.Framework = "Flask"
		g.Run = fmt.Sprintf("flask --app %s run --host 0.0.0.0 --port 5000", module)
		g.Port = 5000
	case exists(dir, "manage.py"):
		g.Framework = "Django"
		g.Run = "python manage.py runserver 0.0.0.0:8000"
	default:
		g.Framework = "Python"
		g.Run = "python " + module + ".py"
	}
}

// Port sniffing, most specific first. The old rule required a digit
// immediately after `listen(`, which misses the single most common form
// in real Node apps - `listen(process.env.PORT || 8080)` - and silently
// fell back to 3000, generating a config whose verify hook can never
// pass because nothing listens there.
var portRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)process\.env\.PORT\s*\|\|\s*(\d{2,5})`),     // listen(process.env.PORT || 8080)
	regexp.MustCompile(`(?i)\bPORT\s*[=:]\s*['"` + "`" + `]?(\d{2,5})`), // const PORT = 8080 / PORT: 8080
	regexp.MustCompile(`listen\(\s*(\d{2,5})`),                          // listen(3000, ...)
	regexp.MustCompile(`listen\([^)]{0,80}?(\d{2,5})`),                  // listen(host, 8080) and friends
}

// commentRe strips line comments before sniffing: a commented-out
// `// app.listen(4000)` above the real one used to win, because the
// first match in the file was taken.
var commentRe = regexp.MustCompile(`(?m)^\s*(//|#).*$`)

// firstPort scans likely entrypoints for a listen(<port>) call.
func firstPort(dir string, candidates []string, fallback int) int {
	for _, c := range candidates {
		raw, err := os.ReadFile(filepath.Join(dir, c))
		if err != nil {
			continue
		}
		body := commentRe.ReplaceAll(raw, nil)
		for _, re := range portRes {
			m := re.FindSubmatch(body)
			if m == nil {
				continue
			}
			var p int
			fmt.Sscanf(string(m[1]), "%d", &p)
			if p > 0 {
				return p
			}
		}
	}
	return fallback
}

var nameRe = regexp.MustCompile(`[^a-z0-9-]+`)

func appName(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "app"
	}
	name := nameRe.ReplaceAllString(strings.ToLower(filepath.Base(abs)), "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "app"
	}
	return name
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func readLower(dir, name string) string {
	raw, _ := os.ReadFile(filepath.Join(dir, name))
	return strings.ToLower(string(raw))
}

// YAML renders the guess as a commented starter hotlane.yml.
func (g *Guess) YAML() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by hotlane init (detected: %s). Review before serving.\n", g.Framework)
	fmt.Fprintf(&b, "app: %s\n", g.App)
	fmt.Fprintf(&b, "image: %s\n", g.Image)
	if g.Build != "" {
		fmt.Fprintf(&b, "build: %s\n", g.Build)
	}
	fmt.Fprintf(&b, "run: %s\n", g.Run)
	fmt.Fprintf(&b, "port: %d\n", g.Port)
	b.WriteString("verify:\n")
	b.WriteString("  - http: / == 200        # add a real /health endpoint and check it here\n")
	if g.VerifyTimeout != "" {
		fmt.Fprintf(&b, "    timeout: %s         # this stack compiles at boot; a COLD start (drift check,\n", g.VerifyTimeout)
		b.WriteString("                          #   or a fork from the clean image) has no build cache\n")
	} else {
		b.WriteString("  #  timeout: 5s          # optional per-hook budget (defaults: http 15s, run 60s)\n")
	}
	b.WriteString("ring: 5                   # versions kept for instant rollback\n")
	b.WriteString("# archive: registry/ref   # push the archivist's clean images here\n")
	b.WriteString("# notify: ${HOTLANE_NOTIFY_URL}  # webhook for drift + rejected-push events;\n")
	b.WriteString("#                         # ${VAR} interpolates from the daemon's environment\n")
	return b.String()
}
