package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// Dispatch keeps a handler's own declared code, classifies the fs and
// context sentinels, and reports everything else as "unknown" — never an
// empty code on the wire.
func TestDispatchAssignsErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"declared", &Error{Code: CodeNotFound, Message: "not_found: /x"}, CodeNotFound},
		{"fs not-exist", fmt.Errorf("vfs: %w", fs.ErrNotExist), CodeNotFound},
		{"fs permission", fmt.Errorf("vfs: %w", fs.ErrPermission), CodePermissionDenied},
		{"canceled", context.Canceled, CodeCanceled},
		{"deadline", context.DeadlineExceeded, CodeDeadlineExceeded},
		{"plain", errors.New("boom"), CodeUnknown},
	}
	for _, tc := range cases {
		r := NewRouter()
		must(t, r.Handle("x/fail", func(ctx context.Context, req Request) (any, error) {
			return nil, tc.err
		}))
		resp := r.Dispatch(context.Background(), Request{ID: 1, Method: "x/fail"})
		if resp.OK || resp.Error != tc.err.Error() || resp.Code != tc.want {
			t.Errorf("%s: resp = %+v, want code %q with message %q", tc.name, resp, tc.want, tc.err)
		}
	}
}

// A code-bearing error crosses the socket intact: the client reconstructs
// an *Error whose Code classifies and whose Unwrap restores the sentinel,
// so errors.Is sees the same answer on both sides.
func TestClientRoundTripPreservesErrorCode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter()
	must(t, r.Handle("fs/read", func(ctx context.Context, req Request) (any, error) {
		return nil, fmt.Errorf("vfs: %w", fs.ErrNotExist)
	}))
	srv := NewServer(ln, r, "sekrit", slog.New(slog.NewTextHandler(discard{}, nil)))
	t.Cleanup(func() { srv.Close() })
	go srv.Serve()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(conn, "sekrit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	err = client.Call(context.Background(), "fs/read", nil, nil)
	if err == nil {
		t.Fatal("Call succeeded, want not_found failure")
	}
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != CodeNotFound {
		t.Fatalf("error = %#v, want *Error{Code: %q}", err, CodeNotFound)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want errors.Is(fs.ErrNotExist)", err)
	}
	if !strings.Contains(err.Error(), "file does not exist") {
		t.Errorf("message = %q, want the human-readable text preserved", err)
	}
}

// A peer speaking the pre-code protocol still yields a usable error: the
// message is intact and the empty Code simply means unclassified.
func TestClientToleratesOldPeerWithoutCode(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		r := bufio.NewScanner(serverConn)
		w := bufio.NewWriter(serverConn)
		for r.Scan() {
			var req Request
			if err := json.Unmarshal(r.Bytes(), &req); err != nil {
				return
			}
			var line string
			if req.Method == "auth" {
				line = fmt.Sprintf(`{"id":%d,"ok":true}`, req.ID)
			} else {
				line = fmt.Sprintf(`{"id":%d,"ok":false,"error":"not_found: /x"}`, req.ID)
			}
			if _, err := w.WriteString(line + "\n"); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	}()

	client, err := NewClient(clientConn, "anything")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	err = client.Call(context.Background(), "fs/read", nil, nil)
	if err == nil {
		t.Fatal("Call succeeded, want failure")
	}
	if err.Error() != "not_found: /x" {
		t.Fatalf("message = %q, want the old peer's error text", err)
	}
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != "" {
		t.Fatalf("error = %#v, want *Error with empty Code", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Error("unclassified old-peer error must not match fs.ErrNotExist")
	}
}

// ErrorCode classifies declared codes first, then sentinels, so callers
// get a stable wire vocabulary regardless of error provenance.
func TestErrorCodeClassification(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{&Error{Code: CodePolicyDenied, Message: "denied"}, CodePolicyDenied},
		{fmt.Errorf("wrap: %w", &Error{Code: CodeIO, Message: "disk"}), CodeIO},
		{fmt.Errorf("wrap: %w", fs.ErrExist), CodeAlreadyExists},
		{context.DeadlineExceeded, CodeDeadlineExceeded},
		{errors.New("mystery"), CodeUnknown},
	}
	for _, tc := range cases {
		if got := ErrorCode(tc.err); got != tc.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// sentinelForCode restores the fs/context vocabulary errors.Is expects.
func TestErrorUnwrapRestoresSentinels(t *testing.T) {
	for code, want := range map[string]error{
		CodeNotFound:         fs.ErrNotExist,
		CodeAlreadyExists:    fs.ErrExist,
		CodePermissionDenied: fs.ErrPermission,
		CodeInvalid:          fs.ErrInvalid,
		CodeCanceled:         context.Canceled,
		CodeDeadlineExceeded: context.DeadlineExceeded,
	} {
		err := &Error{Code: code, Message: "m"}
		if !errors.Is(err, want) {
			t.Errorf("&Error{Code:%q} does not match %v", code, want)
		}
	}
	if errors.Is(&Error{Code: CodeUnknown, Message: "m"}, fs.ErrNotExist) {
		t.Error("unknown code should not match fs.ErrNotExist")
	}
}
