package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"serverhub/internal/storage"
)

// JobRunner executes the tmux job scripts on this machine. Local job state is
// kept inside storage.Dir() so dev builds never touch the live installation or
// the user's own tmux server.
type JobRunner struct {
	jobsDir string
	tmuxDir string
	env     []string
}

// TmuxAvailable reports whether local detached jobs can run here.
func TmuxAvailable() bool {
	if runtime.GOOS == "windows" {
		return false
	}
	_, err := exec.LookPath("tmux")
	return err == nil
}

// NewJobRunner prepares the local job and tmux directories.
func NewJobRunner() (*JobRunner, error) {
	if !TmuxAvailable() {
		return nil, fmt.Errorf("local detached jobs require tmux (not available on this machine)")
	}
	dir, err := storage.Dir()
	if err != nil {
		return nil, err
	}
	jobsDir := filepath.Join(dir, "local-jobs")
	tmuxDir := filepath.Join(dir, "tmux")
	env := append(os.Environ(), "TMUX_TMPDIR="+tmuxDir)
	return &JobRunner{jobsDir: jobsDir, tmuxDir: tmuxDir, env: env}, nil
}

// Dir returns the directory local job logs/status files live in.
func (r *JobRunner) Dir() string { return r.jobsDir }

func (r *JobRunner) ensureDirs() error {
	if err := storage.EnsureDir(r.jobsDir); err != nil {
		return err
	}
	return storage.EnsureDir(r.tmuxDir)
}

// RunCommand runs a shell script locally.
func (r *JobRunner) RunCommand(ctx context.Context, cmd string) (string, error) {
	if err := r.ensureDirs(); err != nil {
		return "", err
	}
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Env = r.env
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// StreamCommand streams a local shell command's output.
func (r *JobRunner) StreamCommand(ctx context.Context, cmd string) (io.ReadCloser, error) {
	if err := r.ensureDirs(); err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Env = r.env
	pr, pw := io.Pipe()
	c.Stdout = pw
	c.Stderr = pw
	if err := c.Start(); err != nil {
		pw.Close()
		return nil, err
	}
	go func() {
		pw.CloseWithError(c.Wait())
	}()
	return pr, nil
}
