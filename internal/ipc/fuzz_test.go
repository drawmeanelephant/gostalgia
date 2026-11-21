package ipc

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func FuzzScanNDJSON(f *testing.F) {
	seeds := [][]byte{
		[]byte("{}"),
		[]byte("{}\n"),
		[]byte("{}\r\n"),
		[]byte("{\"id\":1,\"method\":\"test\"}\n"),
		[]byte("{\"id\":1,\"method\":\"test\"}\r\n"),
		[]byte("{\"id\":1}\n{\"id\":2}\n"),
		[]byte("\n\n\n"),
		[]byte("\r\n\r\n"),
		[]byte("unterminated frame"),
		[]byte(""),
		bytes.Repeat([]byte("A"), 1024),
		[]byte("\x00\x01\x02\n"),
	}
	for _, seed := range seeds {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, data []byte, atEOF bool) {
		adv, token, err := scanNDJSON(data, atEOF)
		if adv < 0 || adv > len(data) {
			t.Fatalf("scanNDJSON returned invalid advance: %d for data len %d", adv, len(data))
		}
		if err != nil && atEOF && len(data) > 0 {
			// Expected for truncated frame at EOF
		}
		_ = token
	})
}

func FuzzReadRequest(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"id":1,"method":"auth","params":{"token":"abc"}}` + "\n"),
		[]byte(`{"id":2,"method":"sys/status"}` + "\n"),
		[]byte(`{"id":-1,"method":"test"}` + "\n"),
		[]byte(`{"id":0,"method":"test"}` + "\n"),
		[]byte(`{"id":1,"method":""}` + "\n"),
		[]byte(`{"id":"invalid","method":"test"}` + "\n"),
		[]byte(`{"id":1e50,"method":"test"}` + "\n"),
		[]byte(`not json at all` + "\n"),
		[]byte(``),
		[]byte(`{}` + "\n"),
		[]byte(`[1, 2, 3]` + "\n"),
		[]byte(`12345` + "\n"),
		[]byte(`"string"` + "\n"),
		[]byte("{\"id\":1,\"method\":\"test\x00with\x00nuls\"}\n"),
		bytes.Repeat([]byte("B"), 1024),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Split(scanNDJSON)
		req, err := readRequest(scanner)
		if err == nil {
			if req.ID <= 0 {
				t.Fatalf("readRequest returned non-positive ID %d without error", req.ID)
			}
			if req.Method == "" {
				t.Fatalf("readRequest returned empty method without error")
			}
		}
	})
}

func FuzzServerFrames(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"id":1,"method":"auth","params":{"token":"secret"}}` + "\n"),
		[]byte(`{"id":1,"method":"unauthorized"}` + "\n"),
		[]byte(`garbage input not json` + "\n"),
		[]byte(`{"id":-99}` + "\n"),
		[]byte("\x00\x00\x00\n"),
		[]byte(`{"id":1,"method":"auth","params":{"token":"secret"}}` + "\n" + `{"id":2,"method":"test/echo"}` + "\n"),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		router := NewRouter()
		router.Handle("test/echo", func(ctx context.Context, req Request) (any, error) {
			return "echo", nil
		})

		serverConn, clientConn := net.Pipe()
		defer func() { _ = serverConn.Close() }()
		defer func() { _ = clientConn.Close() }()

		srv := NewServer(nil, router, "secret", nil,
			WithHandshakeTimeout(100*time.Millisecond),
			WithWriteTimeout(100*time.Millisecond),
		)

		srv.mu.Lock()
		srv.conns[serverConn] = struct{}{}
		srv.wg.Add(1)
		srv.mu.Unlock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.handleConn(serverConn)
		}()

		// Send fuzzed payload from client
		go func() {
			_ = clientConn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
			_, _ = clientConn.Write(payload)
			_ = clientConn.Close()
		}()

		select {
		case <-done:
			// Handled gracefully without crash/panic
		case <-time.After(300 * time.Millisecond):
			// Force close
			_ = serverConn.Close()
			_ = clientConn.Close()
			<-done
		}
	})
}
