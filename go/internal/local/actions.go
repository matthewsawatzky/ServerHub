package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	sshclient "serverhub/internal/ssh"
)

const actionTimeout = 60 * time.Second

// RunShell executes a user-configured quick action on the local machine and
// returns its combined output.
func RunShell(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	return RunShellContext(ctx, command)
}

// RunShellContext is RunShell bound to a caller-provided context, so a
// cancelled request stops the child process.
func RunShellContext(ctx context.Context, command string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// CollectContainers lists local Docker containers using the same wire format
// as the remote SSH collector.
func CollectContainers() ([]sshclient.Container, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		"docker", "ps", "-a", "--format",
		`{"name":"{{.Names}}","status":"{{.Status}}","state":"{{.State}}"}`,
	).Output()
	if err != nil {
		return nil, err
	}

	var containers []sshclient.Container
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c sshclient.Container
		if err := json.Unmarshal([]byte(line), &c); err == nil && sshclient.SanitizeContainer(c) {
			containers = append(containers, c)
		}
	}
	return containers, nil
}

// RunContainerAction starts, stops or restarts a local container.
func RunContainerAction(name, action string) (string, error) {
	if !sshclient.SanitizeContainer(sshclient.Container{Name: name}) {
		return "", fmt.Errorf("invalid container name")
	}
	switch action {
	case "start", "stop", "restart":
	default:
		return "", fmt.Errorf("invalid container action")
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", action, name).CombinedOutput()
	return string(out), err
}

const dockerImagesFormat = `{"repository":"{{.Repository}}","tag":"{{.Tag}}","id":"{{.ID}}","size":"{{.Size}}","created":"{{.CreatedSince}}"}`

// CollectImages lists local Docker images using the same wire format as the
// remote SSH collector.
func CollectImages() ([]sshclient.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "images", "--format", dockerImagesFormat).Output()
	if err != nil {
		return nil, err
	}
	return sshclient.ParseImageLines(string(out)), nil
}

// StreamLogs follows a local container's logs. Both stdout and stderr are
// merged; cancelling ctx kills the docker process.
func StreamLogs(ctx context.Context, name string, tail int) (io.ReadCloser, error) {
	if !sshclient.ValidContainerName(name) {
		return nil, fmt.Errorf("invalid container name")
	}
	if tail < 1 {
		tail = 200
	}
	if tail > 10000 {
		tail = 10000
	}
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", strconv.Itoa(tail), "-f", name)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		return nil, err
	}
	go func() {
		pw.CloseWithError(cmd.Wait())
	}()
	return pr, nil
}
