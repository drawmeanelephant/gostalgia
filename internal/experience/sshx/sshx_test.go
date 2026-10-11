package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

func testSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

func authorizedLine(signer gossh.Signer, comment string) string {
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + comment
}

func TestNormalizeAddr(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{":2222", "127.0.0.1:2222", false},
		{"2222", "127.0.0.1:2222", false},
		{"  :2222 ", "127.0.0.1:2222", false},
		{"localhost:2200", "localhost:2200", false},
		{"127.0.0.1:22", "127.0.0.1:22", false},
		{"0.0.0.0:2222", "0.0.0.0:2222", false},
		{"[::1]:2222", "[::1]:2222", false},
		{"", "", true},
		{"host:", "", true},
		{"not-an-addr", "", true},
		{":", "", true},
		{"70000", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeAddr(c.in)
		if c.err {
			if err == nil {
				t.Errorf("NormalizeAddr(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeAddr(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseAuthorizedKeys(t *testing.T) {
	k1, k2, k3 := testSigner(t), testSigner(t), testSigner(t)

	data := "# comment line\n\n" +
		authorizedLine(k1, "laptop") + "\n" +
		authorizedLine(k2, "workstation gostalgia:user=alice") + "\n"
	keys, err := ParseAuthorizedKeys([]byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if user, ok := keys.Lookup(k1.PublicKey()); !ok || user != "" {
		t.Errorf("k1 lookup = (%q, %v), want unscoped match", user, ok)
	}
	if user, ok := keys.Lookup(k2.PublicKey()); !ok || user != "alice" {
		t.Errorf("k2 lookup = (%q, %v), want scoped alice", user, ok)
	}
	if _, ok := keys.Lookup(k3.PublicKey()); ok {
		t.Error("unknown key must not be authorized")
	}
}

func TestParseAuthorizedKeysFailClosed(t *testing.T) {
	k1 := testSigner(t)
	for name, data := range map[string]string{
		"empty":            "",
		"comments only":    "# nothing here\n",
		"garbage":          "not a key line\n",
		"bad base64":       "ssh-ed25519 !!!\n",
		"key options":      "restrict " + authorizedLine(k1, "") + "\n",
		"duplicate scope":  authorizedLine(k1, "gostalgia:user=a gostalgia:user=b") + "\n",
		"invalid profile":  authorizedLine(k1, "gostalgia:user=../evil") + "\n",
		"malformed second": authorizedLine(k1, "ok") + "\nbroken line\n",
	} {
		if _, err := ParseAuthorizedKeys([]byte(data)); err == nil {
			t.Errorf("%s: expected parse error", name)
		}
	}
}

func TestLoadOrCreateHostKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh", "ssh_host_ed25519_key")

	signer, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("host key mode = %o, want 600", info.Mode().Perm())
	}
	signer2, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !ssh.KeysEqual(signer.PublicKey(), signer2.PublicKey()) {
		t.Error("reloaded host key differs from generated key")
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateHostKey(path); err == nil {
		t.Error("corrupt host key must fail, not silently regenerate")
	}
}

func TestNewFailClosed(t *testing.T) {
	signer := testSigner(t)
	keys, err := ParseAuthorizedKeys([]byte(authorizedLine(signer, "")))
	if err != nil {
		t.Fatal(err)
	}
	connect := func(context.Context) (Connection, error) { return nil, errors.New("unused") }

	if _, err := New(Options{Addr: ":0", HostKey: signer, Keys: keys, Connect: connect}); err != nil {
		t.Errorf("valid options: %v", err)
	}
	if _, err := New(Options{Addr: ":0", Keys: keys, Connect: connect}); err == nil {
		t.Error("missing host key must fail")
	}
	if _, err := New(Options{Addr: ":0", HostKey: signer, Connect: connect}); err == nil {
		t.Error("missing authorized keys must fail")
	}
	empty, perr := ParseAuthorizedKeys([]byte(authorizedLine(signer, "")))
	if perr != nil {
		t.Fatal(perr)
	}
	empty.entries = nil
	if _, err := New(Options{Addr: ":0", HostKey: signer, Keys: empty, Connect: connect}); err == nil {
		t.Error("empty authorized keys must fail")
	}
	if _, err := New(Options{Addr: ":0", HostKey: signer, Keys: keys}); err == nil {
		t.Error("missing connector must fail")
	}
}

// fakeConnection is an in-memory Connection recording every Call.
type fakeConnection struct {
	done      chan struct{}
	closeOnce sync.Once

	mu        sync.Mutex
	methods   []string
	params    []any
	sessions  []sessionSummary
	attachRsp map[string]any
}

func (f *fakeConnection) Call(_ context.Context, method string, params, out any) error {
	f.mu.Lock()
	f.methods = append(f.methods, method)
	f.params = append(f.params, params)
	f.mu.Unlock()
	var data any
	switch method {
	case "session/list":
		data = f.sessions
	case "session/attach":
		data = f.attachRsp
	}
	if out != nil && data != nil {
		raw, _ := json.Marshal(data)
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (f *fakeConnection) Done() <-chan struct{} { return f.done }

func (f *fakeConnection) Close() error {
	f.closeOnce.Do(func() { close(f.done) })
	return nil
}

func (f *fakeConnection) called(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.methods {
		if m == method {
			return true
		}
	}
	return false
}

func (f *fakeConnection) attachParams() map[string]any {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for i, m := range f.methods {
			if m == "session/attach" {
				p, _ := f.params[i].(map[string]any)
				f.mu.Unlock()
				return p
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func TestResolveSession(t *testing.T) {
	sessions := []sessionSummary{
		{ID: "session-1"},
		{ID: "session-2"},
	}
	sessions[0].User.ID, sessions[0].User.Name = "u-guest", "guest"
	sessions[1].User.ID, sessions[1].User.Name = "u-alice", "alice"
	conn := &fakeConnection{done: make(chan struct{}), sessions: sessions}

	// Scoped key: only alice's session may be selected.
	id, err := resolveSession(context.Background(), conn, "alice", "bob")
	if err != nil || id != "session-2" {
		t.Errorf("scoped alice = %q, %v; want session-2", id, err)
	}
	if _, err := resolveSession(context.Background(), conn, "mallory", "alice"); err == nil {
		t.Error("scoped key with no matching session must fail closed")
	}
	// Unscoped: ssh login name selects a matching session.
	id, err = resolveSession(context.Background(), conn, "", "alice")
	if err != nil || id != "session-2" {
		t.Errorf("login alice = %q, %v; want session-2", id, err)
	}
	// Unscoped + unknown login: default resolution (empty session id).
	id, err = resolveSession(context.Background(), conn, "", "tbuddy")
	if err != nil || id != "" {
		t.Errorf("unknown login = %q, %v; want default resolution", id, err)
	}
}

// serveTest starts a Server on 127.0.0.1:0 and returns its address.
func serveTest(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return srv, ln.Addr().String()
}

func clientConfig(user string, signer gossh.Signer) *gossh.ClientConfig {
	cfg := &gossh.ClientConfig{
		User:            user,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	if signer != nil {
		cfg.Auth = []gossh.AuthMethod{gossh.PublicKeys(signer)}
	}
	return cfg
}

func TestServerAuthFailClosed(t *testing.T) {
	host := testSigner(t)
	allowed, denied := testSigner(t), testSigner(t)
	keys, err := ParseAuthorizedKeys([]byte(authorizedLine(allowed, "")))
	if err != nil {
		t.Fatal(err)
	}
	connect := func(context.Context) (Connection, error) {
		return &fakeConnection{done: make(chan struct{})}, nil
	}
	_, addr := serveTest(t, Options{Addr: ":0", HostKey: host, Keys: keys, Connect: connect})

	// Authorized key: handshake completes.
	c, err := gossh.Dial("tcp", addr, clientConfig("operator", allowed))
	if err != nil {
		t.Fatalf("authorized key handshake: %v", err)
	}
	c.Close()

	// Unauthorized key: rejected.
	if _, err := gossh.Dial("tcp", addr, clientConfig("operator", denied)); err == nil {
		t.Error("unauthorized key must be rejected")
	}

	// No credentials: rejected (no anonymous access).
	if _, err := gossh.Dial("tcp", addr, clientConfig("operator", nil)); err == nil {
		t.Error("anonymous login must be rejected")
	}

	// Password auth is not offered: a client with only a password method
	// must fail.
	cfg := clientConfig("operator", nil)
	cfg.Auth = []gossh.AuthMethod{gossh.Password("hunter2")}
	if _, err := gossh.Dial("tcp", addr, cfg); err == nil {
		t.Error("password auth must not be offered")
	}
}

func TestServerRejectsExec(t *testing.T) {
	host, client := testSigner(t), testSigner(t)
	keys, err := ParseAuthorizedKeys([]byte(authorizedLine(client, "")))
	if err != nil {
		t.Fatal(err)
	}
	connect := func(context.Context) (Connection, error) {
		return &fakeConnection{done: make(chan struct{})}, nil
	}
	_, addr := serveTest(t, Options{Addr: ":0", HostKey: host, Keys: keys, Connect: connect})

	c, err := gossh.Dial("tcp", addr, clientConfig("operator", client))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	// An exec request is denied at the protocol level: only interactive
	// shell sessions are served.
	if err := sess.Run("echo hi"); err == nil {
		t.Error("exec request must be rejected")
	}
}

func TestSSHSessionAttachesShell(t *testing.T) {
	host, clientKey := testSigner(t), testSigner(t)
	keys, err := ParseAuthorizedKeys([]byte(authorizedLine(clientKey, "key gostalgia:user=alice")))
	if err != nil {
		t.Fatal(err)
	}
	sessions := []sessionSummary{{ID: "session-1"}, {ID: "session-2"}}
	sessions[0].User.ID, sessions[0].User.Name = "u-guest", "guest"
	sessions[1].User.ID, sessions[1].User.Name = "u-alice", "alice"

	fake := &fakeConnection{
		done:     make(chan struct{}),
		sessions: sessions,
		attachRsp: map[string]any{
			"session_id": "session-2", "attachment_id": "att-1",
			"client_id": "ssh-x", "client_type": "ssh", "active_count": 1,
			// cwd makes the model emit session/workspace/set in the same
			// update that records the session identity — a deterministic
			// signal the attach response has been consumed.
			"workspace": map[string]any{"cwd": "/users/alice"},
		},
	}
	connect := func(context.Context) (Connection, error) { return fake, nil }
	_, addr := serveTest(t, Options{Addr: ":0", HostKey: host, Keys: keys, Connect: connect})

	c, err := gossh.Dial("tcp", addr, clientConfig("alice", clientKey))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm-256color", 40, 120, gossh.TerminalModes{}); err != nil {
		t.Fatalf("pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	params := fake.attachParams()
	if params == nil {
		t.Fatal("shell never called session/attach")
	}
	if params["client_type"] != attachClientType {
		t.Errorf("client_type = %v, want %q", params["client_type"], attachClientType)
	}
	if params["session_id"] != "session-2" {
		t.Errorf("session_id = %v, want session-2 (profile-scoped key)", params["session_id"])
	}
	if id, _ := params["client_id"].(string); !strings.HasPrefix(id, "ssh-") {
		t.Errorf("client_id = %v, want ssh- prefix", params["client_id"])
	}

	// Wait until the model has consumed the attach response — observable via
	// the session/workspace/set call it emits in the same update — before
	// dropping the connection, so the handler's detach teardown has identity.
	deadline := time.Now().Add(10 * time.Second)
	for !fake.called("session/workspace/set") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !fake.called("session/workspace/set") {
		t.Fatal("model never consumed the attach response")
	}

	// Dropping the SSH connection must release the session attachment.
	c.Close()
	deadline = time.Now().Add(10 * time.Second)
	for !fake.called("session/detach") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !fake.called("session/detach") {
		t.Error("disconnect must detach the session attachment")
	}
}

func TestSSHSessionRejectsMismatchedScope(t *testing.T) {
	host, clientKey := testSigner(t), testSigner(t)
	keys, err := ParseAuthorizedKeys([]byte(authorizedLine(clientKey, "key gostalgia:user=mallory")))
	if err != nil {
		t.Fatal(err)
	}
	sessions := []sessionSummary{{ID: "session-1"}}
	sessions[0].User.ID, sessions[0].User.Name = "u-guest", "guest"

	fake := &fakeConnection{done: make(chan struct{}), sessions: sessions}
	connect := func(context.Context) (Connection, error) { return fake, nil }
	_, addr := serveTest(t, Options{Addr: ":0", HostKey: host, Keys: keys, Connect: connect})

	c, err := gossh.Dial("tcp", addr, clientConfig("mallory", clientKey))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatalf("pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell request: %v", err)
	}
	// The scoped profile has no session: the channel must exit non-zero
	// without ever attaching.
	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()
	select {
	case err := <-waitErr:
		if err == nil {
			t.Error("session must exit non-zero for unmatched scope")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not exit for unmatched scope")
	}
	if fake.called("session/attach") {
		t.Error("session/attach must not run for an unmatched profile scope")
	}
}
