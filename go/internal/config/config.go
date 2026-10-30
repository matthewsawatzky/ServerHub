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
# Controls how ServerHub runs (appearance, network, terminal defaults).
# The server inventory lives in servers.conf next to this file.
# Full documentation: https://github.com/your-org/serverhub

server:
  port: 7070
  bind: "127.0.0.1"    # loopback only; use your Tailscale IP to expose to tailnet
  open_browser: true
  # insecure_ignore_host_key: false  # true disables SSH host key verification (MITM risk)

hue: 120                # accent colour (0-360) — set by the onboarding wizard / Settings
shade: 20               # surface shade 0=black … 100=white

terminal:
  background: "#0e0e10" # web terminal background
  foreground: "#e0e0e8" # web terminal default text
  accent: "#639922"     # web terminal cursor / chrome accent
`

const defaultServers = `# ServerHub server inventory
# Each entry is one remote machine. Add servers from the dashboard, or edit
# this file by hand and restart ServerHub.
#
# servers:
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

servers: []
`

const defaultSchedules = `# ServerHub schedules
# Each entry starts a detached tmux job on one or more servers. Definitions
# are managed from the Schedules page or edited here and reloaded on restart.
# Runtime history lives in schedules-state.json.
#
# schedules:
#   - id: nightly-updates
#     name: "Nightly updates"
#     enabled: true
#     targets: [web-1, web-2]      # explicit server IDs
#     tags: [prod]                 # optional union with targets
#     quick_action: pull-images    # stable quick-action ID...
#     # command: "..."             # ...or a raw shell command
#     every: 24h
#     at: "03:30"                  # optional local time-of-day
#     overlap: skip                # skip (default) | allow
#     timeout: 30m

schedules: []
`

// Dir returns the directory ServerHub's state lives in (see storage.Dir).
// Dev builds (./dev) override it with the repo's ./test-config directory.
func Dir() (string, error) { return storage.Dir() }

// DefaultPath returns the canonical path to config.yaml inside Dir().
func DefaultPath() (string, error) { return storage.DefaultPath() }

// EnsureDefault creates the state dir and writes starter config.yaml,
// servers.conf, and schedules.conf files if they do not already exist.
// Returns (path, created, error). created is true only on the first run.
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

	serversPath := ServersPath(path)
	if _, err := os.Stat(serversPath); os.IsNotExist(err) {
		if err := storage.Write(serversPath, []byte(defaultServers)); err != nil {
			return "", false, fmt.Errorf("writing default servers: %w", err)
		}
	}

	schedulesPath := SchedulesPath(path)
	if _, err := os.Stat(schedulesPath); os.IsNotExist(err) {
		if err := storage.Write(schedulesPath, []byte(defaultSchedules)); err != nil {
			return "", false, fmt.Errorf("writing default schedules: %w", err)
		}
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := storage.Write(path, []byte(defaultConfig)); err != nil {
			return "", false, fmt.Errorf("writing default config: %w", err)
		}
		return path, true, nil
	}
	return path, false, nil
}

// ServersPath returns the inventory file that sits next to configPath.
func ServersPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "servers.conf")
}

// SchedulesPath returns the schedule definitions file next to configPath.
func SchedulesPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "schedules.conf")
}

// serversFile is the on-disk shape of servers.conf.
type serversFile struct {
	Servers []Host `yaml:"servers"`
}

// diskConfig is config.yaml as read from disk. Servers is only present for
// legacy installs (pre-split); it is migrated into servers.conf on load.
type diskConfig struct {
	Config  `yaml:",inline"`
	Servers []Host `yaml:"servers,omitempty"`
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
	ID         string `yaml:"id,omitempty"          json:"id,omitempty"` // stable identity (derived from label when absent)
	Label      string `yaml:"label"                 json:"label"`
	Command    string `yaml:"command"               json:"command"`
	Mode       string `yaml:"mode,omitempty"        json:"mode,omitempty"`        // "" (shell) | "detached"
	Session    string `yaml:"session,omitempty"     json:"session,omitempty"`     // fixed detached tmux session suffix
	LastRan    int64  `yaml:"last_ran,omitempty"    json:"last_ran,omitempty"`    // unix seconds
	LastStatus string `yaml:"last_status,omitempty" json:"last_status,omitempty"` // success | error | stopped | started
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
	Favorite     *bool           `yaml:"favorite,omitempty" json:"favorite,omitempty"`
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
	Favorite     *bool         `yaml:"favorite,omitempty" json:"favorite,omitempty"`
	QuickActions []QuickAction `yaml:"quick_actions" json:"quick_actions"`
}

// App is a user-defined app link shown in the dashboard and app library.
type App struct {
	ID         string `yaml:"id" json:"id"`
	Name       string `yaml:"name" json:"name"`
	URL        string `yaml:"url" json:"url"`
	Logo       string `yaml:"logo,omitempty" json:"logo,omitempty"`               // stored media filename
	LogoSource string `yaml:"logo_source,omitempty" json:"logo_source,omitempty"` // original remote URL
	Favorite   bool   `yaml:"favorite,omitempty" json:"favorite,omitempty"`
}

type Config struct {
	Server                 ServerConfig    `yaml:"server"         json:"server"`
	Hue                    int             `yaml:"hue"            json:"hue"`
	Shade                  int             `yaml:"shade"          json:"shade"`
	Animations             *bool           `yaml:"animations"     json:"animations"` // nil means true (default on)
	Terminal               TerminalConfig  `yaml:"terminal"       json:"terminal"`   // global web-terminal colours
	LocalTerminal          *TerminalConfig `yaml:"local_terminal,omitempty" json:"local_terminal,omitempty"`
	Owner                  string          `yaml:"owner"           json:"owner"`                     // the user's name, set during onboarding (greeting)
	SizeUnits              string          `yaml:"size_units,omitempty" json:"size_units,omitempty"` // GB (decimal) or GiB (binary)
	BackgroundImage        string          `yaml:"background_image,omitempty" json:"background_image,omitempty"`
	BackgroundBlur         int             `yaml:"background_blur,omitempty"  json:"background_blur,omitempty"`
	BackgroundColor        string          `yaml:"background_color,omitempty" json:"background_color,omitempty"`
	BackgroundColorEnabled *bool           `yaml:"background_color_enabled,omitempty" json:"background_color_enabled,omitempty"`
	SurfaceColor           string          `yaml:"surface_color,omitempty"    json:"surface_color,omitempty"`
	SurfaceColorEnabled    *bool           `yaml:"surface_color_enabled,omitempty" json:"surface_color_enabled,omitempty"`
	SurfaceShade           *int            `yaml:"surface_shade,omitempty"    json:"surface_shade,omitempty"`
	DashboardOrder         []string        `yaml:"dashboard_order,omitempty"  json:"dashboard_order,omitempty"`
	DashboardHidden        []string        `yaml:"dashboard_hidden,omitempty" json:"dashboard_hidden,omitempty"`
	DashboardDays          *bool           `yaml:"dashboard_days,omitempty"   json:"dashboard_days,omitempty"`
	Apps                   []App           `yaml:"apps,omitempty" json:"apps,omitempty"`
	Local                  LocalConfig     `yaml:"local"          json:"local"`
	LocalName              string          `yaml:"local_name,omitempty" json:"local_name,omitempty"` // legacy; migrated into Local.Name
	// Servers is persisted in servers.conf, not config.yaml (hence yaml:"-").
	Servers []Host `yaml:"-" json:"servers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	// The config file holds hostnames, users and quick-action commands (and
	// servers.conf holds the rest); correct a group/world-readable file and
	// warn (audit V10).
	if changed, err := storage.FixPermissions(path); err == nil && changed {
		log.Printf("warning: %s was group/world-readable — correcting to 0600", path)
	} else if err != nil && !os.IsNotExist(err) {
		log.Printf("warning: could not check permissions on %s: %v", path, err)
	}

	var disk diskConfig
	if err := yaml.Unmarshal(data, &disk); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg := disk.Config
	legacy := disk.Servers

	serversPath := ServersPath(path)
	fileServers, haveServersFile, err := loadServersFile(serversPath)
	if err != nil {
		return nil, err
	}

	// Legacy installs kept the inventory inside config.yaml. Migrate it once,
	// preferring servers.conf if both exist.
	migrate := false
	switch {
	case len(legacy) > 0 && !haveServersFile:
		cfg.Servers = legacy
		migrate = true
		log.Printf("migrating %d server(s) from %s to %s", len(legacy), path, serversPath)
	case len(legacy) > 0 && haveServersFile:
		cfg.Servers = fileServers
		migrate = true
		log.Printf("warning: %s still contains a servers block — using %s and removing the legacy copy", path, serversPath)
	default:
		cfg.Servers = fileServers
	}

	// Normalise before validation/serialisation.
	for i := range cfg.Servers {
		cfg.Servers[i].Key = expandHome(cfg.Servers[i].Key)
		if cfg.Servers[i].Port == 0 {
			cfg.Servers[i].Port = 22
		}
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if migrate {
		if err := SaveAll(path, &cfg); err != nil {
			return nil, fmt.Errorf("migrating server inventory: %w", err)
		}
	}

	return &cfg, nil
}

// loadServersFile reads servers.conf. The second return value reports whether
// the file exists (an empty file still counts as authoritative).
func loadServersFile(path string) ([]Host, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading servers: %w", err)
	}
	if changed, err := storage.FixPermissions(path); err == nil && changed {
		log.Printf("warning: %s was group/world-readable — correcting to 0600", path)
	}
	var f serversFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, true, fmt.Errorf("parsing servers: %w", err)
	}
	return f.Servers, true, nil
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
	if cfg.Local.Favorite == nil {
		t := true // the local machine is favorited by default
		cfg.Local.Favorite = &t
	}
	cfg.Local.QuickActions = NormalizeQuickActions(cfg.Local.QuickActions, false)
	for i := range cfg.Servers {
		cfg.Servers[i].QuickActions = NormalizeQuickActions(cfg.Servers[i].QuickActions, true)
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
		if s.Port < 0 || s.Port > 65535 {
			return fmt.Errorf("server %q: port must be between 1 and 65535", s.ID)
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

// NormalizeQuickActions trims entries, derives stable IDs for actions that
// don't have one, de-duplicates IDs, and validates the mode/session pair.
// allowDetached is false for the local machine, which has no tmux jobs.
func NormalizeQuickActions(list []QuickAction, allowDetached bool) []QuickAction {
	out := make([]QuickAction, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, qa := range list {
		qa.Label = strings.TrimSpace(qa.Label)
		qa.Command = strings.TrimSpace(qa.Command)
		if qa.Label == "" || qa.Command == "" {
			continue
		}

		mode := strings.ToLower(strings.TrimSpace(qa.Mode))
		if mode != "detached" || !allowDetached {
			mode = ""
		}
		qa.Mode = mode
		qa.Session = strings.TrimSpace(qa.Session)
		if qa.Mode != "detached" || ValidateID(qa.Session) != nil {
			qa.Session = ""
		}

		id := strings.TrimSpace(qa.ID)
		if ValidateID(id) != nil {
			id = Slugify(id)
			if ValidateID(id) != nil {
				id = Slugify(qa.Label)
			}
			if ValidateID(id) != nil {
				id = "action"
			}
		}
		base := id
		for n := 2; seen[id] && n < 10000; n++ {
			suffix := fmt.Sprintf("-%d", n)
			prefix := base
			if len(prefix) > maxIDLength-len(suffix) {
				prefix = prefix[:maxIDLength-len(suffix)]
			}
			prefix = strings.TrimRight(prefix, "-")
			if prefix == "" {
				prefix = "action"
			}
			id = prefix + suffix
		}
		qa.ID = id
		seen[id] = true
		out = append(out, qa)
	}
	return out
}

// Save writes the app settings in cfg to config.yaml (atomic, mode 0600).
// The server inventory is not included — use SaveServers or SaveAll.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	header := []byte("# ServerHub configuration — managed by the UI\n# Server inventory lives in servers.conf.\n# Manual edits are supported; restart ServerHub to reload.\n\n")
	return storage.Write(path, append(header, data...))
}

// SaveServers writes the inventory to servers.conf next to configPath.
func SaveServers(configPath string, servers []Host) error {
	if servers == nil {
		servers = []Host{}
	}
	data, err := yaml.Marshal(serversFile{Servers: servers})
	if err != nil {
		return fmt.Errorf("marshaling servers: %w", err)
	}
	header := []byte("# ServerHub server inventory — managed by the UI\n# Manual edits are supported; restart ServerHub to reload.\n\n")
	return storage.Write(ServersPath(configPath), append(header, data...))
}

// SaveAll writes both the app settings and the server inventory. The inventory
// goes first so a legacy config.yaml is never stripped before its servers have
// been persisted elsewhere; if the second write fails, servers.conf is restored
// so the two files do not disagree.
func SaveAll(path string, cfg *Config) error {
	serversPath := ServersPath(path)
	previous, readErr := os.ReadFile(serversPath)
	hadServersFile := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("reading servers for rollback: %w", readErr)
	}
	if err := SaveServers(path, cfg.Servers); err != nil {
		return err
	}
	if err := Save(path, cfg); err != nil {
		if hadServersFile {
			if restoreErr := storage.Write(serversPath, previous); restoreErr != nil {
				log.Printf("warning: could not restore %s after failed config save: %v", serversPath, restoreErr)
			}
		} else if removeErr := os.Remove(serversPath); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Printf("warning: could not remove %s after failed config save: %v", serversPath, removeErr)
		}
		return err
	}
	return nil
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
