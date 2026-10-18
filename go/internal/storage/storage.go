// Package storage centralises everything ServerHub persists to disk:
// where the config directory lives, and how files are created and written
// durably and privately (0700 directories, 0600 files, atomic temp+rename
// writes). One place owns these invariants so a new call site can't quietly
// reintroduce a wrong permission or a partial write.
package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// devDir, when baked in at build time (./dev sets it via -ldflags -X),
// replaces the OS config directory so development builds use their own
// state and never touch the live installation.
var devDir string

// Dir returns the directory all of ServerHub's persistent state lives in.
//
//	dev builds:  <repo>/test-config   (overridden via -ldflags -X, see ./dev)
//	macOS:       ~/Library/Application Support/ServerHub
//	Linux:       ~/.config/ServerHub  (or $XDG_CONFIG_HOME/ServerHub)
//	Windows:     %AppData%\ServerHub
func Dir() (string, error) {
	if devDir != "" {
		return devDir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine config directory: %w", err)
	}
	return filepath.Join(base, "ServerHub"), nil
}

// DefaultPath returns the canonical path to config.yaml inside Dir().
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// EnsureDir creates dir (recursively) with private permissions (0700).
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}
	return nil
}

// EnsureFile creates path as an empty 0600 file if missing (append-safe).
func EnsureFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", path, err)
	}
	return f.Close()
}

// Write replaces path atomically: a 0600 temp file in the same directory,
// fsynced, then renamed over the target — a crash mid-write can never leave
// a truncated or partially written file.
func Write(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing %s: %w", path, err)
	}
	// The temp file may carry a restrictive umask-derived mode; force 0600.
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("setting permissions on %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s: %w", path, err)
	}
	return nil
}

// WriteJSON marshals v (indented) and writes it atomically with mode 0600.
func WriteJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	return Write(path, append(data, '\n'))
}

// Append appends one line to path (created 0600 if missing) — used for
// append-only files like known_hosts.
func Append(path, line string) error {
	if err := EnsureFile(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s for append: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("appending to %s: %w", path, err)
	}
	return nil
}

// FixPermissions corrects a group/world-readable file to 0600 and reports
// whether a change was made. Callers log the warning (they know the context).
func FixPermissions(path string) (changed bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Mode().Perm()&0o077 == 0 {
		return false, nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return false, err
	}
	return true, nil
}
