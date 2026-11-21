package ipc

import (
	"context"
	"fmt"
	"sync"

	"gostalgia/internal/events"
	"gostalgia/internal/security"
)

const (
	SubscribeMethod   = "events/v1/subscribe"
	UnsubscribeMethod = "events/v1/unsubscribe"
	HistoryMethod     = "events/v1/history"
)

type SubscribeParams struct {
	Epoch  string  `json:"epoch,omitempty"`
	Topic  string  `json:"topic"`
	Buffer int     `json:"buffer,omitempty"`
	After  *uint64 `json:"after,omitempty"`
}

type HistoryParams struct {
	Epoch string `json:"epoch,omitempty"`
	Topic string `json:"topic"`
	After uint64 `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type SubscriptionInfo struct {
	Epoch        string `json:"epoch"`
	Subscription uint64 `json:"subscription"`
	Cursor       uint64 `json:"cursor"`
	Oldest       uint64 `json:"oldest"`
	Resync       bool   `json:"resync"`
}

// Notification is distinct from Response: it has no request ID.
type Notification struct {
	Kind         string        `json:"kind"`
	Version      int           `json:"version"`
	Subscription uint64        `json:"subscription"`
	Event        events.Record `json:"event"`
	Dropped      uint64        `json:"dropped"`
}

type streamKey struct{}

// RegisterEvents installs admin-only routes. History also works in-process;
// subscribe/unsubscribe need the connection state supplied by Server.
func RegisterEvents(r *Router, bus *events.Bus) error {
	if bus == nil {
		return fmt.Errorf("ipc: event bus is required")
	}
	return r.HandleBatchPolicy(map[string]Handler{
		HistoryMethod: func(ctx context.Context, req Request) (any, error) {
			var p HistoryParams
			if err := DecodeParams(req.Params, &p); err != nil {
				return nil, err
			}
			if p.After != 0 && p.Epoch == "" {
				return nil, fmt.Errorf("ipc: cursor requires an epoch")
			}
			reset := p.Epoch != "" && p.Epoch != bus.Epoch()
			if reset {
				p.After = 0
			}
			history, err := bus.Recent(p.Topic, p.After, p.Limit)
			history.Resync = history.Resync || reset
			return history, err
		},
		SubscribeMethod: func(ctx context.Context, req Request) (any, error) {
			stream, _ := ctx.Value(streamKey{}).(*eventStream)
			if stream == nil {
				return nil, fmt.Errorf("ipc: subscriptions require a socket connection")
			}
			var p SubscribeParams
			if err := DecodeParams(req.Params, &p); err != nil {
				return nil, err
			}
			stream.mu.Lock()
			defer stream.mu.Unlock()
			if stream.closed || ctx.Err() != nil {
				return nil, fmt.Errorf("ipc: connection closed")
			}
			if stream.sub != nil {
				return nil, fmt.Errorf("ipc: connection already has a subscription")
			}
			if p.After != nil && p.Epoch == "" {
				return nil, fmt.Errorf("ipc: cursor requires an epoch")
			}
			reset := p.Epoch != "" && p.Epoch != bus.Epoch()
			if reset && p.After != nil {
				zero := uint64(0)
				p.After = &zero
			}
			sub, history, err := bus.SubscribeBuffered(p.Topic, p.Buffer, p.After)
			if err != nil {
				return nil, err
			}
			stream.sub = sub
			return SubscriptionInfo{
				Epoch: history.Epoch, Subscription: sub.ID, Cursor: history.Cursor,
				Oldest: history.Oldest, Resync: history.Resync || reset,
			}, nil
		},
		UnsubscribeMethod: func(ctx context.Context, req Request) (any, error) {
			var p struct {
				Subscription uint64 `json:"subscription"`
			}
			if err := DecodeParams(req.Params, &p); err != nil {
				return nil, err
			}
			stream, _ := ctx.Value(streamKey{}).(*eventStream)
			if stream == nil {
				return nil, fmt.Errorf("ipc: subscriptions require a socket connection")
			}
			stream.mu.Lock()
			defer stream.mu.Unlock()
			if stream.sub == nil || stream.sub.ID != p.Subscription {
				return nil, fmt.Errorf("ipc: unknown subscription")
			}
			stream.sub.Close()
			stream.sub = nil
			return nil, nil
		},
	}, Cap(security.CapAdmin))
}

// eventStream is connection-scoped. Its queue remains in the bus so publishers
// only enqueue metadata; they never touch a socket or wait on transport.
type eventStream struct {
	mu     sync.Mutex
	sub    *events.Subscription
	closed bool
}

func (s *eventStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.sub != nil {
		s.sub.Close()
		s.sub = nil
	}
}

func (s *eventStream) current() *events.Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sub
}
