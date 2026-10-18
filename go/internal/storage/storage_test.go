package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readJSON(t *testing.T, path string, v interface{}) error {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func TestEnsureDirPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir mode = %04o, want 0700", perm)
	}
	if err := EnsureDir(dir); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestWriteAtomicPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thing.json")
	if err := Write(path, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	// Overwrite (covers the rename-over-existing path too).
	if err := Write(path, []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", perm)
	}
	data, _ := os.ReadFile(path)
	if string(data) != `{"a":2}` {
		t.Fatalf("content = %q", data)
	}
	// No temp litter behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestWriteJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	type payload struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
	}
	in := payload{Version: 1, Name: "dev"}
	if err := WriteJSON(path, in); err != nil {
		t.Fatal(err)
	}
	var out payload
	if err := readJSON(t, path, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: %+v != %+v", out, in)
	}
}

func TestAppendCreatesAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines")
	if err := Append(path, "one"); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, "two"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", perm)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "one\ntwo\n" {
		t.Fatalf("content = %q", data)
	}
}

func TestFixPermissions(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	tight := filepath.Join(dir, "tight")
	for _, p := range []string{loose, tight} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := FixPermissions(loose)
	if err != nil || !changed {
		t.Fatalf("loose: changed=%v err=%v", changed, err)
	}
	info, _ := os.Stat(loose)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("loose mode = %04o, want 0600", perm)
	}
	// Already-private file reports no change.
	changed, err = FixPermissions(tight)
	if err != nil || changed {
		t.Fatalf("tight: changed=%v err=%v", changed, err)
	}
}
