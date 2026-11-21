package ipc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// injectListener returns err for the first n Accept calls, then delegates
// to the wrapped listener.
type injectListener struct {
	net.Listener
	remaining atomic.Int32
	err       error
}

func (l *injectListener) Accept() (net.Conn, error) {
	if l.remaining.Add(-1) >= 0 {
		return nil, l.err
	}
	return l.Listener.Accept()
}

func acceptTestServer(t *testing.T, ln net.Listener) *Server {
	t.Helper()
	r := NewRouter()
	must(t, r.Handle("sys/ping", func(ctx context.Context, req Request) (any, error) {
		return map[string]bool{"pong": true}, nil
	}))
	srv := NewServer(ln, r, "sekrit", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { srv.Close() })
	return srv
}

// Regression for #262: one transient accept error (a momentary descriptor
// shortage) used to end Serve, leaving a bound listener nobody accepts —
// a dead control channel that also defeats the duplicate-boot guard.
// Transient failures are retried with capped backoff instead.
func TestServeRetriesTransientAcceptError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	flaky := &injectListener{
		Listener: ln,
		err: &net.OpError{
			Op:  "accept",
			Net: "tcp",
			Err: os.NewSyscallError("accept", syscall.EMFILE),
		},
	}
	flaky.remaining.Store(1)

	srv := acceptTestServer(t, flaky)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	must(t, err)
	client, err := NewClient(conn, "sekrit")
	must(t, err)
	defer client.Close()
	var pong map[string]bool
	must(t, client.Call(context.Background(), "sys/ping", nil, &pong))
	if !pong["pong"] {
		t.Fatal("ping did not round trip")
	}

	select {
	case err := <-serveErr:
		t.Fatalf("Serve ended after one transient accept error: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// The accept backoff must not delay shutdown: Close during a transient
// retry window ends Serve promptly and cleanly.
func TestServeTransientBackoffYieldsToClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	flaky := &injectListener{
		Listener: ln,
		err:      os.NewSyscallError("accept", syscall.ENFILE),
	}
	flaky.remaining.Store(1 << 20) // effectively always failing

	srv := acceptTestServer(t, flaky)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	// Let the loop enter the backoff sleep before closing.
	time.Sleep(30 * time.Millisecond)
	must(t, srv.Close())
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve after Close = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Close during transient backoff")
	}
}

// Fatal accept errors still end Serve; only transient ones are retried.
func TestServeReturnsOnFatalAcceptError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	flaky := &injectListener{Listener: ln, err: errors.New("permanent failure")}
	flaky.remaining.Store(1)

	srv := acceptTestServer(t, flaky)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	select {
	case err := <-serveErr:
		if err == nil {
			t.Fatal("Serve returned nil on a fatal accept error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve kept retrying a fatal accept error")
	}
}
