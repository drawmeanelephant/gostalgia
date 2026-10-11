// Package sshx serves the Gostalgia shell over SSH.
//
// This is the runtime's first inbound network surface and exists strictly
// behind an operator opt-in flag (`gostalgia boot --ssh ADDR`). It binds
// loopback-first, authenticates by public key only (never passwords, never
// keyboard-interactive, never anonymous), and attaches each SSH session to
// the environment through the same IPC path and session machinery a local
// `gostalgia attach` uses. See the "SSH attach" section of docs/security.md
// for the threat model.
//
// The transport is charm.land/ssh — the SSH server library wish v2 itself is
// built on. Wish's middleware framework was evaluated for this spike but not
// adopted: its bubbletea middleware owns Program.Run and cannot express the
// per-connection IPC client lifecycle (dial, authenticate, detach, close) and
// presentation-view teardown an attached Gostalgia shell needs, and it would
// roughly double the new-module footprint. The session handler below follows
// wish's own emulated-PTY path: the session channel is the byte pipe, TERM and
// window size come from the pty request, and window-change requests feed
// tea.WindowSizeMsg.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/ssh"

	"gostalgia/internal/experience/shell"
)

const (
	// DefaultMaxConnections bounds concurrent inbound SSH connections. Each
	// session channel on a connection dials one IPC client, so this also
	// bounds how much of the IPC connection budget SSH can consume.
	DefaultMaxConnections = 16
	// DefaultHandshakeTimeout bounds how long a connection may take to finish
	// the SSH handshake, matching the 5s IPC handshake budget convention.
	DefaultHandshakeTimeout = 5 * time.Second
)

// Connection is the per-session IPC caller the SSH handler uses. *ipc.Client
// satisfies it; tests supply fakes.
type Connection interface {
	shell.Caller
	// Done closes when the underlying IPC connection drops.
	Done() <-chan struct{}
	Close() error
}

// Connector supplies an authenticated IPC connection for one attached SSH
// session. It is invoked after SSH public-key authentication succeeds.
type Connector func(ctx context.Context) (Connection, error)

// Options configures a Server.
type Options struct {
	// Addr is the listen address. A bare port ("2222") or empty host
	// (":2222") binds loopback. Non-loopback binds are allowed but logged.
	Addr string
	// HostKey is the server host key (required).
	HostKey ssh.Signer
	// Keys is the authorized client key set (required, non-empty).
	Keys *AuthorizedKeys
	// Connect builds the IPC connection for a session (required).
	Connect Connector
	// Log receives server lifecycle and auth-rejection logs (optional).
	Log *slog.Logger
	// MaxConnections bounds concurrent connections (default DefaultMaxConnections).
	MaxConnections int
	// HandshakeTimeout bounds the SSH handshake (default DefaultHandshakeTimeout).
	HandshakeTimeout time.Duration
}

// Server is an opt-in SSH attach listener.
type Server struct {
	opts Options
	srv  *ssh.Server
	log  *slog.Logger

	mu   sync.Mutex
	lns  map[net.Listener]struct{}
	done chan struct{}
	once sync.Once
}

// NormalizeAddr resolves the --ssh flag value into a listen address. A bare
// port ("2222") or an empty host (":2222") binds loopback; any explicit host
// is used as given. The port must be numeric.
func NormalizeAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("sshx: empty listen address")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// A bare port number fails SplitHostPort with "missing port".
		if p, perr := strconv.Atoi(addr); perr == nil && p > 0 && p < 65536 {
			return net.JoinHostPort("127.0.0.1", addr), nil
		}
		return "", fmt.Errorf("sshx: invalid listen address %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		return "", fmt.Errorf("sshx: missing port in listen address %q", addr)
	}
	return net.JoinHostPort(host, port), nil
}

// New builds a Server. It fails closed: host key, authorized keys and the IPC
// connector are all required, and an empty key set is a configuration error.
func New(opts Options) (*Server, error) {
	if opts.HostKey == nil {
		return nil, errors.New("sshx: host key is required")
	}
	if opts.Keys == nil || len(opts.Keys.entries) == 0 {
		return nil, errors.New("sshx: at least one authorized key is required")
	}
	if opts.Connect == nil {
		return nil, errors.New("sshx: IPC connector is required")
	}
	addr, err := NormalizeAddr(opts.Addr)
	if err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = DefaultMaxConnections
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	opts.Addr = addr

	s := &Server{opts: opts, log: log, lns: map[net.Listener]struct{}{}, done: make(chan struct{})}
	s.srv = &ssh.Server{
		Addr:              addr,
		Handler:           s.handleSession,
		HostSigners:       []ssh.Signer{opts.HostKey},
		Version:           "gostalgia",
		PublicKeyHandler:  s.publicKeyAuth,
		HandshakeTimeout:  opts.HandshakeTimeout,
		ChannelHandlers:   map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler},
		RequestHandlers:   map[string]ssh.RequestHandler{},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{},
		// Only interactive "shell" requests are served. "exec" and "subsystem"
		// carry no meaning for an attached TUI session.
		SessionRequestCallback: func(_ ssh.Session, requestType string) bool {
			return requestType == "shell"
		},
	}
	return s, nil
}

// Addr returns the resolved listen address.
func (s *Server) Addr() string { return s.opts.Addr }

// ListenAndServe binds the configured address and serves until Close.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("sshx: listen %s: %w", s.opts.Addr, err)
	}
	return s.Serve(ln)
}

// Serve accepts connections on ln until Close. It returns nil on intentional
// shutdown.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		ln.Close()
		return nil
	default:
	}
	s.lns[ln] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.lns, ln)
		s.mu.Unlock()
	}()
	err := s.srv.Serve(&limitListener{Listener: ln, max: s.opts.MaxConnections})
	if errors.Is(err, ssh.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops all listeners and drops live connections. It never waits on a
// connection it did not close itself.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.srv.Close()
	})
	return err
}

// HostKeyFingerprint returns the OpenSSH-format SHA256 fingerprint of the
// host key, for operators to pin in known_hosts.
func (s *Server) HostKeyFingerprint() string {
	return Fingerprint(s.opts.HostKey.PublicKey())
}

// limitListener bounds concurrent accepted connections. Excess connections
// are closed on accept rather than queued, so a connect flood cannot park
// unbounded sockets inside the process.
type limitListener struct {
	net.Listener
	max int

	mu      sync.Mutex
	pending map[net.Conn]struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		if l.pending == nil {
			l.pending = map[net.Conn]struct{}{}
		}
		if len(l.pending) >= l.max {
			l.mu.Unlock()
			conn.Close()
			continue
		}
		l.pending[conn] = struct{}{}
		l.mu.Unlock()
		return &boundedConn{Conn: conn, parent: l}, nil
	}
}

func (l *limitListener) release(conn net.Conn) {
	l.mu.Lock()
	delete(l.pending, conn)
	l.mu.Unlock()
}

type boundedConn struct {
	net.Conn
	parent *limitListener
	once   sync.Once
}

func (c *boundedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.parent.release(c.Conn) })
	return err
}
