// Package docker is a thin wrapper over the docker CLI. Shelling out keeps
// the binary small and avoids the moby module tree; the daemon only needs a
// handful of verbs, all stable CLI surface.
package docker

import (
	"context"
	"errors"
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

// verbTimeout bounds each docker verb. Nothing here may block forever:
// Fork holds the pool mutex across commit/create/start, so one wedged
// CLI would stall every deploy, rollback and status call - and `docker
// commit` PAUSES the container it snapshots, which is the LIVE one, so
// a hang there freezes production while the proxy keeps routing to it.
func verbTimeout(verb string) time.Duration {
	switch verb {
	case "build", "push", "pull":
		return 30 * time.Minute // cold builds and registry round-trips
	case "commit", "cp", "create", "start", "stop", "rm", "rmi", "tag":
		return 5 * time.Minute
	default: // ps, inspect, port, history, logs, version
		return 60 * time.Second
	}
}

func run(args ...string) (string, error) {
	budget := verbTimeout(args[0])
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return s, fmt.Errorf("docker %s: timed out after %s - the docker daemon is not responding: %s", args[0], budget, s)
	}
	if err != nil {
		return s, fmt.Errorf("docker %s: %w: %s", args[0], err, s)
	}
	return s, nil
}

// Preflight checks that Docker is present and usable before the daemon
// tries to do anything with it. Every one of these failures is somebody's
// first thirty seconds with hotlane, so each gets the fix, not a wrapped
// exec error.
func Preflight() error {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("Docker is required, but no `docker` command is on PATH.\n" +
			"  install it: https://docs.docker.com/engine/install/\n" +
			"  hotlane runs your app in containers on this machine - see https://hotlane.dev/docs#where")
	case strings.Contains(msg, "permission denied"):
		return fmt.Errorf("Docker is installed but this user cannot talk to it:\n  %s\n"+
			"  fix: sudo usermod -aG docker $USER   (then log out and back in, or run: newgrp docker)", msg)
	case strings.Contains(msg, "Cannot connect to the Docker daemon"), strings.Contains(msg, "daemon is not running"):
		return fmt.Errorf("Docker is installed but its daemon is not running:\n  %s\n"+
			"  fix: sudo systemctl start docker   (macOS: open Docker Desktop)", msg)
	default:
		return fmt.Errorf("Docker is not usable:\n  %s", msg)
	}
}

// Running returns the names of running hotlane containers for app, newest
// version first (numeric on the -v<N> suffix - lexical sort misorders v10
// vs v9). The newest running version is the authoritative live one: promote
// starts stopping its predecessor immediately, so a lower version still
// running is only mid-grace on its way down - adopting it means adopting a
// container that is about to die.
func Running(app string) ([]string, error) {
	out, err := run("ps", "--filter", "label="+LabelApp+"="+app, "--format", "{{.Names}}")
	if err != nil || out == "" {
		return nil, err
	}
	names := strings.Split(out, "\n")
	ver := func(name string) int {
		i := strings.LastIndex(name, "-v")
		if i < 0 {
			return 0
		}
		v, _ := strconv.Atoi(name[i+2:])
		return v
	}
	sort.Slice(names, func(i, j int) bool { return ver(names[i]) > ver(names[j]) })
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
	out, err := run("logs", "--tail", strconv.Itoa(n), name)
	if err != nil {
		return fmt.Sprintf("(hotlane could not read logs for %s: %v)", name, err)
	}
	return out
}

// ImageExists reports whether an image ref exists locally.
func ImageExists(ref string) bool {
	_, err := run("inspect", "--type", "image", ref)
	return err == nil
}

// LayerDepth returns the number of history entries of a container's
// image - a proxy for overlayfs layer depth, which Docker caps around
// 125. Every promoted fork adds a layer (docker commit), so long-running
// daemons under rapid pushing grow this without bound unless rebased.
func LayerDepth(container string) (int, error) {
	img, err := run("inspect", "-f", "{{.Image}}", container)
	if err != nil {
		return 0, err
	}
	return ImageLayerDepth(img)
}

// ImageLayerDepth is LayerDepth for an image ref. Used as the reference
// point for the fork chain: what matters is how far the chain has grown
// past the image it started from, not how many layers that image
// happened to have.
func ImageLayerDepth(ref string) (int, error) {
	out, err := run("history", "-q", ref)
	if err != nil {
		return 0, err
	}
	return len(strings.Split(out, "\n")), nil
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
// ContainerImageID returns the image ID a container was created from.
func ContainerImageID(name string) (string, error) {
	out, err := run("inspect", "-f", "{{.Image}}", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

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
