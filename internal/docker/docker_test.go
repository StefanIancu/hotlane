package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeDocker puts a `docker` on PATH that prints msg to stderr and fails.
// An empty msg means: no docker binary at all.
func fakeDocker(t *testing.T, msg string) {
	t.Helper()
	dir := t.TempDir()
	if msg != "" {
		script := "#!/bin/sh\necho " + strconv.Quote(msg) + " >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// Preflight's whole job is the first thirty seconds of somebody's first
// session: every failure must name the fix, not leak an exec error.
func TestPreflightDiagnostics(t *testing.T) {
	cases := []struct {
		name, dockerSays string
		want             []string
	}{
		{"missing", "", []string{"no `docker` command is on PATH", "docs.docker.com/engine/install"}},
		{"daemon down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			[]string{"daemon is not running", "systemctl start docker", "Docker Desktop"}},
		{"permission", "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock",
			[]string{"cannot talk to it", "usermod -aG docker"}},
		{"unknown", "something else went wrong", []string{"not usable", "something else went wrong"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeDocker(t, c.dockerSays)
			err := Preflight()
			if err == nil {
				t.Fatal("want an error")
			}
			for _, frag := range c.want {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("message missing %q:\n%v", frag, err)
				}
			}
			if strings.Contains(err.Error(), "exec:") || strings.Contains(err.Error(), "exit status") {
				t.Errorf("raw exec plumbing leaked into the message:\n%v", err)
			}
		})
	}
}

func TestPreflightPassesWithWorkingDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on this machine")
	}
	if err := Preflight(); err != nil {
		t.Skipf("docker present but not usable here: %v", err)
	}
}
