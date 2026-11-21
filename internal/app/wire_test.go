package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

var wireTestLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// startWireLoop runs childWireLoop over a net.Pipe where childConn plays the
// app child. Returns the loop's done channel plus write/read helpers.
func startWireLoop(dispatch func(rpcWireMessage) rpcWireMessage) (child net.Conn, done <-chan struct{}, pendingMu *sync.Mutex, pending map[int64]chan rpcWireMessage) {
	runtimeConn, childConn := net.Pipe()
	pendingMu = &sync.Mutex{}
	pending = map[int64]chan rpcWireMessage{}
	writeMu := &sync.Mutex{}
	d := make(chan struct{})
	go func() {
		childWireLoop(runtimeConn, bufio.NewReader(runtimeConn), writeMu, pendingMu, pending, dispatch, wireTestLog)
		close(d)
	}()
	return childConn, d, pendingMu, pending
}

func writeWire(t *testing.T, conn net.Conn, msg rpcWireMessage) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("child write: %v", err)
	}
}

// A frame beyond the 6 MiB wire cap is a protocol violation: the loop must
// error and close the connection rather than buffer it without bound.
func TestChildWireLoopRejectsOversizedFrame(t *testing.T) {
	dispatch := func(req rpcWireMessage) rpcWireMessage {
		return rpcWireMessage{ID: req.ID, OK: true}
	}
	childConn, done, _, _ := startWireLoop(dispatch)
	defer childConn.Close()

	chunk := bytes.Repeat([]byte{'a'}, 1<<20)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for i := 0; i < 7; i++ {
			if _, err := childConn.Write(chunk); err != nil {
				return
			}
		}
		_, _ = childConn.Write([]byte("\n"))
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wire loop kept reading an oversized frame")
	}
	<-writeDone
	_ = childConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := childConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open after oversized frame")
	}
}

// Child requests dispatch under a bounded in-flight budget. Overflow is
// rejected with an error response instead of queueing: the read loop must
// keep draining the connection because call responses share it.
func TestChildWireLoopBoundsInFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, maxChildInFlight*2)
	dispatch := func(req rpcWireMessage) rpcWireMessage {
		entered <- struct{}{}
		<-release
		return rpcWireMessage{ID: req.ID, OK: true}
	}
	childConn, _, pendingMu, pending := startWireLoop(dispatch)
	defer childConn.Close()
	defer close(release)

	replies := make(chan rpcWireMessage, 8)
	go func() {
		r := bufio.NewReader(childConn)
		for {
			l, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var m rpcWireMessage
			if json.Unmarshal(bytes.TrimSpace(l), &m) == nil {
				replies <- m
			}
		}
	}()

	// Fill the in-flight budget with blocked dispatches.
	for i := int64(1); i <= maxChildInFlight; i++ {
		writeWire(t, childConn, rpcWireMessage{ID: i, Method: "test/block"})
	}
	for i := 0; i < maxChildInFlight; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("dispatch %d never ran", i)
		}
	}

	// Even with the budget exhausted, call responses must still be routed —
	// blocking the read loop on a full budget would deadlock handlers that
	// are themselves waiting on child replies.
	pendingMu.Lock()
	callCh := make(chan rpcWireMessage, 1)
	pending[999] = callCh
	pendingMu.Unlock()
	writeWire(t, childConn, rpcWireMessage{ID: 999, OK: true})
	select {
	case <-callCh:
	case <-time.After(2 * time.Second):
		t.Fatal("call response stalled behind saturated in-flight budget")
	}

	// Requests beyond the budget are rejected with an error response.
	writeWire(t, childConn, rpcWireMessage{ID: maxChildInFlight + 1, Method: "test/block"})
	writeWire(t, childConn, rpcWireMessage{ID: maxChildInFlight + 2, Method: "test/block"})
	got := map[int64]rpcWireMessage{}
	for len(got) < 2 {
		select {
		case m := <-replies:
			got[m.ID] = m
		case <-time.After(5 * time.Second):
			t.Fatal("no rejection response for in-flight overflow")
		}
	}
	for id, m := range got {
		if m.OK || !strings.Contains(m.Error, "in-flight") {
			t.Fatalf("reply %d = %+v, want in-flight rejection", id, m)
		}
	}
}

func TestReadWireLineBoundsFrameSize(t *testing.T) {
	// Payload at the cap plus terminator is accepted.
	ok := append(bytes.Repeat([]byte{'b'}, maxWireLine-1), '\n')
	line, err := readWireLine(bufio.NewReader(bytes.NewReader(ok)))
	if err != nil {
		t.Fatalf("at-limit frame rejected: %v", err)
	}
	if len(line) != maxWireLine {
		t.Fatalf("line len = %d, want %d", len(line), maxWireLine)
	}

	// One byte over the cap fails instead of buffering on.
	big := append(bytes.Repeat([]byte{'c'}, maxWireLine), '\n')
	if _, err := readWireLine(bufio.NewReader(bytes.NewReader(big))); !errors.Is(err, errWireFrameTooLarge) {
		t.Fatalf("oversized frame err = %v, want errWireFrameTooLarge", err)
	}
}

func TestReadWireLineSequencesAndEOF(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{\"id\":1}\n{\"id\":2}\n"))
	l1, err := readWireLine(r)
	if err != nil || string(l1) != "{\"id\":1}\n" {
		t.Fatalf("line1 = %q err=%v", l1, err)
	}
	l2, err := readWireLine(r)
	if err != nil || string(l2) != "{\"id\":2}\n" {
		t.Fatalf("line2 = %q err=%v", l2, err)
	}
	if _, err := readWireLine(r); !errors.Is(err, io.EOF) {
		t.Fatalf("final read err = %v, want EOF", err)
	}
}
