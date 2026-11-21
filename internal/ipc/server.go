package ipc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"gostalgia/internal/events"
	"gostalgia/internal/security"
)

// maxLine bounds one protocol line (request or response). It is sized so a
// maximum service payload — 4 MiB of raw data base64-encoded plus the JSON
// envelope — fits end-to-end; a 4 MiB frame cap would make the documented
// payload limit unreachable.
const maxLine = 6 << 20 // 6 MiB

var errFrameTooLarge = errors.New("ipc: frame exceeds limit")

const (
	maxInFlight      = 32
	writeTimeout     = 5 * time.Second
	handshakeTimeout = 5 * time.Second
	maxConnections   = 256
)

// ServerOption configures Server parameters.
type ServerOption func(*Server)

// WithHandshakeTimeout configures the budget for handshake completion.
func WithHandshakeTimeout(d time.Duration) ServerOption {
	return func(s *Server) { s.handshakeTimeout = d }
}

// WithWriteTimeout configures the deadline for socket writes.
func WithWriteTimeout(d time.Duration) ServerOption {
	return func(s *Server) { s.writeTimeout = d }
}

// WithMaxConnections sets the maximum number of concurrent open connections.
func WithMaxConnections(n int) ServerOption {
	return func(s *Server) { s.maxConns = n }
}

// WithMaxInFlight sets the maximum number of concurrent in-flight requests per connection.
func WithMaxInFlight(n int) ServerOption {
	return func(s *Server) { s.maxInFlight = n }
}

// Authenticator authenticates and validates connection credentials.
type Authenticator interface {
	// Authenticate verifies the handshake token, returning the bound principal and capabilities.
	Authenticate(token string) (security.Principal, *security.Capabilities, error)
	// Validate checks whether an already-authenticated token remains valid and unrevoked.
	Validate(token string) error
}

type staticAuthenticator struct {
	token string
}

func (s staticAuthenticator) Authenticate(token string) (security.Principal, *security.Capabilities, error) {
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		return security.Principal{}, nil, errors.New("unauthorized: bad token")
	}
	return security.OperatorPrincipal(security.User{Name: "operator"}), security.AdminCapabilities(), nil
}

func (s staticAuthenticator) Validate(token string) error {
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		return errors.New("unauthorized: bad token")
	}
	return nil
}

type queuedResponse struct {
	response   Response
	disconnect bool
}

// inFlightCall tracks one dispatched request: its method (for subscription
// activation) and the cancel func of its per-request context, invoked when
// the peer abandons the call.
type inFlightCall struct {
	method string
	cancel context.CancelFunc
}

// Server serves the router over a local listener. Every connection must
// authenticate before any other method is accepted; authenticated connections
// receive the capabilities and principal bound to their credential.
type Server struct {
	ln     net.Listener
	router *Router
	auth   Authenticator
	log    *slog.Logger

	handshakeTimeout time.Duration
	writeTimeout     time.Duration
	maxConns         int
	maxInFlight      int

	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func NewServer(ln net.Listener, router *Router, auth any, log *slog.Logger, opts ...ServerOption) *Server {
	if log == nil {
		log = slog.Default()
	}
	var a Authenticator
	switch v := auth.(type) {
	case Authenticator:
		a = v
	case string:
		a = staticAuthenticator{token: v}
	default:
		panic(fmt.Sprintf("ipc: unsupported auth type %T", auth))
	}
	s := &Server{
		ln:               ln,
		router:           router,
		auth:             a,
		log:              log,
		handshakeTimeout: handshakeTimeout,
		writeTimeout:     writeTimeout,
		maxConns:         maxConnections,
		maxInFlight:      maxInFlight,
		conns:            map[net.Conn]struct{}{},
		done:             make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Serve accepts connections until Close is called. It returns nil after
// an intentional close.
func (s *Server) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				return fmt.Errorf("ipc: accept: %w", err)
			}
		}
		// Track and arm the connection while holding the same mutex
		// Close sweeps s.conns under, and re-check done inside it. A
		// connection accepted concurrently with Close can then never
		// escape being closed, and wg.Add can never race wg.Wait from
		// a zero counter.
		s.mu.Lock()
		select {
		case <-s.done:
			s.mu.Unlock()
			conn.Close()
			return nil
		default:
		}
		if s.maxConns > 0 && len(s.conns) >= s.maxConns {
			active := len(s.conns)
			s.mu.Unlock()
			s.log.Warn("ipc: connection budget exceeded, rejecting connection", "remote", conn.RemoteAddr(), "active", active, "max", s.maxConns)
			conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

// Close stops the listener and drops all live connections, including
// idle ones — shutdown must never hang on a connected client, and it
// never waits on a connection it did not close itself.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.closeErr = s.ln.Close()
		s.mu.Lock()
		for conn := range s.conns {
			conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s.closeErr
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	conn.Close()
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer s.untrack(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := bufio.NewScanner(conn)
	r.Split(scanNDJSON)
	r.Buffer(make([]byte, 64*1024), maxLine)
	w := bufio.NewWriter(conn)
	_ = conn.SetDeadline(time.Now().Add(s.handshakeTimeout))

	// Handshake: the first request must be auth.
	req, err := readRequest(r)
	if err != nil {
		return
	}
	if req.Method != "auth" {
		_ = writeResponse(w, Response{ID: req.ID, Error: "unauthorized: first request must be auth"})
		return
	}
	var p struct {
		Token string `json:"token"`
	}
	if err := DecodeParams(req.Params, &p); err != nil {
		_ = writeResponse(w, Response{ID: req.ID, Error: err.Error()})
		return
	}
	principal, caps, err := s.auth.Authenticate(p.Token)
	if err != nil {
		s.log.Warn("ipc: rejected connection with invalid token", "remote", conn.RemoteAddr(), "err", err)
		_ = writeResponse(w, Response{ID: req.ID, Error: err.Error()})
		return
	}
	if err := writeResponse(w, Response{ID: req.ID, OK: true}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	authCtx := WithPrincipal(WithCapabilities(ctx, caps), principal)
	token := p.Token
	// Connection-bound state registered by handlers (session attachments)
	// is reclaimed here no matter how the connection ends — clean quit,
	// crash, or revoked credential.
	reaper := &connReaper{}
	defer reaper.run()
	authCtx = context.WithValue(authCtx, connReaperKey{}, reaper)
	stream := &eventStream{}
	defer stream.close()
	authCtx = context.WithValue(authCtx, streamKey{}, stream)
	responses := make(chan queuedResponse, s.maxInFlight)
	var flightMu sync.Mutex
	active := make(map[int64]inFlightCall)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer cancel()
		defer conn.Close()
		var sub *events.Subscription
		for {
			if sub != nil && stream.current() != sub {
				sub = nil
			}
			var queue <-chan events.Delivery
			if sub != nil {
				queue = sub.Events
			}
			var reply queuedResponse
			var delivery events.Delivery
			isResponse := false
			// Replies have priority over queued notifications. Only this
			// goroutine writes, so lines cannot interleave.
			select {
			case reply = <-responses:
				isResponse = true
			default:
				select {
				case <-ctx.Done():
					return
				case reply = <-responses:
					isResponse = true
				case item, ok := <-queue:
					if !ok {
						sub = nil
						continue
					}
					delivery = item
				}
			}
			resp := reply.response
			disconnect := reply.disconnect || s.auth.Validate(token) != nil
			if disconnect {
				if !isResponse {
					return
				}
				resp = Response{ID: resp.ID, Error: "unauthorized: credential revoked"}
			}
			_ = conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
			if isResponse {
				flightMu.Lock()
				call, tracked := active[resp.ID]
				delete(active, resp.ID)
				flightMu.Unlock()
				if !tracked {
					// The peer canceled while the handler ran and already
					// discarded the call; skip the stale reply.
					continue
				}
				if err := writeResponse(w, resp); err != nil {
					return
				}
				if disconnect {
					return
				}
				// Activate delivery only after the subscribe acknowledgement.
				var info SubscriptionInfo
				if call.method == SubscribeMethod && resp.OK && json.Unmarshal(resp.Data, &info) == nil && info.Subscription != 0 {
					current := stream.current()
					if current != nil && current.ID == info.Subscription {
						sub = current
					}
				}
			} else if stream.current() == sub {
				if err := writeFrame(w, Notification{
					Kind: "event", Version: 1, Subscription: sub.ID,
					Event: delivery.Event, Dropped: delivery.Dropped,
				}); err != nil {
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		conn.Close()
		<-writerDone
	}()
	for {
		req, err := readRequest(r)
		if err != nil {
			return
		}
		if req.kind == kindCancel {
			// Fire-and-forget cancellation: free the slot and stop the
			// handler's request context. A reply produced after this
			// point is dropped by the writer — the peer already
			// discarded the call.
			flightMu.Lock()
			if call, ok := active[req.ID]; ok {
				delete(active, req.ID)
				call.cancel()
			}
			flightMu.Unlock()
			continue
		}
		flightMu.Lock()
		if _, dup := active[req.ID]; dup || len(active) >= s.maxInFlight {
			flightMu.Unlock()
			// Duplicate IDs and floods are protocol violations. Closing
			// bounds goroutines, response memory and work per connection.
			return
		}
		reqCtx, reqCancel := context.WithCancel(authCtx)
		active[req.ID] = inFlightCall{method: req.Method, cancel: reqCancel}
		flightMu.Unlock()
		go func(req Request, reqCtx context.Context, reqCancel context.CancelFunc) {
			defer reqCancel()
			reply := queuedResponse{}
			if err := s.auth.Validate(token); err != nil {
				s.log.Warn("ipc: connection credential invalidated", "remote", conn.RemoteAddr(), "err", err)
				reply.response = Response{ID: req.ID, Error: "unauthorized: credential revoked"}
				reply.disconnect = true
			} else {
				if curPrincipal, _, err := s.auth.Authenticate(token); err == nil {
					reqCtx = WithPrincipal(reqCtx, curPrincipal)
				}
				reply.response = s.router.Dispatch(reqCtx, req)
			}
			select {
			case responses <- reply:
			case <-ctx.Done():
			}
		}(req, reqCtx, reqCancel)
	}
}

func readRequest(r *bufio.Scanner) (Request, error) {
	if !r.Scan() {
		err := r.Err()
		if err == nil {
			err = errors.New("connection closed")
		}
		return Request{}, err
	}
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(r.Bytes(), &head); err != nil {
		return Request{}, fmt.Errorf("ipc: bad request: %w", err)
	}
	var req Request
	if head.Kind != "" {
		// Control frames carry no method; the only kind today is cancel.
		if err := json.Unmarshal(r.Bytes(), &req); err != nil ||
			head.Kind != kindCancel || req.ID <= 0 || req.Method != "" || len(req.Params) > 0 {
			return Request{}, errors.New("ipc: malformed cancel frame")
		}
		req.kind = kindCancel
		return req, nil
	}
	if err := json.Unmarshal(r.Bytes(), &req); err != nil {
		return Request{}, fmt.Errorf("ipc: bad request: %w", err)
	}
	if req.ID <= 0 || req.Method == "" {
		return Request{}, errors.New("ipc: request requires a positive id and method")
	}
	return req, nil
}

// Unlike ScanLines, a partial final frame is not accepted at EOF.
func scanNDJSON(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, bytes.TrimSuffix(data[:i], []byte{'\r'}), nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("ipc: truncated frame")
	}
	return 0, nil, nil
}

func writeResponse(w *bufio.Writer, resp Response) error {
	return writeFrame(w, resp)
}

func writeFrame(w *bufio.Writer, frame any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(b)+1 >= maxLine {
		if resp, ok := frame.(Response); ok {
			b, _ = json.Marshal(Response{ID: resp.ID, Error: "ipc: response exceeds frame limit"})
		} else {
			return errFrameTooLarge
		}
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}
