package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func newAdversarialServer(t *testing.T, opts ...ServerOption) (*Server, net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	token := "adversarial-secret-token"
	router := NewRouter()
	router.Handle("test/echo", func(ctx context.Context, req Request) (any, error) {
		var p any
		_ = DecodeParams(req.Params, &p)
		return p, nil
	})
	router.Handle("test/slow", func(ctx context.Context, req Request) (any, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return "done", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	router.Handle("test/oversized", func(ctx context.Context, req Request) (any, error) {
		return strings.Repeat("A", maxLine), nil
	})

	srv := NewServer(ln, router, token, nil, opts...)
	go func() {
		_ = srv.Serve()
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, ln, token
}

func TestMalformedFrames(t *testing.T) {
	_, ln, token := newAdversarialServer(t)

	testCases := []struct {
		name         string
		preAuth      bool
		frame        string
		expectClosed bool
		expectErrSub string
	}{
		{
			name:         "Pre-auth non-auth method is rejected",
			preAuth:      false,
			frame:        `{"id":1,"method":"test/echo","params":"hi"}` + "\n",
			expectClosed: true,
			expectErrSub: "unauthorized: first request must be auth",
		},
		{
			name:         "Auth with missing token",
			preAuth:      false,
			frame:        `{"id":1,"method":"auth","params":{}}` + "\n",
			expectClosed: true,
			expectErrSub: "unauthorized",
		},
		{
			name:         "Auth with invalid token",
			preAuth:      false,
			frame:        `{"id":1,"method":"auth","params":{"token":"wrong"}}` + "\n",
			expectClosed: true,
			expectErrSub: "unauthorized",
		},
		{
			name:         "Auth with non-string token type",
			preAuth:      false,
			frame:        `{"id":1,"method":"auth","params":{"token":12345}}` + "\n",
			expectClosed: true,
			expectErrSub: "bad params",
		},
		{
			name:         "Unterminated JSON frame",
			preAuth:      true,
			frame:        `{"id":2,"method":"test/echo"` + "\n",
			expectClosed: true,
		},
		{
			name:         "Non-JSON garbage",
			preAuth:      true,
			frame:        `GET / HTTP/1.1` + "\n",
			expectClosed: true,
		},
		{
			name:         "Primitive JSON number",
			preAuth:      true,
			frame:        `12345` + "\n",
			expectClosed: true,
		},
		{
			name:         "Primitive JSON string",
			preAuth:      true,
			frame:        `"hello world"` + "\n",
			expectClosed: true,
		},
		{
			name:         "JSON array instead of object",
			preAuth:      true,
			frame:        `[1, 2, 3]` + "\n",
			expectClosed: true,
		},
		{
			name:         "Missing ID",
			preAuth:      true,
			frame:        `{"method":"test/echo"}` + "\n",
			expectClosed: true,
		},
		{
			name:         "Zero ID",
			preAuth:      true,
			frame:        `{"id":0,"method":"test/echo"}` + "\n",
			expectClosed: true,
		},
		{
			name:         "Negative ID",
			preAuth:      true,
			frame:        `{"id":-42,"method":"test/echo"}` + "\n",
			expectClosed: true,
		},
		{
			name:         "Empty method",
			preAuth:      true,
			frame:        `{"id":2,"method":""}` + "\n",
			expectClosed: true,
		},
		{
			name:         "NUL bytes inside JSON",
			preAuth:      true,
			frame:        "{\"id\":2,\"method\":\"test\x00echo\"}\n",
			expectClosed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			scanner := bufio.NewScanner(conn)

			if tc.preAuth {
				// Authenticate first
				authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
				if _, err := conn.Write([]byte(authReq)); err != nil {
					t.Fatalf("write auth: %v", err)
				}
				if !scanner.Scan() {
					t.Fatalf("read auth response: %v", scanner.Err())
				}
				var resp Response
				if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil || !resp.OK {
					t.Fatalf("auth failed: %v, resp=%s", err, scanner.Text())
				}
			}

			// Send the malformed frame
			if _, err := conn.Write([]byte(tc.frame)); err != nil {
				// Writing may fail if connection was closed immediately
				return
			}

			if tc.expectErrSub != "" {
				if scanner.Scan() {
					var resp Response
					_ = json.Unmarshal(scanner.Bytes(), &resp)
					if !strings.Contains(resp.Error, tc.expectErrSub) {
						t.Errorf("resp.Error = %q, want substring %q", resp.Error, tc.expectErrSub)
					}
				}
			}

			if tc.expectClosed {
				// After malformed request, subsequent read should hit EOF
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				buf := make([]byte, 128)
				for {
					_, err := conn.Read(buf)
					if err != nil {
						if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "connection reset") {
							// Successfully closed
							break
						}
						// Deadline exceeded before closed
						t.Fatalf("expected connection closed, got read err: %v", err)
					}
				}
			}
		})
	}
}

func TestDuplicateRequestIDs(t *testing.T) {
	_, ln, token := newAdversarialServer(t)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Authenticate
	authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	if _, err := conn.Write([]byte(authReq)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("auth scan failed")
	}

	// Send slow request with ID 10
	req1 := `{"id":10,"method":"test/slow"}` + "\n"
	if _, err := conn.Write([]byte(req1)); err != nil {
		t.Fatal(err)
	}

	// Send duplicate request with ID 10 while first is in flight
	req2 := `{"id":10,"method":"test/echo","params":"dup"}` + "\n"
	if _, err := conn.Write([]byte(req2)); err != nil {
		t.Fatal(err)
	}

	// Duplicate in-flight request is a protocol violation; server must close connection
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 1024)
	closed := false
	for {
		_, err := conn.Read(buf)
		if err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("expected server to disconnect client on duplicate in-flight ID")
	}
}

func TestOversizedPayloads(t *testing.T) {
	_, ln, token := newAdversarialServer(t)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	if _, err := conn.Write([]byte(authReq)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("auth scan failed")
	}

	// Frame exceeding maxLine (6 MiB + 1024 bytes)
	giantPayload := strings.Repeat("Z", maxLine+1024)
	oversizedReq := fmt.Sprintf(`{"id":2,"method":"test/echo","params":%q}`+"\n", giantPayload)

	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte(oversizedReq))

	// Scanner buffer on server will fail with bufio.ErrTooLong and drop connection
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 512)
	closed := false
	for {
		_, err := conn.Read(buf)
		if err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("expected server to close connection on oversized request")
	}
}

func TestOversizedResponseHandling(t *testing.T) {
	_, ln, token := newAdversarialServer(t)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	if _, err := conn.Write([]byte(authReq)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), maxLine)
	if !scanner.Scan() {
		t.Fatal("auth scan failed")
	}

	// Call test/oversized
	req := `{"id":2,"method":"test/oversized"}` + "\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	if !scanner.Scan() {
		t.Fatalf("failed to scan response: %v", scanner.Err())
	}
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.OK {
		t.Fatal("expected oversized response to fail")
	}
	if !strings.Contains(resp.Error, "frame limit") {
		t.Fatalf("expected frame limit error, got %q", resp.Error)
	}
}

func TestConnectionBudget(t *testing.T) {
	// Configure server with budget of 3 concurrent connections
	maxConns := 3
	_, ln, token := newAdversarialServer(t, WithMaxConnections(maxConns))

	conns := make([]net.Conn, 0, maxConns)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	for i := 0; i < maxConns; i++ {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("connection %d failed to dial: %v", i+1, err)
		}
		conns = append(conns, conn)
		// Authenticate
		authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
		if _, err := conn.Write([]byte(authReq)); err != nil {
			t.Fatalf("conn %d write auth: %v", i+1, err)
		}
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			t.Fatalf("conn %d auth scan: %v", i+1, scanner.Err())
		}
	}

	// 4th connection should exceed budget and be immediately closed
	extraConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("extra dial: %v", err)
	}
	defer extraConn.Close()

	_ = extraConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 64)
	closed := false
	for {
		_, err := extraConn.Read(buf)
		if err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("expected 4th connection to be rejected by connection budget")
	}

	// Close one existing connection to free budget
	_ = conns[0].Close()
	conns = conns[1:]
	time.Sleep(50 * time.Millisecond) // allow untrack

	// Now another connection should succeed
	newConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("new dial after freeing slot: %v", err)
	}
	defer newConn.Close()

	authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	if _, err := newConn.Write([]byte(authReq)); err != nil {
		t.Fatalf("new conn write auth: %v", err)
	}
	scanner := bufio.NewScanner(newConn)
	if !scanner.Scan() {
		t.Fatalf("new conn auth scan failed: %v", scanner.Err())
	}
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil || !resp.OK {
		t.Fatalf("new conn auth failed: %v, resp: %s", err, scanner.Text())
	}
}

func TestHandshakeBudgetTimeout(t *testing.T) {
	// Configure short handshake budget: 100ms
	_, ln, _ := newAdversarialServer(t, WithHandshakeTimeout(100*time.Millisecond))

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Do not send anything; wait for handshake deadline to expire
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 64)
	closed := false
	for {
		_, err := conn.Read(buf)
		if err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("expected server to close connection after handshake timeout")
	}
}

func TestSlowHandshakeDripFeed(t *testing.T) {
	// Handshake budget of 100ms
	_, ln, token := newAdversarialServer(t, WithHandshakeTimeout(100*time.Millisecond))

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Drip-feed auth frame 1 byte every 30ms (total ~50 bytes = 1500ms, exceeding 100ms)
	authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	dripFailed := false
	for i := 0; i < len(authReq); i++ {
		_ = conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := conn.Write([]byte{authReq[i]}); err != nil {
			dripFailed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Server should have terminated connection before full handshake completed
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 64)
	closed := dripFailed
	if !closed {
		for {
			_, err := conn.Read(buf)
			if err != nil {
				closed = true
				break
			}
		}
	}
	if !closed {
		t.Fatal("expected slow drip handshake to be terminated by handshake budget")
	}
}

func TestFloodingClient(t *testing.T) {
	// Configure max in-flight of 4
	maxInFlightLimit := 4
	_, ln, token := newAdversarialServer(t, WithMaxInFlight(maxInFlightLimit))

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	if _, err := conn.Write([]byte(authReq)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("auth scan failed")
	}

	// Send 10 concurrent requests to test/slow without waiting (exceeding limit of 4)
	for id := int64(10); id < 20; id++ {
		req := fmt.Sprintf(`{"id":%d,"method":"test/slow"}`+"\n", id)
		_, _ = conn.Write([]byte(req))
	}

	// Server detects flood (> maxInFlight) and drops connection
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 512)
	closed := false
	for {
		_, err := conn.Read(buf)
		if err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("expected flooding client to be disconnected")
	}
}

func TestSlowClientBlockedWrites(t *testing.T) {
	// Configure short write timeout: 100ms
	_, ln, token := newAdversarialServer(t, WithWriteTimeout(100*time.Millisecond))

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Authenticate
	authReq := fmt.Sprintf(`{"id":1,"method":"auth","params":{"token":%q}}`+"\n", token)
	if _, err := conn.Write([]byte(authReq)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("auth scan failed")
	}

	// Send requests with large payload (1 MiB each) and do NOT read responses
	largeData := strings.Repeat("W", 1024*1024)
	for i := 2; i <= 8; i++ {
		req := fmt.Sprintf(`{"id":%d,"method":"test/echo","params":%q}`+"\n", i, largeData)
		if _, err := conn.Write([]byte(req)); err != nil {
			break
		}
	}

	// Wait for server to attempt writing back 1MB payloads, fill OS TCP buffers,
	// and expire the 100ms write timeout.
	time.Sleep(200 * time.Millisecond)

	// Read until EOF is encountered
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 4096)
	closed := false
	for {
		_, err := conn.Read(buf)
		if err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("expected slow reader connection to be closed after write deadline exceeded")
	}
}
