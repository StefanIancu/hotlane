package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, yml string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hotlane.yml")
	if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const base = "app: demo\nimage: node:22-alpine\nrun: node server.js\nport: 3000\n"

func TestInterpolateNotifyAndArchive(t *testing.T) {
	t.Setenv("HL_TEST_NOTIFY", "https://hooks.example.com/T00/secret")
	t.Setenv("HL_TEST_REG", "ghcr.io/acme")
	c, err := load(t, base+"notify: ${HL_TEST_NOTIFY}\narchive: ${HL_TEST_REG}/api\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Notify != "https://hooks.example.com/T00/secret" {
		t.Errorf("notify = %q", c.Notify)
	}
	if c.Archive != "ghcr.io/acme/api" {
		t.Errorf("archive = %q", c.Archive)
	}
}

func TestInterpolateUnsetVarIsError(t *testing.T) {
	os.Unsetenv("HL_TEST_MISSING")
	_, err := load(t, base+"notify: ${HL_TEST_MISSING}\n")
	if err == nil || !strings.Contains(err.Error(), "HL_TEST_MISSING") {
		t.Errorf("want error naming HL_TEST_MISSING, got %v", err)
	}
}

func TestInterpolateLeavesScriptsAlone(t *testing.T) {
	// ${PORT} in run/build/verify scripts belongs to the shell inside the
	// container - config load must not touch it (or require it set).
	c, err := load(t, base+"build: echo ${HL_TEST_NOT_SET_EITHER}\nverify:\n  - run: curl -f localhost:${PORT}/health\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Build != "echo ${HL_TEST_NOT_SET_EITHER}" {
		t.Errorf("build = %q", c.Build)
	}
	if c.Verify[0].Run != "curl -f localhost:${PORT}/health" {
		t.Errorf("verify run = %q", c.Verify[0].Run)
	}
}

func TestInterpolateBareDollarUntouched(t *testing.T) {
	c, err := load(t, base+"notify: https://h.example.com/a$b/$NOPE\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Notify != "https://h.example.com/a$b/$NOPE" {
		t.Errorf("notify = %q", c.Notify)
	}
}

func TestVerifyTimeout(t *testing.T) {
	c, err := load(t, base+"verify:\n  - http: /health == 200\n    timeout: 5s\n  - run: ./smoke.sh\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(c.Verify[0].Timeout); got != 5*time.Second {
		t.Errorf("timeout = %v", got)
	}
	if c.Verify[1].Timeout != 0 {
		t.Errorf("unset timeout = %v, want 0", c.Verify[1].Timeout)
	}
}

func TestVerifyTimeoutRejectsUnitless(t *testing.T) {
	_, err := load(t, base+"verify:\n  - http: /health == 200\n    timeout: 90\n")
	if err == nil || !strings.Contains(err.Error(), "duration") {
		t.Errorf("want duration parse error, got %v", err)
	}
}

func TestVerifyTimeoutRejectsNegative(t *testing.T) {
	_, err := load(t, base+"verify:\n  - http: /health == 200\n    timeout: -5s\n")
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Errorf("want positive-timeout error, got %v", err)
	}
}

func TestSrcResolvesAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "checkout"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "hotlane.yml")
	if err := os.WriteFile(p, []byte(base+"src: ./checkout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Src != filepath.Join(dir, "checkout") {
		t.Errorf("src = %q, want %q", c.Src, filepath.Join(dir, "checkout"))
	}
}

func TestDomainRejectsNonHostnames(t *testing.T) {
	for _, d := range []string{"https://api.example.com", "api.example.com/path", "api.example.com:443"} {
		if _, err := load(t, base+"domain: \""+d+"\"\n"); err == nil {
			t.Errorf("domain %q accepted, want bare-hostname error", d)
		}
	}
	if _, err := load(t, base+"domain: api.example.com\n"); err != nil {
		t.Errorf("bare hostname rejected: %v", err)
	}
}

// writeApps lays out a multi-app config directory with per-app checkouts.
func writeApps(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, yml := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func appYML(app, domain string) string {
	return "app: " + app + "\nimage: node:22-alpine\nrun: node server.js\nport: 3000\nsrc: ./srv\ndomain: " + domain + "\n"
}

func TestLoadDir(t *testing.T) {
	dir := writeApps(t, map[string]string{
		"api.yml":  appYML("api", "api.example.com"),
		"blog.yml": appYML("blog", "blog.example.com"),
	})
	configs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 || configs[0].App != "api" || configs[1].App != "blog" {
		t.Errorf("got %d configs, want sorted [api blog]", len(configs))
	}
}

func TestLoadDirAllOrNothing(t *testing.T) {
	// Three distinct problems across two files - every one must be in the
	// error, and no configs may come back.
	dir := writeApps(t, map[string]string{
		"a.yml": appYML("api", "api.example.com"),
		"b.yml": "app: api\nimage: node:22-alpine\nrun: node server.js\nport: 3000\ndomain: api.example.com\n", // dup app, dup domain, no src
	})
	configs, err := LoadDir(dir)
	if configs != nil || err == nil {
		t.Fatalf("want nil configs + error, got %v, %v", configs, err)
	}
	for _, frag := range []string{"already defined", "already routed", "src: is required"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error missing %q:\n%v", frag, err)
		}
	}
}

func TestLoadDirEmpty(t *testing.T) {
	if _, err := LoadDir(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no *.yml") {
		t.Errorf("want no-configs error, got %v", err)
	}
}

func TestLoadDirMissingSrcDir(t *testing.T) {
	dir := writeApps(t, map[string]string{"api.yml": "app: api\nimage: node:22-alpine\nrun: node server.js\nport: 3000\nsrc: ./nope\n"})
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("want missing-src error, got %v", err)
	}
}

// The app name becomes an API route pattern, where "{x}" would register
// a ServeMux wildcard that swallows every other app's routes, and ".."
// or a space crashes the daemon at startup.
func TestAppNameCharsetEnforced(t *testing.T) {
	for _, bad := range []string{"{x}", "{x...}", "..", "a b", "UPPER", "under_score", "-lead", "sla/sh"} {
		if _, err := load(t, strings.Replace(base, "app: demo", "app: "+"\""+bad+"\"", 1)); err == nil {
			t.Errorf("app name %q accepted", bad)
		}
	}
	for _, ok := range []string{"demo", "api2", "my-app", "a"} {
		if _, err := load(t, strings.Replace(base, "app: demo", "app: "+ok, 1)); err != nil {
			t.Errorf("app name %q rejected: %v", ok, err)
		}
	}
}
