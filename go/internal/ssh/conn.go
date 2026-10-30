package ssh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Conn is the single control connection to one host. Polls, container
// listing, and one-off actions share it; attached terminals keep their own
// dials. The connection is dialled lazily, re-dialled after a failure with
// exponential backoff, and kept alive with keepalive@openssh.com.
//
// Sessions opened through the control connection are capped by a semaphore;
// callers that cancel a command close only their session, never the shared
// connection.
type Conn struct {
	host    string
	port    int
	user    string
	keyPath string

	dialFn            func() (connClient, error)
	keepaliveInterval time.Duration
	sem               chan struct{}
	closeCh           chan struct{}

	mu       sync.Mutex
	client   connClient
	dialing  chan struct{}
	lastErr  error
	retryAt  time.Time
	failures int
	closed   bool
}

const (
	maxControlSessions = 8

	keepaliveRequest = "keepalive@openssh.com"
	keepaliveTimeout = 10 * time.Second

	keepaliveInterval = 30 * time.Second

	minReconnectBackoff = 5 * time.Second
	maxReconnectBackoff = 60 * time.Second
)

var errConnClosed = errors.New("ssh connection closed")

// connClient and connSession isolate Conn from the concrete x/crypto types so
// the connection lifecycle can be exercised in tests.
type connClient interface {
	NewSession() (connSession, error)
	SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error)
	Close() error
}

type connSession interface {
	CombinedOutput(cmd string) ([]byte, error)
	StdoutPipe() (io.Reader, error)
	Start(cmd string) error
	Close() error
}

type gosshClient struct{ *gossh.Client }

func (c gosshClient) NewSession() (connSession, error) {
	sess, err := c.Client.NewSession()
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// NewConn creates the per-host control connection. The keepalive goroutine
// starts immediately; call Close when the server entry is removed.
func NewConn(host string, port int, user, keyPath string) *Conn {
	return newConn(host, port, user, keyPath, func() (connClient, error) {
		client, err := Dial(host, port, user, keyPath)
		if err != nil {
			return nil, err
		}
		return gosshClient{client}, nil
	}, keepaliveInterval)
}

func newConn(host string, port int, user, keyPath string, dialFn func() (connClient, error), keepalive time.Duration) *Conn {
	c := &Conn{
		host:              host,
		port:              port,
		user:              user,
		keyPath:           keyPath,
		dialFn:            dialFn,
		keepaliveInterval: keepalive,
		sem:               make(chan struct{}, maxControlSessions),
		closeCh:           make(chan struct{}),
	}
	if keepalive > 0 {
		go c.keepaliveLoop()
	}
	return c
}

// Close invalidates the shared connection and stops the keepalive loop.
// Subsequent calls fail with errConnClosed. Safe to call more than once.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.closeCh)
	client := c.client
	c.client = nil
	c.mu.Unlock()

	if client != nil {
		client.Close()
	}
	return nil
}

// client returns the live shared client, dialling one if necessary. Concurrent
// callers share a single dial. While a failed host is in backoff, callers fail
// fast instead of redialling per poll or action.
func (c *Conn) clientOrDial() (connClient, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, errConnClosed
		}
		if c.client != nil {
			client := c.client
			c.mu.Unlock()
			return client, nil
		}
		if c.dialing != nil {
			wait := c.dialing
			c.mu.Unlock()
			<-wait
			continue
		}
		if !c.retryAt.IsZero() && time.Now().Before(c.retryAt) {
			err := c.lastErr
			if err == nil {
				err = fmt.Errorf("ssh: reconnect backoff")
			}
			c.mu.Unlock()
			return nil, err
		}

		wait := make(chan struct{})
		c.dialing = wait
		c.mu.Unlock()

		client, err := c.dialFn()

		c.mu.Lock()
		c.dialing = nil
		if err != nil {
			c.failures++
			c.lastErr = err
			c.retryAt = time.Now().Add(reconnectBackoff(c.failures))
			close(wait)
			c.mu.Unlock()
			return nil, err
		}
		if c.closed {
			c.mu.Unlock()
			close(wait)
			client.Close()
			return nil, errConnClosed
		}
		c.client = client
		c.failures = 0
		c.lastErr = nil
		c.retryAt = time.Time{}
		close(wait)
		c.mu.Unlock()
		return client, nil
	}
}

// invalidate drops client if it is still the live one and applies reconnect
// backoff. The caller still owns the closed connection.
func (c *Conn) invalidate(client connClient, reason error) {
	c.mu.Lock()
	if c.client != client {
		c.mu.Unlock()
		return
	}
	c.client = nil
	c.failures++
	c.lastErr = reason
	c.retryAt = time.Now().Add(reconnectBackoff(c.failures))
	c.mu.Unlock()

	client.Close()
}

func reconnectBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > 8 {
		return maxReconnectBackoff
	}
	d := minReconnectBackoff << (failures - 1)
	if d <= 0 || d > maxReconnectBackoff {
		return maxReconnectBackoff
	}
	return d
}

// acquire takes a control-connection session slot.
func (c *Conn) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return errConnClosed
	}
}

func (c *Conn) release() { <-c.sem }

// newSession acquires a slot and opens a session on the shared connection.
func (c *Conn) newSession(ctx context.Context) (connSession, connClient, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, nil, err
	}
	client, err := c.clientOrDial()
	if err != nil {
		c.release()
		return nil, nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		c.release()
		c.invalidate(client, err)
		return nil, nil, fmt.Errorf("new session: %w", err)
	}
	return sess, client, nil
}

// RunCommand runs a command over the shared connection. Cancelling ctx closes
// only this session (after collecting any partial output), not the connection.
func (c *Conn) RunCommand(ctx context.Context, cmd string) (string, error) {
	sess, client, err := c.newSession(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		sess.Close()
		c.release()
	}()

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := sess.CombinedOutput(cmd)
		done <- result{out: strings.TrimSpace(string(out)), err: err}
	}()

	select {
	case <-ctx.Done():
		sess.Close()
		r := <-done
		return r.out, ctx.Err()
	case r := <-done:
		// A non-zero exit status is normal command output; anything else
		// means the shared connection is no longer trustworthy.
		var exitErr *gossh.ExitError
		if r.err != nil && !errors.As(r.err, &exitErr) {
			c.invalidate(client, r.err)
		}
		return r.out, r.err
	}
}

// StreamCommand starts a command over the shared connection and returns its
// combined stdout/stderr stream. Closing the stream (or cancelling ctx) closes
// only that session; reading to EOF also releases the session slot.
func (c *Conn) StreamCommand(ctx context.Context, cmd string) (io.ReadCloser, error) {
	sess, client, err := c.newSession(ctx)
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		c.release()
		c.invalidate(client, err)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		c.release()
		var exitErr *gossh.ExitError
		if !errors.As(err, &exitErr) {
			c.invalidate(client, err)
		}
		return nil, fmt.Errorf("start command: %w", err)
	}

	st := &commandStream{
		r:       stdout,
		sess:    sess,
		release: c.release,
		done:    make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			st.Close()
		case <-st.done:
		}
	}()
	return st, nil
}

type commandStream struct {
	r       io.Reader
	sess    connSession
	release func()
	once    sync.Once
	done    chan struct{}
}

func (s *commandStream) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil {
		s.Close()
	}
	return n, err
}

func (s *commandStream) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.sess.Close()
		s.release()
	})
	return nil
}

// CollectStats fetches raw kernel counters over the shared connection.
func (c *Conn) CollectStats(ctx context.Context) (*RawStats, error) {
	out, err := c.RunCommand(ctx, statsCmd)
	if err != nil {
		return nil, fmt.Errorf("stats command: %w (output: %s)", err, out)
	}

	var stats RawStats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		return nil, fmt.Errorf("parsing stats json %q: %w", out, err)
	}
	return &stats, nil
}

// CollectContainers lists Docker containers over the shared connection.
func (c *Conn) CollectContainers(ctx context.Context) ([]Container, error) {
	out, err := c.RunCommand(ctx, dockerCmd)
	if err != nil {
		// docker might not be installed — treat as empty
		return nil, nil
	}

	var containers []Container
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c Container
		if err := json.Unmarshal([]byte(line), &c); err == nil && SanitizeContainer(c) {
			containers = append(containers, c)
		}
	}
	return containers, nil
}

func (c *Conn) keepaliveLoop() {
	ticker := time.NewTicker(c.keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.closeCh:
			return
		case <-ticker.C:
			c.sendKeepalive()
		}
	}
}

func (c *Conn) sendKeepalive() {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest(keepaliveRequest, true, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("[ssh] %s keepalive failed: %v — scheduling reconnect", c.addr(), err)
			c.invalidate(client, err)
			go c.reconnectAfterFailure()
		}
	case <-time.After(keepaliveTimeout):
		log.Printf("[ssh] %s keepalive timed out — scheduling reconnect", c.addr())
		c.invalidate(client, errors.New("keepalive timed out"))
		go c.reconnectAfterFailure()
	case <-c.closeCh:
	}
}

// reconnectAfterFailure waits out the reconnect backoff and makes one attempt,
// so an idle host recovers without waiting for the next poll. Concurrent poll
// dials are single-flighted.
func (c *Conn) reconnectAfterFailure() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	wait := time.Until(c.retryAt)
	c.mu.Unlock()

	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.closeCh:
			return
		}
	}
	c.clientOrDial() //nolint:errcheck — failure is retried by the poller/backoff
}

func (c *Conn) addr() string { return fmt.Sprintf("%s@%s:%d", c.user, c.host, c.port) }
