package ipc

import (
	"context"
	"testing"

	"gostalgia/internal/security"
)

func policyHandler(called *int) Handler {
	return func(ctx context.Context, req Request) (any, error) {
		*called = *called + 1
		return "ok", nil
	}
}

func TestRoutePolicyReportsDecision(t *testing.T) {
	r := NewRouter()
	must(t, r.Handle("open/route", policyHandler(new(int))))
	must(t, r.HandlePolicy("closed/route", policyHandler(new(int)), Cap(security.CapFileRead)))

	if _, decided := r.RoutePolicy("open/route"); decided {
		t.Fatal("Handle must record an undecided policy")
	}
	if p, decided := r.RoutePolicy("closed/route"); !decided || len(p.Caps) != 1 || p.Caps[0] != security.CapFileRead {
		t.Fatalf("RoutePolicy = %+v decided=%v, want fs.read", p, decided)
	}
	if _, decided := r.RoutePolicy("missing/route"); decided {
		t.Fatal("unregistered route must report undecided")
	}
}

// Dispatch must deny before invoking the handler: a caller without the
// declared grant never reaches handler code, so parameter validation can
// never mask a missing authorization check.
func TestPolicyDeniedBeforeHandlerRuns(t *testing.T) {
	r := NewRouter()
	called := 0
	must(t, r.HandlePolicy("secure/thing", policyHandler(&called), Cap(security.CapShutdown)))

	resp := r.Dispatch(context.Background(), Request{ID: 1, Method: "secure/thing"})
	if resp.OK || resp.Code != CodePermissionDenied {
		t.Fatalf("denied dispatch = %+v, want permission denied", resp)
	}
	if called != 0 {
		t.Fatal("handler ran on a denied dispatch")
	}
}

func TestPolicyAdmitsDeclaredCapability(t *testing.T) {
	r := NewRouter()
	called := 0
	must(t, r.HandlePolicy("secure/thing", policyHandler(&called), Cap(security.CapShutdown)))

	ctx := WithCapabilities(context.Background(), security.NewCapabilities(security.CapShutdown))
	resp := r.Dispatch(ctx, Request{ID: 1, Method: "secure/thing"})
	if !resp.OK || called != 1 {
		t.Fatalf("admitted dispatch = %+v called=%d", resp, called)
	}
}

func TestPolicyPrincipalAlternatives(t *testing.T) {
	operator := WithPrincipal(context.Background(),
		security.OperatorPrincipal(security.User{ID: "op"}))
	app := WithPrincipal(
		WithCapabilities(context.Background(), security.NewCapabilities(security.CapIPC)),
		security.AppPrincipal("com.test.app", 7, "", security.User{ID: "guest"}))

	r := NewRouter()
	must(t, r.HandlePolicy("ops/route", policyHandler(new(int)),
		OperatorOr(security.CapSessionRead, security.CapAdmin)))
	must(t, r.HandlePolicy("apps/route", policyHandler(new(int)),
		AppOnly(security.CapIPC)))

	if resp := r.Dispatch(operator, Request{ID: 1, Method: "ops/route"}); !resp.OK {
		t.Fatalf("operator should satisfy OperatorOr policy: %s", resp.Error)
	}
	if resp := r.Dispatch(app, Request{ID: 2, Method: "ops/route"}); resp.OK {
		t.Fatal("baseline app should not satisfy session.read-or-operator policy")
	}
	if resp := r.Dispatch(operator, Request{ID: 3, Method: "apps/route"}); resp.OK {
		t.Fatal("operator should not satisfy app-only policy")
	}
	if resp := r.Dispatch(app, Request{ID: 4, Method: "apps/route"}); !resp.OK {
		t.Fatalf("app with ipc should satisfy app-only ipc policy: %s", resp.Error)
	}
}

func TestHandleBatchPolicyAppliesSharedDecision(t *testing.T) {
	r := NewRouter()
	must(t, r.HandleBatchPolicy(map[string]Handler{
		"batch/one": policyHandler(new(int)),
		"batch/two": policyHandler(new(int)),
	}, Cap(security.CapAdmin)))
	for _, m := range []string{"batch/one", "batch/two"} {
		if p, decided := r.RoutePolicy(m); !decided || len(p.Caps) != 1 || p.Caps[0] != security.CapAdmin {
			t.Fatalf("route %q policy = %+v decided=%v, want admin", m, p, decided)
		}
	}
}
