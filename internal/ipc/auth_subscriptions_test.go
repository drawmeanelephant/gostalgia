package ipc

import (
	"context"
	"net"
	"strings"
	"testing"

	"gostalgia/internal/events"
	"gostalgia/internal/security"
)

func authenticatedEventServer(t *testing.T) (*Server, *events.Bus, *Router, *security.TokenStore, string) {
	t.Helper()
	bus, r := events.NewBus(), NewRouter()
	must(t, RegisterEvents(r, bus))
	tokens := security.NewTokenStore()
	user := security.User{ID: "u-test", Name: "tester"}
	must(t, tokens.RegisterOperator("operator-token", user))
	appToken, err := tokens.IssueAppToken("test.app", 42, "test-session", user, security.CapIPC)
	must(t, err)
	must(t, r.Handle("test/identity", func(ctx context.Context, req Request) (any, error) {
		return CallerPrincipal(ctx), nil
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	s := NewServer(ln, r, tokens, nil)
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return s, bus, r, tokens, appToken
}

func authenticatedEventClient(t *testing.T, s *Server, token string) *Client {
	t.Helper()
	conn, err := net.Dial("tcp", s.ln.Addr().String())
	must(t, err)
	c, err := NewClient(conn, token)
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestScopedTokensCannotAccessOperatorEventTrail(t *testing.T) {
	s, bus, _, _, appToken := authenticatedEventServer(t)
	ctx := testContext(t)
	app := authenticatedEventClient(t, s, appToken)
	var identity security.Principal
	must(t, app.Call(ctx, "test/identity", nil, &identity))
	if !identity.IsApp() || identity.AppID != "test.app" || identity.ProcessID != 42 {
		t.Fatalf("scoped principal lost during multiplexing: %+v", identity)
	}
	if _, err := app.Subscribe(ctx, SubscribeParams{Topic: "*"}); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("app subscription = %v, want permission denied", err)
	}
	for _, method := range []string{HistoryMethod, UnsubscribeMethod} {
		if err := app.Call(ctx, method, HistoryParams{Topic: "*"}, nil); err == nil ||
			!strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("app %s = %v, want permission denied", method, err)
		}
	}
	operator := authenticatedEventClient(t, s, "operator-token")
	must(t, operator.Call(ctx, "test/identity", nil, &identity))
	if !identity.IsOperator() {
		t.Fatalf("operator principal lost: %+v", identity)
	}
	sub, err := operator.Subscribe(ctx, SubscribeParams{Topic: "*"})
	must(t, err)
	bus.Publish("tester", auditSecret{Token: appToken})
	if n := nextEvent(t, ctx, sub); n.Event.Type != "test.secret" {
		t.Fatalf("operator event = %+v", n)
	}
	must(t, sub.Close(ctx))
}

func TestRevocationRejectsPendingMultiplexedResponse(t *testing.T) {
	s, _, r, tokens, appToken := authenticatedEventServer(t)
	ctx := testContext(t)
	client := authenticatedEventClient(t, s, appToken)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	must(t, r.Handle("test/block", func(ctx context.Context, req Request) (any, error) {
		close(entered)
		select {
		case <-release:
			return "must not deliver after revocation", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	result := make(chan error, 1)
	go func() { result <- client.Call(ctx, "test/block", nil, nil) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("call did not start")
	}
	// Other calls remain responsive before the pending handler returns.
	must(t, client.Call(ctx, "test/identity", nil, nil))
	tokens.RevokeProcess(42)
	close(release)
	// The credential was valid before dispatch, but the completed handler's
	// result must not be delivered after revocation.
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "credential revoked") {
			t.Fatalf("pending call after revocation = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("pending call not released on revocation")
	}
	if err := client.Call(ctx, "test/identity", nil, nil); err == nil {
		t.Fatal("call on revoked connection succeeded")
	}
}

func TestRevokedEventCredentialDisconnectsAndCleansUp(t *testing.T) {
	s, bus, _, tokens, _ := authenticatedEventServer(t)
	ctx := testContext(t)
	operator := authenticatedEventClient(t, s, "operator-token")
	sub, err := operator.Subscribe(ctx, SubscribeParams{Topic: "*"})
	must(t, err)
	tokens.Revoke("operator-token")
	bus.Publish("tester", auditSecret{})
	select {
	case _, ok := <-sub.Events:
		if ok {
			t.Fatal("event delivered with revoked credential")
		}
	case <-ctx.Done():
		t.Fatal("revoked event connection not closed")
	}
	waitSubscriberCount(t, bus, 0)
}
