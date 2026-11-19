package sshx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
	"gostalgia/internal/ipc"
	"gostalgia/internal/runtime"
	"gostalgia/platform"
)

type motionSSHBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *motionSSHBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *motionSSHBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *motionSSHBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *motionSSHBuffer) ContainsSince(s string, offset int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if offset > b.buf.Len() {
		return false
	}
	return strings.Contains(xansi.Strip(b.buf.String()[offset:]), s)
}

func (b *motionSSHBuffer) ContainsRawSince(s string, offset int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return offset <= b.buf.Len() && strings.Contains(b.buf.String()[offset:], s)
}

type motionSSHClient struct {
	client  *gossh.Client
	session *gossh.Session
	stdin   io.WriteCloser
	out     *motionSSHBuffer
}

func newMotionSSHClient(t *testing.T, addr string, key gossh.Signer) *motionSSHClient {
	t.Helper()
	client, err := gossh.Dial("tcp", addr, clientConfig("guest", key))
	if err != nil {
		t.Fatalf("SSH dial: %v", err)
	}
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		t.Fatalf("SSH session: %v", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		t.Fatalf("SSH stdin: %v", err)
	}
	out := &motionSSHBuffer{}
	session.Stdout, session.Stderr = out, out
	if err := session.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		session.Close()
		client.Close()
		t.Fatalf("SSH PTY: %v", err)
	}
	if err := session.Shell(); err != nil {
		session.Close()
		client.Close()
		t.Fatalf("SSH shell: %v", err)
	}
	c := &motionSSHClient{client: client, session: session, stdin: stdin, out: out}
	t.Cleanup(func() {
		session.Close()
		client.Close()
	})
	return c
}

func (c *motionSSHClient) send(t *testing.T, input string) int {
	t.Helper()
	offset := c.out.Len()
	if _, err := io.WriteString(c.stdin, input); err != nil {
		t.Fatalf("SSH input: %v", err)
	}
	return offset
}

func (c *motionSSHClient) waitSince(t *testing.T, offset int, text string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !c.out.ContainsSince(text, offset) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.out.ContainsSince(text, offset) {
		raw := c.out.String()
		if len(raw) > 4000 {
			raw = raw[len(raw)-4000:]
		}
		t.Fatalf("SSH output after %d never contained %q; tail: %q", offset, text, xansi.Strip(raw))
	}
}

func (c *motionSSHClient) waitRawSince(t *testing.T, offset int, text string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !c.out.ContainsRawSince(text, offset) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.out.ContainsRawSince(text, offset) {
		t.Fatalf("SSH raw output after %d never contained %q", offset, text)
	}
}

func (c *motionSSHClient) command(t *testing.T, command, want string) {
	t.Helper()
	offset := c.send(t, command+"\r")
	c.waitSince(t, offset, want)
}

// hasMoreOutput observes actual post-result terminal writes. A motion-enabled
// failed launch has further frame writes; a reduced one does not. The window
// is shorter than the shell's existing 500ms functional poll.
func (c *motionSSHClient) hasMoreOutput(d time.Duration) bool {
	offset := c.out.Len()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.out.Len() > offset {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c.out.Len() > offset
}

func dialMotionRuntime(root string) (*ipc.Client, error) {
	data, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		return nil, err
	}
	var info struct {
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	conn, err := platform.DialIPC(info.Endpoint)
	if err != nil {
		return nil, err
	}
	client, err := ipc.NewClient(conn, info.Token)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

func TestConcurrentSSHMotionPolicies(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("SSH motion test cleanup") })
	connect := func(context.Context) (Connection, error) { return dialMotionRuntime(root) }
	host, clientKey := testSigner(t), testSigner(t)
	keys, err := ParseAuthorizedKeys([]byte(authorizedLine(clientKey, "")))
	if err != nil {
		t.Fatal(err)
	}
	_, addr := serveTest(t, Options{Addr: ":0", HostKey: host, Keys: keys, Connect: connect})

	// Both clients stay attached to the same server and runtime for every
	// transition below. A starts enabled; B selects ReducedMotion.
	a, b := newMotionSSHClient(t, addr, clientKey), newMotionSSHClient(t, addr, clientKey)
	for name, c := range map[string]*motionSSHClient{"a": a, "b": b} {
		c.waitSince(t, 0, "Services: 15")
		if c.out.Len() == 0 {
			t.Fatalf("client %s received no shell output", name)
		}
	}
	b.command(t, "motion on", "Reduced motion is now enabled")
	if a.out.ContainsSince("Reduced motion is now enabled", 0) {
		t.Fatal("B's motion policy changed A")
	}

	assertFailureMotion := func(name string, c *motionSSHClient, wantFrames bool) {
		t.Helper()
		offset := c.send(t, "launch com.test.missing\r")
		c.waitSince(t, offset, "unknown application")
		if got := c.hasMoreOutput(120 * time.Millisecond); got != wantFrames {
			t.Errorf("%s post-failure animation output = %v, want %v", name, got, wantFrames)
		}
	}
	assertFailureMotion("A enabled", a, true)
	assertFailureMotion("B reduced", b, false)

	a.command(t, "motion on", "Reduced motion is now enabled")
	assertFailureMotion("A changed to reduced", a, false)
	assertFailureMotion("B still reduced", b, false)

	b.command(t, "motion off", "Reduced motion is now disabled")
	assertFailureMotion("A independently reduced", a, false)
	assertFailureMotion("B changed to enabled", b, true)

	for name, c := range map[string]*motionSSHClient{"a": a, "b": b} {
		offset := c.send(t, "\x03")
		c.waitRawSince(t, offset, "\x1b[?1049l")
		_ = c.session.Wait()
		if err := c.client.Close(); err != nil {
			t.Errorf("%s close: %v", name, err)
		}
	}
	status, err := dialMotionRuntime(root)
	if err != nil {
		t.Fatal(err)
	}
	defer status.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out any
	if err := status.Call(ctx, "sys/status", nil, &out); err != nil {
		t.Fatalf("runtime did not survive concurrent detaches: %v", err)
	}
	if fmt.Sprint(out) == "" {
		t.Fatal("runtime returned no status after concurrent detaches")
	}
}
