package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func app(t *testing.T, files map[string]string) *Guess {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Detect(dir)
}

// A Go app compiles at boot. The warm path inherits the build cache, but
// the archivist's drift check and any fork from the clean image start
// cold - and at the 15s http default, a healthy Go app fails its own
// drift check, gets flagged drifted, and then every push forks from
// clean and fails identically. Found on a real box; never regress it.
func TestGoGetsAColdStartBudget(t *testing.T) {
	g := app(t, map[string]string{"go.mod": "module x\n\ngo 1.21\n", "main.go": "package main\n"})
	if g.Framework != "Go" {
		t.Fatalf("framework = %q", g.Framework)
	}
	if g.VerifyTimeout == "" {
		t.Fatal("Go app generated no verify timeout - cold boots will fail the 15s default")
	}
	yml := g.YAML()
	if !strings.Contains(yml, "timeout: "+g.VerifyTimeout) {
		t.Errorf("timeout not emitted into config:\n%s", yml)
	}
	if !strings.Contains(yml, "compiles at boot") {
		t.Errorf("timeout emitted without explaining why:\n%s", yml)
	}
}

// Interpreted stacks start fast cold; they keep the commented-out hint
// rather than an opinionated override.
func TestInterpretedStacksKeepTheDefault(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
	}{
		{"node", map[string]string{"package.json": `{"name":"n","scripts":{"start":"node s.js"}}`}},
		{"python", map[string]string{"requirements.txt": "fastapi\nuvicorn\n", "main.py": "app = 1\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := app(t, c.files)
			if g.VerifyTimeout != "" {
				t.Errorf("%s got an override timeout %q", c.name, g.VerifyTimeout)
			}
			if yml := g.YAML(); !strings.Contains(yml, "#  timeout:") {
				t.Errorf("%s lost the commented timeout hint:\n%s", c.name, yml)
			}
		})
	}
}
