// Package docker is a thin wrapper over the docker CLI. Shelling out keeps
// the binary small and avoids the moby module tree; the daemon only needs a
// handful of verbs, all stable CLI surface.
package docker

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
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

// Running returns the names of running hotlane containers for app, oldest
// version first. Until promote lands (M3) the live instance is always the
// lowest running version: forks boot at higher versions and stay side-lined.
func Running(app string) ([]string, error) {
	out, err := run("ps", "--filter", "label="+LabelApp+"="+app, "--format", "{{.Names}}")
	if err != nil || out == "" {
		return nil, err
	}
	names := strings.Split(out, "\n")
	sort.Strings(names)
	return names, nil
}

// ContainerInfo is one hotlane container in any state.
type ContainerInfo struct {
	Name   string
	Status string // docker's human status, e.g. "Up 5 minutes", "Exited (137) ..."
}

// All returns hotlane containers for app in any state.
func All(app string) ([]ContainerInfo, error) {
	out, err := run("ps", "-a", "--filter", "label="+LabelApp+"="+app, "--format", "{{.Names}}\t{{.Status}}")
	if err != nil || out == "" {
		return nil, err
	}
	var infos []ContainerInfo
	for _, line := range strings.Split(out, "\n") {
		name, status, _ := strings.Cut(line, "\t")
		infos = append(infos, ContainerInfo{Name: name, Status: status})
	}
	return infos, nil
}

// Commit snapshots a container's filesystem into an image. Running
// containers are paused briefly; this is what makes forks inherit the warm
// state (dependencies, build caches) for free.
func Commit(name, imageRef string) error {
	_, err := run("commit", name, imageRef)
	return err
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

// Stop stops a running container, giving the app graceSecs to exit cleanly.
func Stop(name string, graceSecs int) error {
	_, err := run("stop", "-t", strconv.Itoa(graceSecs), name)
	return err
}

// Kill force-kills a container immediately (best effort; it may already
// have exited).
func Kill(name string) {
	run("kill", name)
}

// Exec runs a shell command inside a running container and returns its
// combined output. The command is killed at the timeout.
func Exec(name, workdir, cmd string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", "-w", workdir, name, "sh", "-c", cmd).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return s, fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		return s, fmt.Errorf("exit: %w", err)
	}
	return s, nil
}

// Logs returns the last n lines of a container's output (best effort).
func Logs(name string, n int) string {
	out, _ := run("logs", "--tail", strconv.Itoa(n), name)
	return out
}

// RemoveImage force-removes an image (best effort cleanup of fork
// snapshots; discarded forks must not stack images on disk).
func RemoveImage(imageRef string) error {
	_, err := run("rmi", "-f", imageRef)
	return err
}

// Build builds an image from a context directory (expects a Dockerfile
// inside it). Clean builds are minutes-class by design - they run off the
// critical path.
func Build(dir, tag string) error {
	_, err := run("build", "-t", tag, dir)
	return err
}

// TagImage adds a tag to an existing image.
func TagImage(src, dst string) error {
	_, err := run("tag", src, dst)
	return err
}

// Push pushes an image ref to its registry (requires prior docker login).
func Push(ref string) error {
	_, err := run("push", ref)
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
