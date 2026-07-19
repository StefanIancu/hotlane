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
