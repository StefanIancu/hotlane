// Package pool manages the warm pool: the live, running instance of an app
// that forks are taken from (M2) and traffic is routed to. M1 covers ensure:
// adopt an already-running instance after a daemon restart, or create the
// baseline from source.
package pool

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/StefanIancu/hotlane/internal/config"
	"github.com/StefanIancu/hotlane/internal/docker"
)

// Pool tracks the live version of one app.
type Pool struct {
	Cfg *config.Config
	Src string // app source directory (where hotlane.yml lives)

	Live    string // live container name
	Backend string // loopback host:port the proxy targets
	Version int
}

func containerName(app string, version int) string {
	return fmt.Sprintf("hotlane-%s-v%d", app, version)
}

// Ensure makes sure a live instance exists and is reachable: adopt a running
// hotlane container for this app if one exists (daemon restarts must not
// touch serving traffic), otherwise create the baseline from source.
func (p *Pool) Ensure() error {
	running, err := docker.Running(p.Cfg.App)
	if err != nil {
		return err
	}
	if len(running) > 0 {
		return p.adopt(running[0])
	}
	return p.createBaseline()
}

func (p *Pool) adopt(name string) error {
	addr, err := docker.HostAddr(name, p.Cfg.Port)
	if err != nil {
		return fmt.Errorf("adopting %s: %w", name, err)
	}
	version := 0
	if v, err := docker.Label(name, docker.LabelVersion); err == nil {
		version, _ = strconv.Atoi(v)
	}
	if err := waitReady(addr, 15*time.Second); err != nil {
		return fmt.Errorf("adopting %s: %w", name, err)
	}
	p.Live, p.Backend, p.Version = name, addr, version
	log.Printf("pool: adopted %s (v%d) at %s", name, version, addr)
	return nil
}

func (p *Pool) createBaseline() error {
	version := 1
	name := containerName(p.Cfg.App, version)
	if docker.Exists(name) {
		// Stopped leftover from a previous run: baseline is rebuilt from
		// source anyway, so replace it rather than resurrect stale state.
		log.Printf("pool: removing stale container %s", name)
		if err := docker.Remove(name); err != nil {
			return err
		}
	}

	log.Printf("pool: creating baseline %s from %s", name, p.Cfg.Image)
	labels := map[string]string{
		docker.LabelApp:     p.Cfg.App,
		docker.LabelVersion: strconv.Itoa(version),
	}
	if err := docker.Create(name, p.Cfg.Image, p.Cfg.Workdir, p.Cfg.Port, p.Cfg.RunCmd, labels); err != nil {
		return err
	}
	if err := docker.CopyIn(name, p.Src, p.Cfg.Workdir); err != nil {
		docker.Remove(name)
		return err
	}
	if err := docker.Start(name); err != nil {
		docker.Remove(name)
		return err
	}

	addr, err := docker.HostAddr(name, p.Cfg.Port)
	if err != nil {
		return err
	}
	if err := waitReady(addr, 60*time.Second); err != nil {
		return fmt.Errorf("baseline %s: %w", name, err)
	}
	p.Live, p.Backend, p.Version = name, addr, version
	log.Printf("pool: baseline %s ready at %s", name, addr)
	return nil
}

// waitReady polls until the backend accepts TCP connections.
func waitReady(addr string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("not accepting connections on %s after %s", addr, budget)
}
