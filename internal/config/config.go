// Package config loads and validates hotlane.yml, the single per-app
// configuration surface. Keep this surface small on purpose: every field
// added here is API we have to support forever.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like
// "5s" or "2m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf(`want a duration like "5s"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf(`%q is not a duration like "5s"`, s)
	}
	*d = Duration(v)
	return nil
}

// VerifyHook is one pre-promotion check that runs against the fork.
// Exactly one of HTTP or Run is set.
type VerifyHook struct {
	// HTTP is a check of the form "/path == 200".
	HTTP string `yaml:"http,omitempty"`
	// Run is a script executed inside the fork; exit 0 passes.
	Run string `yaml:"run,omitempty"`
	// Timeout caps this hook's budget. Zero means the built-in default
	// (15s for http, 60s for run).
	Timeout Duration `yaml:"timeout,omitempty"`
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
	if err := c.interpolate(); err != nil {
		return nil, err
	}
	return &c, c.validate()
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolate expands ${VAR} from the daemon's environment in the fields
// that carry endpoints or credentials (notify, archive), so secrets never
// have to live in a committed hotlane.yml. Build/run/verify scripts are
// left untouched - their ${VAR}s belong to the shell inside the container.
// An unset variable is a hard error: a webhook that silently expands to ""
// is a notification channel that silently doesn't exist.
func (c *Config) interpolate() error {
	var missing []string
	expand := func(s string) string {
		return envRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := envRef.FindStringSubmatch(ref)[1]
			v, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
	}
	c.Notify = expand(c.Notify)
	c.Archive = expand(c.Archive)
	if len(missing) > 0 {
		return fmt.Errorf("hotlane.yml references unset environment variable(s): %s", strings.Join(missing, ", "))
	}
	return nil
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
		if h.Timeout < 0 {
			problems = append(problems, fmt.Sprintf("verify[%d]: timeout must be positive", i))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}
