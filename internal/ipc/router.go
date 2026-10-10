package ipc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Router maps method names to handlers. The same router serves in-process
// callers and socket clients, so the wire never defines a second API.
//
// Route retraction is safe against in-flight dispatches: a dispatch that
// has resolved a route holds a reference to it, and Unhandle and
// UnhandlePrefix wait for those references to drain before returning —
// bounded by routeRetractWait. Once either returns normally, no dispatch
// has or will reach a retracted handler. If the bound elapses first the
// routes are still retracted and can never be entered again, but a stale
// in-flight dispatch keeps its dead route reference and may still run (or
// hang); this keeps one wedged handler from blocking a cleanup path
// forever. Handlers run outside the router lock — a handler may register
// routes (application launch does exactly that) — but a handler must not
// synchronously retract its own route: retraction waits for the handler
// to return.
type Router struct {
	mu     sync.RWMutex
	routes map[string]*route
}

// route is one registered handler plus the count of in-flight dispatches.
// A dispatch counts itself while still under the router's read lock, so a
// retraction can never slip between resolving the route and counting the
// dispatch.
type route struct {
	h      Handler
	flight sync.WaitGroup
}

func NewRouter() *Router {
	return &Router{routes: map[string]*route{}}
}

// Handle registers a handler. Duplicate methods are rejected: routes are
// owned by exactly one subsystem or application instance.
func (r *Router) Handle(method string, h Handler) error {
	if method == "" || h == nil {
		return fmt.Errorf("ipc: invalid route %q", method)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.routes[method]; dup {
		return fmt.Errorf("ipc: route %q is already registered", method)
	}
	r.routes[method] = &route{h: h}
	return nil
}

// HandleBatch installs a route set atomically, rejecting any conflict without
// publishing partial application initialization.
func (r *Router) HandleBatch(routes map[string]Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for method, h := range routes {
		if method == "" || h == nil {
			return fmt.Errorf("ipc: invalid route %q", method)
		}
		if _, dup := r.routes[method]; dup {
			return fmt.Errorf("ipc: route %q is already registered", method)
		}
	}
	for method, h := range routes {
		r.routes[method] = &route{h: h}
	}
	return nil
}

// routeRetractWait bounds how long Unhandle and UnhandlePrefix wait for
// in-flight dispatches to release a retracted route. The wait exists so
// teardown code can free handler resources without racing a live dispatch;
// a handler that ignores its cancellation longer than this bound forfeits
// that guarantee (it keeps running on a route that is already gone).
const routeRetractWait = 2 * time.Second

// Unhandle removes one method and waits, up to routeRetractWait, for
// in-flight dispatches to it to complete.
func (r *Router) Unhandle(method string) {
	rt := r.remove(method)
	if rt != nil {
		waitFlights([]*route{rt}, routeRetractWait)
	}
}

// UnhandlePrefix removes every method with the prefix and waits, up to
// routeRetractWait total, for in-flight dispatches to any of them to
// complete; applications use it to retract all their routes ("app/<id>")
// on exit.
func (r *Router) UnhandlePrefix(prefix string) {
	r.mu.Lock()
	var removed []*route
	for method := range r.routes {
		if strings.HasPrefix(method, prefix) {
			removed = append(removed, r.routes[method])
			delete(r.routes, method)
		}
	}
	r.mu.Unlock()
	waitFlights(removed, routeRetractWait)
}

// waitFlights blocks until every route's in-flight dispatches drain or d
// elapses, whichever comes first. On expiry the routes stay retracted (no
// dispatch can ever reach them again) but the call returns anyway; the
// goroutine waiting on the flights then leaks along with the wedged
// handlers — bounded and rare, never fatal.
func waitFlights(rts []*route, d time.Duration) {
	if len(rts) == 0 || d <= 0 {
		return
	}
	done := make(chan struct{})
	go func() {
		for _, rt := range rts {
			rt.flight.Wait()
		}
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (r *Router) remove(method string) *route {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.routes[method]
	if !ok {
		return nil
	}
	delete(r.routes, method)
	return rt
}

// Methods lists registered method names, sorted.
func (r *Router) Methods() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.routes))
	for method := range r.routes {
		out = append(out, method)
	}
	sort.Strings(out)
	return out
}

// Dispatch routes one request. It never panics out and never returns a
// response with a mismatched ID.
func (r *Router) Dispatch(ctx context.Context, req Request) (resp Response) {
	resp.ID = req.ID
	defer func() {
		if p := recover(); p != nil {
			resp = Response{ID: req.ID, Error: fmt.Sprintf("internal error in %q: %v", req.Method, p)}
		}
	}()
	r.mu.RLock()
	rt, ok := r.routes[req.Method]
	if ok {
		rt.flight.Add(1)
	}
	r.mu.RUnlock()
	if !ok {
		return Response{ID: req.ID, Error: fmt.Sprintf("unknown method %q", req.Method)}
	}
	defer rt.flight.Done()

	data, err := rt.h(ctx, req)
	if err != nil {
		return Response{ID: req.ID, Error: err.Error()}
	}
	enc, err := Encode(data)
	if err != nil {
		return Response{ID: req.ID, Error: err.Error()}
	}
	return Response{ID: req.ID, OK: true, Data: enc}
}
