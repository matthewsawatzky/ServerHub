package ssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

type fakeSession struct {
	out     []byte
	err     error
	gate    chan struct{}
	reader  io.Reader
	closed  chan struct{}
	onClose func()
	once    sync.Once
}

func (s *fakeSession) CombinedOutput(string) ([]byte, error) {
	if s.gate != nil {
		<-s.gate
	}
	return append([]byte(nil), s.out...), s.err
}

func (s *fakeSession) StdoutPipe() (io.Reader, error) {
	if s.reader != nil {
		return s.reader, nil
	}
	return bytes.NewReader(s.out), nil
}

func (s *fakeSession) Start(string) error { return nil }

func (s *fakeSession) Close() error {
	s.once.Do(func() {
		if s.onClose != nil {
			s.onClose()
		}
		if s.closed != nil {
			close(s.closed)
		}
	})
	return nil
}

type fakeClient struct {
	mu           sync.Mutex
	newSessionFn func() (connSession, error)
	keepaliveErr error
	sendCalls    int
	closeCount   int
}

func (f *fakeClient) NewSession() (connSession, error) {
	f.mu.Lock()
	fn := f.newSessionFn
	f.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return &fakeSession{}, nil
}

func (f *fakeClient) SendRequest(string, bool, []byte) (bool, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls++
	return f.keepaliveErr == nil, nil, f.keepaliveErr
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCount++
	return nil
}

func (f *fakeClient) stats() (sendCalls, closeCount int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendCalls, f.closeCount
}

func newTestConn(dial func() (connClient, error), keepalive time.Duration) *Conn {
	return newConn("example.test", 22, "tester", "/tmp/key", dial, keepalive)
}

func TestConnSharesSingleDial(t *testing.T) {
	var dials atomic.Int32
	client := &fakeClient{}
	c := newTestConn(func() (connClient, error) {
		dials.Add(1)
		time.Sleep(10 * time.Millisecond)
		return client, nil
	}, time.Hour)
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.clientOrDial()
			if err != nil {
				t.Errorf("clientOrDial: %v", err)
				return
			}
			if got != client {
				t.Errorf("got a different client")
			}
		}()
	}
	wg.Wait()

	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1", got)
	}
}

func TestConnDialFailureBacksOff(t *testing.T) {
	dialErr := errors.New("dial failed")
	var dials atomic.Int32
	c := newTestConn(func() (connClient, error) {
		dials.Add(1)
		return nil, dialErr
	}, time.Hour)
	defer c.Close()

	for i := 0; i < 2; i++ {
		if _, err := c.clientOrDial(); !errors.Is(err, dialErr) {
			t.Fatalf("clientOrDial error = %v, want %v", err, dialErr)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count during backoff = %d, want 1", got)
	}

	c.mu.Lock()
	c.retryAt = time.Now().Add(-time.Second)
	c.mu.Unlock()

	if _, err := c.clientOrDial(); !errors.Is(err, dialErr) {
		t.Fatalf("clientOrDial after backoff error = %v, want %v", err, dialErr)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dial count after backoff = %d, want 2", got)
	}
}

func TestConnSessionSemaphore(t *testing.T) {
	gate := make(chan struct{})
	client := &fakeClient{newSessionFn: func() (connSession, error) {
		return &fakeSession{gate: gate}, nil
	}}
	c := newTestConn(func() (connClient, error) { return client, nil }, time.Hour)
	defer c.Close()

	for i := 0; i < maxControlSessions; i++ {
		sess, _, err := c.newSession(context.Background())
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		defer sess.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.newSession(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ninth session error = %v, want deadline exceeded", err)
	}

	c.release()
	sess, _, err := c.newSession(context.Background())
	if err != nil {
		t.Fatalf("session after release: %v", err)
	}
	sess.Close()
	c.release()
}

func TestConnCancelClosesSessionNotConnection(t *testing.T) {
	closed := make(chan struct{})
	client := &fakeClient{newSessionFn: func() (connSession, error) {
		return &fakeSession{out: []byte("partial"), gate: closed, closed: closed}, nil
	}}
	c := newTestConn(func() (connClient, error) { return client, nil }, time.Hour)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	out, err := c.RunCommand(ctx, "sleep 100")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCommand error = %v, want context.Canceled", err)
	}
	if out != "partial" {
		t.Fatalf("partial output = %q, want %q", out, "partial")
	}
	if n := len(c.sem); n != 0 {
		t.Fatalf("semaphore slots still held: %d", n)
	}
	if _, closes := client.stats(); closes != 0 {
		t.Fatalf("shared connection was closed (close count %d)", closes)
	}
}

func TestConnCommandExitErrorKeepsConnection(t *testing.T) {
	client := &fakeClient{newSessionFn: func() (connSession, error) {
		return &fakeSession{out: []byte("nope"), err: &gossh.ExitError{}}, nil
	}}
	c := newTestConn(func() (connClient, error) { return client, nil }, time.Hour)
	defer c.Close()

	out, err := c.RunCommand(context.Background(), "false")
	if err == nil {
		t.Fatal("expected exit error")
	}
	if out != "nope" {
		t.Fatalf("output = %q, want %q", out, "nope")
	}

	c.mu.Lock()
	live := c.client
	c.mu.Unlock()
	if live != client {
		t.Fatal("normal command failure invalidated the shared connection")
	}
	if _, closes := client.stats(); closes != 0 {
		t.Fatalf("shared connection was closed (close count %d)", closes)
	}
}

func TestConnKeepaliveFailureInvalidatesAndReconnects(t *testing.T) {
	failing := &fakeClient{keepaliveErr: errors.New("keepalive boom")}
	healthy := &fakeClient{}
	var dials atomic.Int32
	c := newTestConn(func() (connClient, error) {
		if dials.Add(1) == 1 {
			return failing, nil
		}
		return healthy, nil
	}, 10*time.Millisecond)
	defer c.Close()

	if _, err := c.clientOrDial(); err != nil {
		t.Fatalf("initial dial: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		live := c.client
		c.mu.Unlock()
		if live == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("keepalive failure did not invalidate the connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, closes := failing.stats(); closes == 0 {
		t.Fatal("failed connection was not closed")
	}

	c.mu.Lock()
	c.retryAt = time.Now().Add(-time.Second)
	c.mu.Unlock()

	got, err := c.clientOrDial()
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	if got != healthy {
		t.Fatal("redial returned the wrong client")
	}
	if n := dials.Load(); n != 2 {
		t.Fatalf("dial count = %d, want 2", n)
	}
}

func TestConnCollectStats(t *testing.T) {
	out := []byte(`{"uptime":42,"cpu_total":100,"cpu_idle":75,"mem_total":2000,"mem_avail":1500,"disk":"10 100","load":"0.1 0.2 0.3"}`)
	client := &fakeClient{newSessionFn: func() (connSession, error) {
		return &fakeSession{out: out}, nil
	}}
	c := newTestConn(func() (connClient, error) { return client, nil }, time.Hour)
	defer c.Close()

	stats, err := c.CollectStats(context.Background())
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}
	if stats.Uptime != 42 || stats.CPUTotal != 100 || stats.MemAvail != 1500 || stats.Load != "0.1 0.2 0.3" {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestConnStreamCommand(t *testing.T) {
	body := "line one\nline two\n"
	client := &fakeClient{newSessionFn: func() (connSession, error) {
		return &fakeSession{reader: strings.NewReader(body)}, nil
	}}
	c := newTestConn(func() (connClient, error) { return client, nil }, time.Hour)
	defer c.Close()

	rc, err := c.StreamCommand(context.Background(), "tail -F log")
	if err != nil {
		t.Fatalf("StreamCommand: %v", err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	if string(data) != body {
		t.Fatalf("stream output = %q, want %q", data, body)
	}
	if n := len(c.sem); n != 0 {
		t.Fatalf("semaphore slots still held after EOF: %d", n)
	}
	rc.Close()
}

func TestConnStreamCommandCancelReleasesSlot(t *testing.T) {
	pr, pw := io.Pipe()
	closed := make(chan struct{})
	client := &fakeClient{newSessionFn: func() (connSession, error) {
		return &fakeSession{reader: pr, closed: closed, onClose: func() { pw.Close() }}, nil
	}}
	c := newTestConn(func() (connClient, error) { return client, nil }, time.Hour)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	rc, err := c.StreamCommand(ctx, "tail -F log")
	if err != nil {
		t.Fatalf("StreamCommand: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	io.ReadAll(rc) // cancel closes the session, which closes the pipe

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelling the stream did not close the session")
	}
	if n := len(c.sem); n != 0 {
		t.Fatalf("semaphore slots still held after cancel: %d", n)
	}
}

func TestReconnectBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 60 * time.Second},
		{50, 60 * time.Second},
	}
	for _, tc := range cases {
		if got := reconnectBackoff(tc.failures); got != tc.want {
			t.Errorf("reconnectBackoff(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}
