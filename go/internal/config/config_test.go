package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateID(t *testing.T) {
	for _, id := range []string{"web-1", "a", "server-01", strings.Repeat("a", maxIDLength)} {
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{
		"",
		"__local__",
		"UPPER",
		"with_underscore",
		"with space",
		"-leading",
		strings.Repeat("a", maxIDLength+1),
	} {
		if err := ValidateID(id); err == nil {
			t.Errorf("ValidateID(%q) = nil, want error", id)
		}
	}
}

func TestValidHexColor(t *testing.T) {
	for _, c := range []string{"#fff", "#ffffff", "#Aa09Ff"} {
		if !ValidHexColor(c) {
			t.Errorf("ValidHexColor(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"", "fff", "#ff", "#ffff", "#gggggg", "red", "#fff; background:url(x)"} {
		if ValidHexColor(c) {
			t.Errorf("ValidHexColor(%q) = true, want false", c)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Web Server 1":  "web-server-1",
		"  Homelab  ":   "homelab",
		"UPPER_case!!":  "upper-case",
		"already-valid": "already-valid",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "missing host",
			yaml: "servers:\n  - id: web\n    user: ops\n",
		},
		{
			name: "missing user",
			yaml: "servers:\n  - id: web\n    host: 10.0.0.2\n",
		},
		{
			name: "duplicate id",
			yaml: "servers:\n  - id: web\n    host: 10.0.0.2\n    user: ops\n  - id: web\n    host: 10.0.0.3\n    user: ops\n",
		},
		{
			name: "reserved id",
			yaml: "servers:\n  - id: __local__\n    host: 10.0.0.2\n    user: ops\n",
		},
		{
			name: "invalid id charset",
			yaml: "servers:\n  - id: Web Server\n    host: 10.0.0.2\n    user: ops\n",
		},
		{
			name: "port out of range",
			yaml: "servers:\n  - id: web\n    host: 10.0.0.2\n    user: ops\n    port: 70000\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tc.yaml)); err == nil {
				t.Fatal("Load succeeded, want validation error")
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	path := writeConfig(t, "servers:\n  - id: web\n    host: 10.0.0.2\n    user: ops\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.WebPort != 7070 {
		t.Errorf("WebPort = %d, want 7070", cfg.Server.WebPort)
	}
	if cfg.Server.Bind != "127.0.0.1" {
		t.Errorf("Bind = %q, want 127.0.0.1", cfg.Server.Bind)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Port != 22 {
		t.Errorf("servers = %+v, want one server on port 22", cfg.Servers)
	}
	if cfg.Local.Tags == nil || cfg.Local.QuickActions == nil {
		t.Error("local lists should be initialised empty, not nil")
	}
	if !ValidHexColor(cfg.Terminal.Background) || !ValidHexColor(cfg.Terminal.Foreground) || !ValidHexColor(cfg.Terminal.Accent) {
		t.Error("terminal colours should default to valid hex values")
	}
	if _, err := os.Stat(ServersPath(path)); err != nil {
		t.Errorf("legacy load should create servers.conf: %v", err)
	}
}

func TestLegacyServersMigrateOutOfConfig(t *testing.T) {
	path := writeConfig(t, "hue: 200\nservers:\n  - id: web\n    host: 10.0.0.2\n    user: ops\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].ID != "web" {
		t.Fatalf("servers = %+v, want the legacy server", cfg.Servers)
	}

	configData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(configData), "servers:") || strings.Contains(string(configData), "id: web") {
		t.Errorf("legacy servers still present in config.yaml:\n%s", configData)
	}

	serversData, err := os.ReadFile(ServersPath(path))
	if err != nil {
		t.Fatalf("read servers.conf: %v", err)
	}
	if !strings.Contains(string(serversData), "servers:") || !strings.Contains(string(serversData), "id: web") {
		t.Errorf("servers.conf missing migrated inventory:\n%s", serversData)
	}

	// A second load must not duplicate or rewrite the inventory.
	again, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if len(again.Servers) != 1 || again.Servers[0].ID != "web" {
		t.Errorf("second load servers = %+v", again.Servers)
	}
}

func TestServersFileWinsOverLegacyBlock(t *testing.T) {
	path := writeConfig(t, "servers:\n  - id: legacy\n    host: 10.0.0.2\n    user: ops\n")
	if err := SaveServers(path, []Host{{ID: "current", Name: "Current", Host: "10.0.0.3", Port: 22, User: "ops"}}); err != nil {
		t.Fatalf("SaveServers: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].ID != "current" {
		t.Fatalf("servers = %+v, want servers.conf to win", cfg.Servers)
	}
	configData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(configData), "id: legacy") {
		t.Errorf("legacy block was not stripped:\n%s", configData)
	}
}

func TestInvalidLegacyServersAreNotMigrated(t *testing.T) {
	path := writeConfig(t, "servers:\n  - id: __local__\n    host: 10.0.0.2\n    user: ops\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded, want validation error")
	}
	if _, err := os.Stat(ServersPath(path)); !os.IsNotExist(err) {
		t.Error("invalid legacy servers should not be written to servers.conf")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{
		Server: ServerConfig{WebPort: 7071, Bind: "127.0.0.1"},
		Hue:    200,
		Shade:  30,
		Owner:  "matthew",
		Local: LocalConfig{
			Name:         "studio",
			Tags:         []string{"local"},
			QuickActions: []QuickAction{{Label: "df", Command: "df -h"}},
		},
		Servers: []Host{{
			ID:    "web-1",
			Name:  "Web 1",
			Host:  "10.0.0.2",
			Port:  2222,
			User:  "ops",
			Key:   "~/.ssh/id_ed25519",
			Tags:  []string{"prod"},
			Notes: "primary",
			Terminal: &TerminalConfig{
				Background: "#000000",
			},
		}},
	}

	if err := SaveAll(path, cfg); err != nil {
		t.Fatalf("SaveAll: %v", err)
	}

	configData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(configData), "servers:") || strings.Contains(string(configData), "web-1") {
		t.Errorf("config.yaml should not contain the inventory:\n%s", configData)
	}
	serversData, err := os.ReadFile(ServersPath(path))
	if err != nil {
		t.Fatalf("read servers.conf: %v", err)
	}
	if !strings.Contains(string(serversData), "servers:") || !strings.Contains(string(serversData), "web-1") {
		t.Errorf("servers.conf should contain the inventory:\n%s", serversData)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Server.WebPort != 7071 || got.Hue != 200 || got.Shade != 30 || got.Owner != "matthew" {
		t.Errorf("top-level fields did not round-trip: %+v", got)
	}
	if len(got.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(got.Servers))
	}
	s := got.Servers[0]
	if s.ID != "web-1" || s.Host != "10.0.0.2" || s.Port != 2222 || s.User != "ops" {
		t.Errorf("server fields did not round-trip: %+v", s)
	}
	if s.Key == "~/.ssh/id_ed25519" || !strings.HasSuffix(s.Key, "/.ssh/id_ed25519") {
		t.Errorf("key path was not expanded on load: %q", s.Key)
	}
	if s.Terminal == nil || s.Terminal.Background != "#000000" {
		t.Errorf("terminal override did not round-trip: %+v", s.Terminal)
	}
	if got.Local.Name != "studio" || len(got.Local.QuickActions) != 1 {
		t.Errorf("local config did not round-trip: %+v", got.Local)
	}
}

func TestNormalizeQuickActions(t *testing.T) {
	in := []QuickAction{
		{Label: "Restart nginx", Command: " systemctl restart nginx "},
		{Label: "Restart nginx", Command: "systemctl restart nginx --force"},
		{Label: "Nightly", Command: "true", Mode: "detached", Session: "nightly"},
		{Label: "Bad session", Command: "true", Mode: "detached", Session: "Bad_Session"},
		{Label: "Bad id", Command: "true", ID: "Bad_ID"},
		{Label: "", Command: "ignored"},
		{Label: "No command", Command: " "},
	}
	got := NormalizeQuickActions(in, true)
	if len(got) != 5 {
		t.Fatalf("got %d actions, want 5: %+v", len(got), got)
	}
	if got[0].ID != "restart-nginx" || got[0].Command != "systemctl restart nginx" {
		t.Errorf("first action = %+v", got[0])
	}
	if got[1].ID != "restart-nginx-2" {
		t.Errorf("duplicate id was not de-duplicated: %q", got[1].ID)
	}
	if got[2].ID != "nightly" || got[2].Mode != "detached" || got[2].Session != "nightly" {
		t.Errorf("detached action = %+v", got[2])
	}
	if got[3].Mode != "detached" || got[3].Session != "" {
		t.Errorf("invalid session should be cleared: %+v", got[3])
	}
	if got[4].ID != "bad-id" {
		t.Errorf("invalid id should be sanitized: %q", got[4].ID)
	}

	local := NormalizeQuickActions([]QuickAction{
		{Label: "Local detach", Command: "true", Mode: "detached", Session: "svc"},
	}, false)
	if len(local) != 1 || local[0].Mode != "" || local[0].Session != "" {
		t.Errorf("local actions must not be detached: %+v", local)
	}
}

func TestLocalFavoriteDefaultsTrue(t *testing.T) {
	cfg := &Config{}
	if err := validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Local.Favorite == nil || !*cfg.Local.Favorite {
		t.Fatalf("local favorite should default to true: %+v", cfg.Local.Favorite)
	}
	off := false
	cfg.Local.Favorite = &off
	if err := validate(cfg); err != nil {
		t.Fatalf("validate with favorite off: %v", err)
	}
	if cfg.Local.Favorite == nil || *cfg.Local.Favorite {
		t.Fatal("explicit favorite=false should be preserved")
	}
}
