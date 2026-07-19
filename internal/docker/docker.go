// Package docker is a thin wrapper over the docker CLI. Shelling out keeps
// the binary small and avoids the moby module tree; the daemon only needs a
// handful of verbs, all stable CLI surface.
package docker

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// LabelApp marks containers managed by hotlane for a given app.
const LabelApp = "hotlane.app"

// LabelVersion carries the ring version number.
const LabelVersion = "hotlane.version"

func run(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("docker %s: %w: %s", args[0], err, s)
	}
	return s, nil
}

// Running returns the names of running hotlane containers for app,
// newest version first (names sort lexically only within same width, so
// callers should rely on version labels for ordering beyond v9).
func Running(app string) ([]string, error) {
	out, err := run("ps", "--filter", "label="+LabelApp+"="+app, "--format", "{{.Names}}")
	if err != nil || out == "" {
		return nil, err
	}
	names := strings.Split(out, "\n")
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// Exists reports whether a container (any state) with this name exists.
func Exists(name string) bool {
	_, err := run("inspect", "--type", "container", name)
	return err == nil
}

// IsRunning reports whether the named container is currently running.
func IsRunning(name string) bool {
	out, err := run("inspect", "-f", "{{.State.Running}}", name)
	return err == nil && out == "true"
}

// Create creates (without starting) a container for an app version. The app
// port is published on a random loopback port so the proxy can reach it
// without the container ever being exposed beyond localhost.
func Create(name, image, workdir string, port int, cmd string, labels map[string]string) error {
	args := []string{
		"create", "--name", name,
		"-w", workdir,
		"-p", fmt.Sprintf("127.0.0.1:0:%d", port),
	}
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, image, "sh", "-c", cmd)
	_, err := run(args...)
	return err
}

// CopyIn copies the contents of srcDir into the container's workdir.
func CopyIn(name, srcDir, workdir string) error {
	_, err := run("cp", srcDir+"/.", name+":"+workdir)
	return err
}

// Start starts a created or stopped container.
func Start(name string) error {
	_, err := run("start", name)
	return err
}

// Stop stops a running container.
func Stop(name string) error {
	_, err := run("stop", name)
	return err
}

// Remove force-removes a container.
func Remove(name string) error {
	_, err := run("rm", "-f", name)
	return err
}

// HostAddr returns the loopback host:port mapped to the container's app port.
func HostAddr(name string, port int) (string, error) {
	out, err := run("port", name, fmt.Sprintf("%d/tcp", port))
	if err != nil {
		return "", err
	}
	// May print one line per address family; take the first.
	addr := strings.SplitN(out, "\n", 2)[0]
	if addr == "" {
		return "", fmt.Errorf("container %s: no host mapping for port %d", name, port)
	}
	return addr, nil
}

// Label reads a single label value from a container.
func Label(name, key string) (string, error) {
	return run("inspect", "-f", fmt.Sprintf("{{index .Config.Labels %q}}", key), name)
}
