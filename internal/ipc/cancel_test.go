package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// A call abandoned by its context must free its server-side in-flight slot:
// the client frees its own slot when the call's context ends, so without a
// cancel frame the server keeps counting the request until the handler
// returns. Enough timed-out calls against a wedged handler would otherwise
// trip the flood rule and sever the whole connection.
func TestCancelReleasesInFlightSlot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	r := NewRouter()
	release := make(chan struct{})
	must(t, r.Handle("test/hang", func(ctx context.Context, req Request) (any, error) {
		<-release // a wedged handler: never answers in time
		return nil, nil
	}))
	must(t, r.Handle("test/ping", func(ctx context.Context, req Request) (any, error) {
		return "pong", nil
	}))
	srv := NewServer(ln, r, "t", nil, WithMaxInFlight(4))
	go srv.Serve()
	t.Cleanup(func() {
		_ = srv.Close()
		close(release)
	})

	conn, err := net.Dial("tcp", ln.Addr().String())
	must(t, err)
	client, err := NewClient(conn, "t")
	must(t, err)
	t.Cleanup(func() { client.Close() })

	// Saturate the in-flight bound with calls that time out client-side.
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := client.Call(ctx, "test/hang", nil, nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("hang call %d = %v, want DeadlineExceeded", i, err)
		}
	}

	// Cancel frames are ordered before this request on the wire, so the
	// server must have freed every slot already; the next request would
	// otherwise be treated as a flood and the connection dropped.
	if err := client.Call(context.Background(), "test/ping", nil, nil); err != nil {
		t.Fatalf("connection unusable after %d timed-out calls: %v", 4, err)
	}
}

// The cancel frame must also cancel the per-request context the server
// hands to the handler, so a route that honors ctx unblocks instead of
// running to completion for a caller that already gave up.
func TestCancelPropagatesToHandlerContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	r := NewRouter()
	handlerCtx := make(chan context.Context, 1)
	must(t, r.Handle("test/wait", func(ctx context.Context, req Request) (any, error) {
		handlerCtx <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	srv := NewServer(ln, r, "t", nil)
	go srv.Serve()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	must(t, err)
	client, err := NewClient(conn, "t")
	must(t, err)
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Call(ctx, "test/wait", nil, nil) }()

	var hctx context.Context
	select {
	case hctx = <-handlerCtx:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("call = %v, want context.Canceled", err)
	}
	select {
	case <-hctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("server handler context not canceled by the cancel frame")
	}
}

// A cancel for a request that never existed or already answered is a no-op,
// not a protocol violation: a late cancel must not kill the connection.
func TestCancelForUnknownIDIsIgnored(t *testing.T) {
	_, ln, token := newAdversarialServer(t)
	conn, err := net.Dial("tcp", ln.Addr().String())
	must(t, err)
	defer conn.Close()

	_, err = fmt.Fprintf(conn, `{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	must(t, err)
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("auth scan failed")
	}

	must(t, conn.SetWriteDeadline(time.Now().Add(2*time.Second)))
	_, err = conn.Write([]byte(`{"kind":"cancel","id":999}` + "\n"))
	must(t, err)
	_, err = conn.Write([]byte(`{"id":2,"method":"test/echo","params":42}` + "\n"))
	must(t, err)

	must(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	if !scanner.Scan() {
		t.Fatal("connection closed after a benign cancel frame")
	}
	var resp Response
	must(t, json.Unmarshal(scanner.Bytes(), &resp))
	if resp.ID != 2 || !resp.OK {
		t.Fatalf("resp = %+v, want ok echo", resp)
	}
}

// Kinded frames other than a well-formed cancel are malformed input and
// close the connection, like any other protocol violation.
func TestMalformedCancelFrameDisconnects(t *testing.T) {
	for _, frame := range []string{
		`{"kind":"cancel"}`,
		`{"kind":"cancel","id":0}`,
		`{"kind":"cancel","id":3,"method":"test/echo"}`,
		`{"kind":"bogus","id":3}`,
	} {
		_, ln, token := newAdversarialServer(t)
		conn, err := net.Dial("tcp", ln.Addr().String())
		must(t, err)
		_, err = fmt.Fprintf(conn, `{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
		must(t, err)
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			t.Fatal("auth scan failed")
		}
		must(t, conn.SetWriteDeadline(time.Now().Add(2*time.Second)))
		_, err = conn.Write([]byte(frame + "\n"))
		must(t, err)

		must(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
		if _, err := conn.Read(make([]byte, 64)); err == nil {
			t.Fatalf("frame %s: expected server to disconnect", frame)
		}
		conn.Close()
	}
}
