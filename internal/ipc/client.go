package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"gostalgia/internal/events"
)

const defaultCallTimeout = 30 * time.Second

type callResult struct {
	response Response
	sub      *ClientSubscription
	err      error
}

type pendingCall struct {
	result chan callResult
	method string
	buffer int
}

type clientWrite struct {
	frame []byte
	ctx   context.Context // nil for fire-and-forget control frames (cancels)
}

// Client multiplexes calls by ID. One reader demultiplexes responses/events
// and one writer owns framing. Neither waits for an event consumer.
type Client struct {
	conn        net.Conn
	mu          sync.Mutex
	nextID      int64
	pending     map[int64]*pendingCall
	err         error
	done        chan struct{}
	writes      chan clientWrite
	wg          sync.WaitGroup
	sub         *ClientSubscription
	subscribing bool
}

func NewClient(conn net.Conn, token string) (*Client, error) {
	c := &Client{
		conn: conn, pending: make(map[int64]*pendingCall),
		done: make(chan struct{}), writes: make(chan clientWrite, maxInFlight),
	}
	c.wg.Add(2)
	go c.readLoop()
	go c.writeLoop()
	if _, err := c.call(context.Background(), "auth", map[string]string{"token": token}, nil, 0); err != nil {
		c.Close()
		return nil, fmt.Errorf("ipc: auth failed: %w", err)
	}
	return c, nil
}

// Call honors context cancellation, including while queued. A canceled call
// does not invalidate other calls: late responses are discarded by ID.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	if method == SubscribeMethod {
		return errors.New("ipc: use Client.Subscribe for event delivery")
	}
	_, err := c.call(ctx, method, params, out, 0)
	return err
}

func (c *Client) call(ctx context.Context, method string, params, out any, buffer int) (*ClientSubscription, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCallTimeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = Encode(params)
		if err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	if len(c.pending) >= maxInFlight {
		c.mu.Unlock()
		return nil, errors.New("ipc: too many outstanding calls")
	}
	c.nextID++
	id := c.nextID
	pending := &pendingCall{result: make(chan callResult, 1), method: method, buffer: buffer}
	c.pending[id] = pending
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	frame, err := json.Marshal(Request{ID: id, Method: method, Params: raw})
	if err != nil {
		return nil, fmt.Errorf("ipc: encode request: %w", err)
	}
	select {
	case c.writes <- clientWrite{frame, ctx}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("ipc: connection closed")
	}
	select {
	case <-ctx.Done():
		c.sendCancel(id)
		return nil, ctx.Err()
	case result := <-pending.result:
		if result.err != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if d, ok := ctx.Deadline(); ok && (errors.Is(result.err, os.ErrDeadlineExceeded) || !time.Now().Before(d)) {
				return nil, context.DeadlineExceeded
			}
			return nil, result.err
		}
		if !result.response.OK {
			return nil, errors.New(result.response.Error)
		}
		if out != nil && len(result.response.Data) > 0 {
			if err := json.Unmarshal(result.response.Data, out); err != nil {
				return nil, err
			}
		}
		return result.sub, nil
	case <-ctx.Done():
		c.sendCancel(id)
		return nil, ctx.Err()
	}
}

// sendCancel enqueues a fire-and-forget cancel frame so the server can
// release the abandoned call's in-flight slot and stop its handler
// context. Best-effort: a dying connection drops it silently.
func (c *Client) sendCancel(id int64) {
	frame, err := json.Marshal(cancelFrame{Kind: kindCancel, ID: id})
	if err != nil {
		return
	}
	timer := time.NewTimer(writeTimeout)
	defer timer.Stop()
	select {
	case c.writes <- clientWrite{frame: frame}:
	case <-c.done:
	case <-timer.C:
	}
}

func (c *Client) writeLoop() {
	defer c.wg.Done()
	w := bufio.NewWriter(c.conn)
	for {
		var item clientWrite
		select {
		case <-c.done:
			return
		case item = <-c.writes:
		}
		var deadline time.Time
		var hasDeadline bool
		var stop func() bool
		var fired chan struct{}
		if item.ctx != nil {
			if item.ctx.Err() != nil {
				continue
			}
			deadline, hasDeadline = item.ctx.Deadline()
			fired = make(chan struct{})
			stop = context.AfterFunc(item.ctx, func() {
				_ = c.conn.SetWriteDeadline(time.Now())
				close(fired)
			})
		} else {
			// Control frames carry no caller context; bound their write so
			// a wedged socket cannot stall the writer forever.
			deadline, hasDeadline = time.Now().Add(writeTimeout), true
		}
		_ = c.conn.SetWriteDeadline(deadline)
		err := writeRawFrame(w, item.frame)
		if stop != nil && !stop() {
			<-fired
		}
		if err != nil {
			if item.ctx != nil && item.ctx.Err() != nil {
				c.fail(fmt.Errorf("ipc: write: %w", item.ctx.Err()))
			} else if hasDeadline && (errors.Is(err, os.ErrDeadlineExceeded) || !time.Now().Before(deadline)) {
				c.fail(fmt.Errorf("ipc: write: %w", context.DeadlineExceeded))
			} else {
				// A partial write cannot be safely resumed as another frame.
				c.fail(fmt.Errorf("ipc: write: %w", err))
			}
			return
		}
	}
}

func writeRawFrame(w *bufio.Writer, frame []byte) error {
	if len(frame)+1 >= maxLine {
		return errFrameTooLarge
	}
	if _, err := w.Write(frame); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	r := bufio.NewScanner(c.conn)
	r.Split(scanNDJSON)
	r.Buffer(make([]byte, 64*1024), maxLine)
	for r.Scan() {
		var frame struct {
			Kind string `json:"kind"`
			ID   *int64 `json:"id"`
			OK   *bool  `json:"ok"`
		}
		if err := json.Unmarshal(r.Bytes(), &frame); err != nil {
			c.fail(fmt.Errorf("ipc: bad frame: %w", err))
			return
		}
		if frame.Kind != "" {
			var n Notification
			if err := json.Unmarshal(r.Bytes(), &n); err != nil || frame.Kind != "event" ||
				n.Version != 1 || n.Subscription == 0 || n.Event.ID == 0 || frame.ID != nil {
				c.fail(errors.New("ipc: malformed event frame"))
				return
			}
			c.mu.Lock()
			if sub := c.sub; sub != nil && sub.Info.Subscription == n.Subscription {
				if n.Event.ID <= sub.lastID {
					c.mu.Unlock()
					c.fail(errors.New("ipc: out-of-order event frame"))
					return
				}
				sub.lastID = n.Event.ID
				sub.enqueue(n)
			}
			c.mu.Unlock()
			continue
		}
		if frame.ID == nil || *frame.ID <= 0 || frame.OK == nil {
			c.fail(errors.New("ipc: malformed response frame"))
			return
		}
		var resp Response
		if err := json.Unmarshal(r.Bytes(), &resp); err != nil {
			c.fail(fmt.Errorf("ipc: bad response: %w", err))
			return
		}
		c.mu.Lock()
		pending := c.pending[resp.ID]
		if pending == nil {
			c.mu.Unlock()
			continue // a canceled call's late response
		}
		result := callResult{response: resp}
		if resp.OK && pending.method == SubscribeMethod {
			var info SubscriptionInfo
			if err := json.Unmarshal(resp.Data, &info); err != nil || info.Subscription == 0 || c.sub != nil {
				c.mu.Unlock()
				c.fail(errors.New("ipc: malformed subscribe acknowledgement"))
				return
			}
			q := make(chan Notification, pending.buffer)
			c.sub = &ClientSubscription{Info: info, Events: q, queue: q, client: c}
			result.sub = c.sub
		} else if resp.OK && pending.method == UnsubscribeMethod && c.sub != nil {
			close(c.sub.queue)
			c.sub = nil
		}
		delete(c.pending, resp.ID)
		pending.result <- result
		c.mu.Unlock()
	}
	err := r.Err()
	if err == nil {
		err = errors.New("connection closed")
	}
	c.fail(fmt.Errorf("ipc: read: %w", err))
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	close(c.done)
	for id, pending := range c.pending {
		pending.result <- callResult{err: err}
		delete(c.pending, id)
	}
	if c.sub != nil {
		close(c.sub.queue)
		c.sub = nil
	}
	c.mu.Unlock()
	c.conn.Close()
}

func (c *Client) Close() error {
	c.fail(errors.New("ipc: client closed"))
	c.wg.Wait()
	return nil
}

// Done returns a channel that is closed when the connection drops or the client closes.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// ClientSubscription is connection-scoped and bounded. Info describes the
// initial cursor/replay status. Events reports total server + local drops.
// A closed Events channel means unsubscribe/disconnect, not a clean replay.
type ClientSubscription struct {
	Info    SubscriptionInfo
	Events  <-chan Notification
	client  *Client
	queue   chan Notification
	dropped uint64
	lastID  uint64
}

func (s *ClientSubscription) enqueue(n Notification) {
	n.Dropped += s.dropped
	select {
	case s.queue <- n:
		return
	default:
	}
	select {
	case <-s.queue:
		s.dropped++
		n.Dropped++
	default:
	}
	s.queue <- n
}

func (c *Client) Subscribe(ctx context.Context, p SubscribeParams) (*ClientSubscription, error) {
	buffer := p.Buffer
	if buffer == 0 {
		buffer = events.DefaultBuffer
	}
	if buffer < 1 || buffer > events.BufferLimit {
		return nil, fmt.Errorf("ipc: buffer must be 1..%d", events.BufferLimit)
	}
	c.mu.Lock()
	if c.sub != nil || c.subscribing {
		c.mu.Unlock()
		return nil, errors.New("ipc: connection already has a subscription")
	}
	c.subscribing = true
	c.mu.Unlock()
	sub, err := c.call(ctx, SubscribeMethod, p, nil, buffer)
	c.mu.Lock()
	c.subscribing = false
	c.mu.Unlock()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The server may have subscribed before cancellation. Disconnect
		// rather than leaving an unowned subscription on this connection.
		c.Close()
	}
	return sub, err
}

func (s *ClientSubscription) Close(ctx context.Context) error {
	err := s.client.Call(ctx, UnsubscribeMethod, map[string]uint64{"subscription": s.Info.Subscription}, nil)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.client.Close()
	}
	return err
}
