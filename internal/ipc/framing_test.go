package ipc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNDJSONRejectsUnterminatedFrames(t *testing.T) {
	for _, frame := range []string{`{"id":1,"ok":true}`, "{"} {
		scanner := bufio.NewScanner(strings.NewReader(frame))
		scanner.Split(scanNDJSON)
		if scanner.Scan() || scanner.Err() == nil {
			t.Fatal("unterminated frame accepted")
		}
	}
	scanner := bufio.NewScanner(strings.NewReader("{}\r\n{}\n"))
	scanner.Split(scanNDJSON)
	count := 0
	for scanner.Scan() {
		if string(scanner.Bytes()) != "{}" {
			t.Fatal("bad CRLF framing")
		}
		count++
	}
	if count != 2 || scanner.Err() != nil {
		t.Fatalf("valid frames: %d, %v", count, scanner.Err())
	}
}

// A request whose frame exceeds the wire cap fails only that call, before
// any byte is written: the connection and its other calls survive.
func TestOversizedCallFailsOnlyThatCall(t *testing.T) {
	s, _, _ := eventServer(t)
	c := eventClient(t, s)
	err := c.Call(testContext(t), "test/echo", strings.Repeat("x", maxLine), nil)
	if err == nil || !strings.Contains(err.Error(), "frame exceeds limit") {
		t.Fatalf("oversized call = %v, want a frame-limit error", err)
	}
	must(t, c.Call(testContext(t), "test/echo", 7, nil))
}

// A frame between the old 4 MiB cap and the wire limit must round-trip:
// this is what a documented-limit fs/write looks like end-to-end.
func TestLargeFrameWithinLimitRoundTrips(t *testing.T) {
	s, _, r := eventServer(t)
	must(t, r.Handle("test/blob", func(ctx context.Context, req Request) (any, error) {
		var p string
		if err := DecodeParams(req.Params, &p); err != nil {
			return nil, err
		}
		return p, nil
	}))
	c := eventClient(t, s)
	payload := strings.Repeat("y", 5<<20)
	var out string
	must(t, c.Call(testContext(t), "test/blob", payload, &out))
	if out != payload {
		t.Fatal("payload did not round-trip")
	}
}

func TestOversizedResponseLeavesCallsUsable(t *testing.T) {
	s, _, r := eventServer(t)
	must(t, r.Handle("test/large", func(context.Context, Request) (any, error) {
		return strings.Repeat("x", maxLine), nil
	}))
	c, ctx := eventClient(t, s), testContext(t)
	err := c.Call(ctx, "test/large", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("oversized response = %v", err)
	}
	must(t, c.Call(ctx, "test/echo", 3, nil))
}

func TestCancelDuringBlockedWriteReturnsContextError(t *testing.T) {
	conn, peer := net.Pipe()
	release, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = peer.Close() }()
		r := bufio.NewScanner(peer)
		if r.Scan() {
			_, _ = fmt.Fprintln(peer, `{"id":1,"ok":true}`)
		}
		<-release // never read the next request
	}()
	c, err := NewClient(conn, "sekrit")
	must(t, err)
	defer func() {
		c.Close()
		close(release)
		<-done
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "test/echo", 1, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write cancellation = %v", err)
	}
}

func TestCanceledUnsubscribeDisconnectsAndCleansUp(t *testing.T) {
	s, bus, _ := eventServer(t)
	c := eventClient(t, s)
	sub, err := c.Subscribe(testContext(t), SubscribeParams{Topic: "*"})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sub.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unsubscribe = %v", err)
	}
	waitSubscriberCount(t, bus, 0)
}
