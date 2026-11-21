package ipc

import (
	"context"
	"sync"
)

// connReaperKey is the context key for a connection's close reaper.
type connReaperKey struct{}

// connReaper runs registered callbacks once, when its connection closes.
// Handlers attach connection-bound state — session attachments — through
// OnConnClose so a client that drops without releasing anything cannot
// leak the slot for the life of the runtime.
type connReaper struct {
	mu   sync.Mutex
	fns  []func()
	done bool
}

// connReaperOf returns the reaper bound to ctx, or nil for callers that
// did not arrive over a connection (in-process dispatch).
func connReaperOf(ctx context.Context) *connReaper {
	r, _ := ctx.Value(connReaperKey{}).(*connReaper)
	return r
}

func (r *connReaper) add(fn func()) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		fn()
		return
	}
	r.fns = append(r.fns, fn)
	r.mu.Unlock()
}

func (r *connReaper) run() {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	fns := r.fns
	r.fns = nil
	r.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// OnConnClose registers fn to run once, when the IPC connection serving
// ctx closes. If the connection is already closed, fn runs immediately.
// It is a no-op for in-process callers, which have no connection.
func OnConnClose(ctx context.Context, fn func()) {
	if r := connReaperOf(ctx); r != nil {
		r.add(fn)
	}
}

// BindReaper returns a context carrying a fresh reaper plus the function
// that runs it; run is idempotent. Non-socket channels that dispatch
// in-process — app child wires — use it to give their handlers the same
// release-on-disconnect contract connections get for free.
func BindReaper(ctx context.Context) (context.Context, func()) {
	r := &connReaper{}
	return context.WithValue(ctx, connReaperKey{}, r), r.run
}
