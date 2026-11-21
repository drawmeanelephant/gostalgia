package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ServeOptions configures execution of an external application instance.
type ServeOptions struct {
	Endpoint string
	Token    string
	AppID    string
	Log      *slog.Logger
}

// Serve runs an external application instance using the environment protocol.
// It resolves the channel and credentials from GOSTALGIA_IPC_FD (an inherited
// pre-connected descriptor used for sandboxed launches) or GOSTALGIA_ENDPOINT
// (a dialable listener address), plus GOSTALGIA_APP_TOKEN (or GOSTALGIA_TOKEN),
// performs the startup handshake, declares routes, and runs the instance until
// termination.
func Serve(inst Instance) error {
	return ServeWithOptions(context.Background(), inst, ServeOptions{})
}

// ServeContext runs an external application instance honoring ctx cancellation.
func ServeContext(ctx context.Context, inst Instance) error {
	return ServeWithOptions(ctx, inst, ServeOptions{})
}

type rpcMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// maxWireFrame bounds one newline-delimited wire frame, matching the
// runtime's 6 MiB line cap; an oversized frame kills the connection instead
// of buffering without bound. maxInFlight bounds concurrent
// environment-initiated handler calls, matching the runtime's per-connection
// in-flight limit.
const (
	maxWireFrame = 6 << 20
	maxInFlight  = 32
)

var errWireFrameTooLarge = errors.New("sdk: wire frame exceeds 6 MiB limit")

// readWireLine reads one newline-delimited frame, aborting once it exceeds
// maxWireFrame. Unlike ReadBytes it cannot buffer an unterminated line
// without bound.
func readWireLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if err == bufio.ErrBufferFull {
			if len(line) > maxWireFrame {
				return nil, errWireFrameTooLarge
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(line) > maxWireFrame {
			return nil, errWireFrameTooLarge
		}
		return line, nil
	}
}

// ServeWithOptions runs an external application instance with explicit options.
func ServeWithOptions(ctx context.Context, inst Instance, opts ServeOptions) error {
	if inst == nil {
		return errors.New("sdk: instance is required")
	}
	if opts.Endpoint == "" {
		opts.Endpoint = os.Getenv("GOSTALGIA_ENDPOINT")
	}
	ipcFD := os.Getenv("GOSTALGIA_IPC_FD")
	if opts.Token == "" {
		opts.Token = os.Getenv("GOSTALGIA_APP_TOKEN")
	}
	if opts.Token == "" {
		opts.Token = os.Getenv("GOSTALGIA_TOKEN")
	}
	if opts.AppID == "" {
		opts.AppID = os.Getenv("GOSTALGIA_APP_ID")
	}
	if ipcFD == "" && opts.Endpoint == "" {
		return errors.New("sdk: channel is required (set GOSTALGIA_IPC_FD or GOSTALGIA_ENDPOINT)")
	}
	if opts.Token == "" {
		return errors.New("sdk: token is required (set GOSTALGIA_APP_TOKEN)")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	var conn net.Conn
	var err error
	switch {
	case ipcFD != "":
		// The runtime handed us a pre-connected channel as an inherited
		// descriptor. This works under network-denying sandboxes because no
		// connect() is ever performed.
		fd, convErr := strconv.Atoi(ipcFD)
		if convErr != nil || fd < 0 {
			return fmt.Errorf("sdk: invalid GOSTALGIA_IPC_FD %q", ipcFD)
		}
		f := os.NewFile(uintptr(fd), "gostalgia-ipc")
		conn, err = net.FileConn(f)
		_ = f.Close() // FileConn dups the descriptor
	case strings.HasPrefix(opts.Endpoint, "unix://"):
		conn, err = net.Dial("unix", strings.TrimPrefix(opts.Endpoint, "unix://"))
	case strings.HasPrefix(opts.Endpoint, "tcp://"):
		conn, err = net.Dial("tcp", strings.TrimPrefix(opts.Endpoint, "tcp://"))
	default:
		return fmt.Errorf("sdk: unsupported endpoint scheme %q", opts.Endpoint)
	}
	if err != nil {
		return fmt.Errorf("sdk: connect: %w", err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	var writeMu sync.Mutex
	writeMsg := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		_, err = conn.Write(b)
		return err
	}

	// 1. Send auth request
	authParams, _ := json.Marshal(map[string]string{"token": opts.Token})
	if err := writeMsg(rpcMessage{ID: 1, Method: "auth", Params: authParams}); err != nil {
		return fmt.Errorf("sdk: write auth: %w", err)
	}
	line, err := readWireLine(reader)
	if err != nil {
		return fmt.Errorf("sdk: read auth response: %w", err)
	}
	var authResp rpcMessage
	if err := json.Unmarshal(line, &authResp); err != nil {
		return fmt.Errorf("sdk: unmarshal auth response: %w", err)
	}
	if !authResp.OK {
		return &Error{Code: authResp.Code, Message: fmt.Sprintf("sdk: auth failed: %s", authResp.Error)}
	}

	// 2. Setup Context and initialize instance
	routes := make(map[string]Handler)
	var routesMu sync.RWMutex
	var nextID int64 = 2 // 1 was auth, 2 will be ready, 3+ for runtime calls
	var pendingMu sync.Mutex
	pendingCalls := make(map[int64]chan rpcMessage)

	call := func(c context.Context, method string, params, out any) error {
		var raw json.RawMessage
		if params != nil {
			var err error
			raw, err = json.Marshal(params)
			if err != nil {
				return err
			}
		}
		resCh := make(chan rpcMessage, 1)
		pendingMu.Lock()
		nextID++
		reqID := nextID
		pendingCalls[reqID] = resCh
		pendingMu.Unlock()

		defer func() {
			pendingMu.Lock()
			delete(pendingCalls, reqID)
			pendingMu.Unlock()
		}()

		req := rpcMessage{ID: reqID, Method: method, Params: raw}
		// An oversized request fails only this call, before any byte is
		// written: the runtime would drop the connection otherwise.
		if b, err := json.Marshal(req); err != nil {
			return err
		} else if len(b)+1 >= maxWireFrame {
			return errWireFrameTooLarge
		}
		if err := writeMsg(req); err != nil {
			return err
		}

		select {
		case <-c.Done():
			return c.Err()
		case <-ctx.Done():
			return ctx.Err()
		case res := <-resCh:
			if !res.OK {
				return &Error{Code: res.Code, Message: res.Error}
			}
			if out != nil && len(res.Data) > 0 && !bytes.Equal(res.Data, []byte("null")) {
				return json.Unmarshal(res.Data, out)
			}
			return nil
		}
	}

	initializing := true
	handle := func(name string, h Handler) error {
		routesMu.Lock()
		defer routesMu.Unlock()
		if !initializing {
			return errors.New("app: routes may only be declared during Init")
		}
		if _, dup := routes[name]; dup {
			return fmt.Errorf("app: duplicate route %q", name)
		}
		routes[name] = h
		return nil
	}

	manifest := Manifest{ID: opts.AppID, Mode: ModeExternal, ProtocolVersion: ProtocolVersion}
	sdkCtx := NewContext(manifest, opts.Log, call, handle)

	if err := inst.Init(sdkCtx); err != nil {
		return fmt.Errorf("sdk: init: %w", err)
	}
	routesMu.Lock()
	initializing = false
	routesMu.Unlock()

	// 3. Send app/ready
	routeNames := make([]string, 0, len(routes))
	for name := range routes {
		routeNames = append(routeNames, name)
	}
	sort.Strings(routeNames)
	readyParams, _ := json.Marshal(map[string]any{
		"protocol_version": ProtocolVersion,
		"app_id":           opts.AppID,
		"routes":           routeNames,
	})
	if err := writeMsg(rpcMessage{ID: 2, Method: "app/ready", Params: readyParams}); err != nil {
		return fmt.Errorf("sdk: write ready: %w", err)
	}
	line, err = readWireLine(reader)
	if err != nil {
		return fmt.Errorf("sdk: read ready response: %w", err)
	}
	var readyResp rpcMessage
	if err := json.Unmarshal(line, &readyResp); err != nil {
		return fmt.Errorf("sdk: unmarshal ready response: %w", err)
	}
	if !readyResp.OK {
		return &Error{Code: readyResp.Code, Message: fmt.Sprintf("sdk: ready failed: %s", readyResp.Error)}
	}

	// 4. Multiplex requests and run instance
	appCtx, appCancel := context.WithCancel(ctx)
	defer appCancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			appCancel()
		case <-appCtx.Done():
		}
	}()

	// In-flight bound mirrors the runtime side: overflow is rejected with an
	// error response rather than queued on the read loop, which must keep
	// draining the connection because call responses share it.
	inFlight := make(chan struct{}, maxInFlight)
	go func() {
		for {
			l, err := readWireLine(reader)
			if err != nil {
				appCancel()
				return
			}
			l = bytes.TrimSpace(l)
			if len(l) == 0 {
				continue
			}
			var msg rpcMessage
			if err := json.Unmarshal(l, &msg); err != nil {
				continue
			}
			if msg.Method != "" {
				// Incoming request from environment
				select {
				case inFlight <- struct{}{}:
				default:
					_ = writeMsg(rpcMessage{ID: msg.ID, OK: false, Error: "sdk: too many in-flight requests", Code: CodeUnavailable})
					continue
				}
				go func(req rpcMessage) {
					defer func() { <-inFlight }()
					routesMu.RLock()
					h, ok := routes[req.Method]
					routesMu.RUnlock()
					if !ok {
						_ = writeMsg(rpcMessage{ID: req.ID, OK: false, Error: fmt.Sprintf("unknown method %q", req.Method), Code: CodeUnknownMethod})
						return
					}
					res, hErr := h(appCtx, req.Params)
					if hErr != nil {
						code := ErrorCode(hErr)
						if code == "" {
							code = CodeUnknown
						}
						_ = writeMsg(rpcMessage{ID: req.ID, OK: false, Error: hErr.Error(), Code: code})
						return
					}
					var rawData json.RawMessage
					if res != nil {
						rawData, _ = json.Marshal(res)
					}
					_ = writeMsg(rpcMessage{ID: req.ID, OK: true, Data: rawData})
				}(msg)
			} else {
				// Incoming response to our call
				pendingMu.Lock()
				ch, ok := pendingCalls[msg.ID]
				pendingMu.Unlock()
				if ok {
					select {
					case ch <- msg:
					default:
					}
				}
			}
		}
	}()

	runErr := inst.Run(appCtx)
	appCancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	stopErr := inst.Stop(stopCtx)

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return stopErr
}
