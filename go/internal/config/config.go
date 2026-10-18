package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"serverhub/internal/storage"
)

const defaultConfig = `# ServerHub configuration
# Edit this file to add your servers, then restart ServerHub.
# Full documentation: https://github.com/your-org/serverhub

server:
  port: 7070
  bind: "127.0.0.1"    # loopback only; use your Tailscale IP to expose to tailnet
  open_browser: true
  # insecure_ignore_host_key: false  # true disables SSH host key verification (MITM risk)

theme: "green"          # green | blue | purple | coral
hue: 120                # accent colour (0-360) — set by the onboarding wizard / Settings
shade: 20               # surface shade 0=black … 100=white

terminal:
  background: "#0e0e10" # web terminal background
  foreground: "#e0e0e8" # web terminal default text
  accent: "#639922"     # web terminal cursor / chrome accent

servers: []
# Add servers like this:
#
#   - id: myserver
#     name: "myserver"
#     host: "192.168.1.10"
#     port: 22
#     user: "youruser"
#     key: "~/.ssh/id_ed25519"
#     tags: ["homelab"]
#     notes: "Optional notes"
#     quick_actions:
#       - label: "Restart nginx"
#         command: "sudo systemctl restart nginx"
`

// Dir returns the directory ServerHub's state lives in (see storage.Dir).
// Dev builds (./dev) override it with the repo's ./test-config directory.
func Dir() (string, error) { return storage.Dir() }

// DefaultPath returns the canonical path to config.yaml inside Dir().
func DefaultPath() (string, error) { return storage.DefaultPath() }

// EnsureDefault creates the state dir and writes a starter config.yaml if one
// does not already exist. Returns (path, created, error). created is true only
// on first run.
func EnsureDefault() (path string, created bool, err error) {
	dir, err := storage.Dir()
	if err != nil {
		return "", false, err
	}
	// The config dir holds the server list (hostnames, users, key paths,
	// quick-action commands) — keep it private to the user (0700).
	if err := storage.EnsureDir(dir); err != nil {
		return "", false, fmt.Errorf("creating config directory %s: %w", dir, err)
	}
	path, err = storage.DefaultPath()
	if err != nil {
		return "", false, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := storage.Write(path, []byte(defaultConfig)); err != nil {
			return "", false, fmt.Errorf("writing default config: %w", err)
		}
		return path, true, nil
	}
	return path, false, nil
}

type ServerConfig struct {
	WebPort     int    `yaml:"port"         json:"port"`
	Bind        string `yaml:"bind"         json:"bind"`
	OpenBrowser bool   `yaml:"open_browser" json:"open_browser"`
	// InsecureIgnoreHostKey disables SSH host key verification. Default is
	// false — verified connections use TOFU against <config dir>/known_hosts.
	InsecureIgnoreHostKey bool `yaml:"insecure_ignore_host_key" json:"insecure_ignore_host_key"`
}

type QuickAction struct {
	Label      string `yaml:"label"                 json:"label"`
	Command    string `yaml:"command"               json:"command"`
	LastRan    int64  `yaml:"last_ran,omitempty"    json:"last_ran,omitempty"`    // unix seconds
	LastStatus string `yaml:"last_status,omitempty" json:"last_status,omitempty"` // success | error
}

type Host struct {
	ID           string          `yaml:"id"            json:"id"`
	Name         string          `yaml:"name"          json:"name"`
	Host         string          `yaml:"host"          json:"host"`
	Port         int             `yaml:"port"          json:"port"`
	User         string          `yaml:"user"          json:"user"`
	Key          string          `yaml:"key"           json:"key"`
	Tags         []string        `yaml:"tags"          json:"tags"`
	Notes        string          `yaml:"notes"         json:"notes"`
	QuickActions []QuickAction   `yaml:"quick_actions" json:"quick_actions"`
	Terminal     *TerminalConfig `yaml:"terminal,omitempty" json:"terminal,omitempty"` // per-server colour override
}

type TerminalConfig struct {
	Background string `yaml:"background,omitempty" json:"background,omitempty"` // xterm background
	Foreground string `yaml:"foreground,omitempty" json:"foreground,omitempty"` // xterm default text
	Accent     string `yaml:"accent,omitempty"     json:"accent,omitempty"`     // cursor + terminal chrome
}

// LocalConfig holds the same user-editable fields remote servers have, minus
// the SSH connection details (the local machine needs none).
type LocalConfig struct {
	Name         string        `yaml:"name"          json:"name"`
	Tags         []string      `yaml:"tags"          json:"tags"`
	Notes        string        `yaml:"notes"         json:"notes"`
	QuickActions []QuickAction `yaml:"quick_actions" json:"quick_actions"`
}

type Config struct {
	Server        ServerConfig    `yaml:"server"         json:"server"`
	Hue           int             `yaml:"hue"            json:"hue"`
	Shade         int             `yaml:"shade"          json:"shade"`
	Animations    *bool           `yaml:"animations"     json:"animations"` // nil means true (default on)
	Terminal      TerminalConfig  `yaml:"terminal"       json:"terminal"`   // global web-terminal colours
	LocalTerminal *TerminalConfig `yaml:"local_terminal,omitempty" json:"local_terminal,omitempty"`
	Owner         string          `yaml:"owner"          json:"owner"` // the user's name, set during onboarding (greeting)
	Local         LocalConfig     `yaml:"local"          json:"local"`
	LocalName     string          `yaml:"local_name,omitempty" json:"local_name,omitempty"` // legacy; migrated into Local.Name
	Servers       []Host          `yaml:"servers"        json:"servers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	// The file holds hostnames, users and quick-action commands; correct a
	// group/world-readable file and warn (audit V10).
	if changed, err := storage.FixPermissions(path); err == nil && changed {
		log.Printf("warning: %s was group/world-readable — correcting to 0600", path)
	} else if err != nil && !os.IsNotExist(err) {
		log.Printf("warning: could not check permissions on %s: %v", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Expand ~ in key paths
	for i := range cfg.Servers {
		cfg.Servers[i].Key = expandHome(cfg.Servers[i].Key)
		if cfg.Servers[i].Port == 0 {
			cfg.Servers[i].Port = 22
		}
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	if cfg.Server.WebPort == 0 {
		cfg.Server.WebPort = 7070
	}
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "127.0.0.1"
	}
	if cfg.Hue < 0 || cfg.Hue > 360 {
		cfg.Hue = 120
	}
	if cfg.Shade < 0 || cfg.Shade > 100 {
		cfg.Shade = 20
	}
	// Legacy configs (pre-colour-model) had no hue/shade fields at all — 0/0
	// means "unset" (a deliberate red-on-black choice would also set one of
	// them explicitly via the UI).
	if cfg.Hue == 0 && cfg.Shade == 0 {
		cfg.Hue = 120
		cfg.Shade = 20
	}
	if !ValidHexColor(cfg.Terminal.Background) {
		cfg.Terminal.Background = DefaultTerminalBackground
	}
	if !ValidHexColor(cfg.Terminal.Foreground) {
		cfg.Terminal.Foreground = DefaultTerminalForeground
	}
	if !ValidHexColor(cfg.Terminal.Accent) {
		cfg.Terminal.Accent = DefaultTerminalAccent
	}
	// Per-machine overrides are optional: empty fields inherit the global
	// colours, so only non-empty invalid values are cleared.
	validateTerminalOverride(cfg.LocalTerminal)
	for i := range cfg.Servers {
		validateTerminalOverride(cfg.Servers[i].Terminal)
	}

	// Migrate the old local_name field into the local machine block.
	if cfg.Local.Name == "" {
		cfg.Local.Name = cfg.LocalName
	}
	cfg.LocalName = ""
	if cfg.Local.Tags == nil {
		cfg.Local.Tags = []string{}
	}
	if cfg.Local.QuickActions == nil {
		cfg.Local.QuickActions = []QuickAction{}
	}

	ids := make(map[string]bool)
	for i, s := range cfg.Servers {
		if s.ID == "" {
			return fmt.Errorf("servers[%d]: id is required", i)
		}
		if err := ValidateID(s.ID); err != nil {
			return fmt.Errorf("servers[%d]: invalid id %q: %v", i, s.ID, err)
		}
		if s.Host == "" {
			return fmt.Errorf("server %q: host is required", s.ID)
		}
		if s.User == "" {
			return fmt.Errorf("server %q: user is required", s.ID)
		}
		if ids[s.ID] {
			return fmt.Errorf("duplicate server id %q", s.ID)
		}
		ids[s.ID] = true
	}
	return nil
}

// Default web-terminal colours.
const (
	DefaultTerminalBackground = "#0e0e10"
	DefaultTerminalForeground = "#e0e0e8"
	DefaultTerminalAccent     = "#639922"
)

// hexColorPattern matches #rgb / #rrggbb — the only values accepted for
// terminal colours (they are injected into CSS and xterm theme options).
var hexColorPattern = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// ValidHexColor reports whether s is a safe CSS hex colour.
func ValidHexColor(s string) bool { return hexColorPattern.MatchString(s) }

// validateTerminalOverride clears invalid non-empty colours, leaving blanks
// as "inherit from the global terminal colours".
func validateTerminalOverride(t *TerminalConfig) {
	if t == nil {
		return
	}
	if t.Background != "" && !ValidHexColor(t.Background) {
		t.Background = ""
	}
	if t.Foreground != "" && !ValidHexColor(t.Foreground) {
		t.Foreground = ""
	}
	if t.Accent != "" && !ValidHexColor(t.Accent) {
		t.Accent = ""
	}
}

// idPattern restricts server IDs to a URL- and HTML-safe charset: they are
// interpolated into element ids, data-id attributes and /terminal/<id> URLs.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// maxIDLength keeps IDs practical in element ids and URLs.
const maxIDLength = 64

// ValidateID enforces the server ID charset. It rejects the reserved
// __local__ sentinel (which would collide with the local-machine card).
func ValidateID(id string) error {
	if id == "__local__" {
		return fmt.Errorf("__local__ is reserved for the local machine")
	}
	if len(id) > maxIDLength {
		return fmt.Errorf("max length is %d characters", maxIDLength)
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("must match %s (lowercase letters, digits, hyphens)", idPattern.String())
	}
	return nil
}

// Save writes cfg back to path in YAML format (atomic, mode 0600).
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	header := []byte("# ServerHub configuration — managed by the UI\n# Manual edits are supported; restart ServerHub to reload.\n\n")
	return storage.Write(path, append(header, data...))
}

// Slugify converts a display name to a safe server ID.
func Slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// ExpandHomePath expands a leading ~ to the user's home directory.
func ExpandHomePath(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

func expandHome(p string) string { return ExpandHomePath(p) }
