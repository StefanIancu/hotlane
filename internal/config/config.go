// Package config loads and validates hotlane.yml, the single per-app
// configuration surface. Keep this surface small on purpose: every field
// added here is API we have to support forever.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// VerifyHook is one pre-promotion check that runs against the fork.
// Exactly one of HTTP or Run is set.
type VerifyHook struct {
	// HTTP is a check of the form "/path == 200".
	HTTP string `yaml:"http,omitempty"`
	// Run is a script executed inside the fork; exit 0 passes.
	Run string `yaml:"run,omitempty"`
}

// Config is the parsed hotlane.yml.
type Config struct {
	App     string       `yaml:"app"`
	Image   string       `yaml:"image"`
	Workdir string       `yaml:"workdir"`
	Build   string       `yaml:"build"`
	RunCmd  string       `yaml:"run"`
	Port    int          `yaml:"port"`
	Verify  []VerifyHook `yaml:"verify"`
	Ring    int          `yaml:"ring"`
	Archive string       `yaml:"archive"`
	Notify  string       `yaml:"notify"` // webhook URL for drift/rejection events (Slack/Discord compatible)
}

// Load reads and validates a hotlane.yml.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Ring == 0 {
		c.Ring = 5
	}
	if c.Workdir == "" {
		c.Workdir = "/app"
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	var problems []string
	if c.App == "" {
		problems = append(problems, "app: name is required")
	}
	if c.Image == "" {
		problems = append(problems, "image: base image is required")
	}
	if c.RunCmd == "" {
		problems = append(problems, "run: command is required")
	}
	if c.Port == 0 {
		problems = append(problems, "port: app port is required")
	}
	for i, h := range c.Verify {
		if (h.HTTP == "") == (h.Run == "") {
			problems = append(problems, fmt.Sprintf("verify[%d]: exactly one of http or run must be set", i))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}
