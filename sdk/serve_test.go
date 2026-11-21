package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

type mockInstance struct {
	initFn func(*Context) error
	runFn  func(context.Context) error
	stopFn func(context.Context) error
}

func (m *mockInstance) Init(ctx *Context) error {
	if m.initFn != nil {
		return m.initFn(ctx)
	}
	return nil
}

func (m *mockInstance) Run(ctx context.Context) error {
	if m.runFn != nil {
		return m.runFn(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *mockInstance) Stop(ctx context.Context) error {
	if m.stopFn != nil {
		return m.stopFn(ctx)
	}
	return nil
}

func TestServeWithOptions(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test-serve.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	endpoint := "unix://" + sockPath
	token := "test-token-123"
	appID := "com.test.served"

	serverErrCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		writeMsg := func(id int64, ok bool, data any, errStr string) {
			var raw json.RawMessage
			if data != nil {
				raw, _ = json.Marshal(data)
			}
			msg := rpcMessage{ID: id, OK: ok, Data: raw, Error: errStr}
			b, _ := json.Marshal(msg)
			b = append(b, '\n')
			_, _ = conn.Write(b)
		}

		// 1. Read auth
		line, err := reader.ReadBytes('\n')
		if err != nil {
			serverErrCh <- fmt.Errorf("server read auth: %w", err)
			return
		}
		var authReq rpcMessage
		if err := json.Unmarshal(line, &authReq); err != nil {
			serverErrCh <- err
			return
		}
		var authP map[string]string
		_ = json.Unmarshal(authReq.Params, &authP)
		if authP["token"] != token {
			writeMsg(authReq.ID, false, nil, "bad token")
			return
		}
		writeMsg(authReq.ID, true, map[string]bool{"authenticated": true}, "")

		// 2. Read app/ready
		line, err = reader.ReadBytes('\n')
		if err != nil {
			serverErrCh <- fmt.Errorf("server read ready: %w", err)
			return
		}
		var readyReq rpcMessage
		if err := json.Unmarshal(line, &readyReq); err != nil {
			serverErrCh <- err
			return
		}
		var readyP map[string]any
		_ = json.Unmarshal(readyReq.Params, &readyP)
		if readyP["app_id"] != appID {
			writeMsg(readyReq.ID, false, nil, "bad app_id")
			return
		}
		writeMsg(readyReq.ID, true, map[string]bool{"ready": true}, "")

		// 3. Send a test invocation
		reqB, _ := json.Marshal(rpcMessage{
			ID:     100,
			Method: "ping",
			Params: json.RawMessage(`{"text":"hello"}`),
		})
		reqB = append(reqB, '\n')
		_, _ = conn.Write(reqB)

		// 4. Read response
		line, err = reader.ReadBytes('\n')
		if err != nil {
			serverErrCh <- fmt.Errorf("server read ping response: %w", err)
			return
		}
		var pingResp rpcMessage
		if err := json.Unmarshal(line, &pingResp); err != nil {
			serverErrCh <- err
			return
		}
		if !pingResp.OK {
			serverErrCh <- fmt.Errorf("ping failed: %s", pingResp.Error)
			return
		}
		serverErrCh <- nil
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inst := &mockInstance{
		initFn: func(c *Context) error {
			return c.Handle("ping", func(ctx context.Context, raw json.RawMessage) (any, error) {
				var p map[string]string
				_ = json.Unmarshal(raw, &p)
				return map[string]string{"reply": "pong: " + p["text"]}, nil
			})
		},
		runFn: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		},
	}

	clientErrCh := make(chan error, 1)
	go func() {
		clientErrCh <- ServeWithOptions(ctx, inst, ServeOptions{
			Endpoint: endpoint,
			Token:    token,
			AppID:    appID,
		})
	}()

	// Wait for server to verify handshake and ping
	select {
	case err := <-serverErrCh:
		if err != nil {
			t.Fatalf("server error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server timed out")
	}

	// Stop client
	cancel()
	select {
	case err := <-clientErrCh:
		if err != nil {
			t.Fatalf("client error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client timed out stopping")
	}
}
