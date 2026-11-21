package e2e

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gostalgia/internal/ipc"
	"gostalgia/internal/runtime"
	"gostalgia/internal/security"
)

// baselineMethods enumerates the service routes a credential holding only
// "ipc" may reach. The list must shrink, never grow: admitting a new route
// here is a deliberate design decision, and tightening a baseline route
// behind a real grant is the intended direction.
var baselineMethods = map[string]bool{
	"sys/ping":                 true,
	"sys/status":               true,
	"sys/policy":               true,
	"session/whoami":           true,
	"clipboard/status":         true,
	"sound/status":             true,
	"net/status":               true,
	"hostfs/list":              true,
	"app/invalidate":           true, // app-only: posts an event naming its own id
	"fs/grant/list":            true, // app-scoped: callers only ever see their own grants
	"doc/recents":              true, // filtered by caller identity
	"doc/favorites":            true, // filtered by caller identity
	"doc/search":               true, // app results are scoped to its grants
	"doc/lookup":               true, // app results are scoped to its grants
	"doc/associations":         true,
	"doc/associations/resolve": true,
}

func bootRouteRuntime(t *testing.T) *runtime.Runtime {
	t.Helper()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { rt.Shutdown("test end") })
	return rt
}

// appPublished reports whether method is an application-published route
// (app/<app-id>/<name>) as opposed to a system route in the app/* namespace
// like app/list.
func appPublished(method string) bool {
	return strings.HasPrefix(method, "app/") && strings.Count(method, "/") == 2
}

func TestRouteAuthorizationIsDeclaredAndEnforced(t *testing.T) {
	rt := bootRouteRuntime(t)
	methods := rt.Router.Methods()
	if len(methods) == 0 {
		t.Fatal("router has no routes")
	}

	// A credential decision must exist for every registered route. A
	// handler added through Handle/HandleBatch without a policy lands
	// here — this subtest is the RED test for "forgot the capability
	// decision".
	t.Run("every route declares a policy", func(t *testing.T) {
		for _, m := range methods {
			if _, decided := rt.Router.RoutePolicy(m); !decided {
				t.Errorf("route %q registered without a policy decision", m)
			}
		}
	})

	// The router enforces the declared policy before the handler runs, so
	// every route — including ones whose handlers would fail parameter
	// validation first — rejects a context with no principal and no
	// capabilities.
	t.Run("zero credential denied on every route", func(t *testing.T) {
		for _, m := range methods {
			resp := rt.Router.Dispatch(context.Background(), ipc.Request{ID: 1, Method: m})
			if resp.OK || resp.Code != ipc.CodePermissionDenied {
				t.Errorf("route %q: zero-credential dispatch = ok:%v code:%v err:%q, want permission denied",
					m, resp.OK, resp.Code, resp.Error)
			}
		}
	})

	// A baseline application credential (kind=app, caps={ipc}) must reach
	// exactly the routes in baselineMethods — every other route denies it,
	// whether by policy or by the app-scoped checks inside the handler.
	t.Run("baseline ipc reaches only the allowlist", func(t *testing.T) {
		baseline := ipc.WithPrincipal(
			ipc.WithCapabilities(context.Background(), security.NewCapabilities(security.CapIPC)),
			security.AppPrincipal("com.test.probe", 4242, "", security.User{ID: "probe", Name: "probe"}),
		)
		reachable := map[string]bool{}
		for _, m := range methods {
			if appPublished(m) {
				// Application-published routes are baseline by contract:
				// publishing and invoking both require ipc.
				p, decided := rt.Router.RoutePolicy(m)
				if !decided || !reflect.DeepEqual(p, ipc.Baseline) {
					t.Errorf("app route %q policy = %+v (decided=%v), want baseline ipc", m, p, decided)
				}
				continue
			}
			if resp := rt.Router.Dispatch(baseline, ipc.Request{ID: 2, Method: m}); resp.OK {
				reachable[m] = true
			}
		}
		for m := range reachable {
			if !baselineMethods[m] {
				t.Errorf("route %q is reachable with a baseline ipc credential but is not in baselineMethods — "+
					"give it a capability requirement or admit it deliberately", m)
			}
		}
		for m := range baselineMethods {
			if !reachable[m] {
				t.Errorf("baseline route %q is no longer reachable with an ipc credential — "+
					"remove it from baselineMethods (the list shrinks, never grows)", m)
			}
		}
	})
}

// appCallable reports whether an application principal could satisfy the
// policy: it holds ipc plus every non-admin capability the policy names
// (applications cannot hold admin).
func appCallable(p ipc.Policy) bool {
	caps := []string{security.CapIPC}
	for _, c := range p.Caps {
		if c != security.CapAdmin {
			caps = append(caps, c)
		}
	}
	ctx := ipc.WithPrincipal(
		ipc.WithCapabilities(context.Background(), security.NewCapabilities(caps...)),
		security.AppPrincipal("com.test.probe", 4242, "", security.User{ID: "probe", Name: "probe"}),
	)
	return p.Authorize(ctx) == nil
}

var (
	docMethodToken = regexp.MustCompile("`([a-z]+(?:/[a-z0-9_.-]+)+)`")
	docMethodName  = regexp.MustCompile(`^[a-z]+(?:/[a-z0-9_.-]+)+$`)
	docAnyToken    = regexp.MustCompile("`([^`]+)`")
)

// docsCapabilitySection returns the applications.md section that documents
// the capability/method contract.
func docsCapabilitySection(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/applications.md")
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	const marker = "### Capability and method reference"
	i := strings.Index(string(data), marker)
	if i < 0 {
		t.Fatalf("docs/applications.md lacks %q", marker)
	}
	section := string(data)[i:]
	if j := strings.Index(section[len(marker):], "\n## "); j >= 0 {
		section = section[:len(marker)+j]
	}
	return section
}

// The capability table is the public contract for what a grant buys. The
// policy source is authoritative: every route an application could invoke
// must be documented, and every capability a table row claims for a method
// must match the declared route policy.
func TestDocsCapabilityTableMatchesRoutes(t *testing.T) {
	rt := bootRouteRuntime(t)
	section := docsCapabilitySection(t)

	documented := map[string]bool{}
	for _, tok := range docMethodToken.FindAllStringSubmatch(section, -1) {
		documented[tok[1]] = true
	}

	for _, m := range rt.Router.Methods() {
		if appPublished(m) {
			continue // documented generically as app handlers under `ipc`
		}
		p, decided := rt.Router.RoutePolicy(m)
		if !decided {
			t.Errorf("route %q registered without a policy decision", m)
			continue
		}
		if appCallable(p) && !documented[m] {
			t.Errorf("route %q is callable by an application but missing from the capability table in docs/applications.md", m)
		}
	}

	// Row-level honesty: the capability named in each table row must be
	// one the route policy actually requires. hostfs.* rows are exempt —
	// they describe conditional shared-mount grants, not the route gate.
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "| `") {
			continue
		}
		toks := docAnyToken.FindAllStringSubmatch(line, -1)
		if len(toks) < 2 {
			continue
		}
		capTok := toks[0][1]
		if capTok == "hostfs.read" || capTok == "hostfs.write" {
			continue
		}
		for _, tok := range toks[1:] {
			m := tok[1]
			if !docMethodName.MatchString(m) {
				continue // not a method name (params like {"path":...})
			}
			p, decided := rt.Router.RoutePolicy(m)
			if !decided {
				t.Errorf("docs: capability %q names unregistered route %q", capTok, m)
				continue
			}
			if !slices.Contains(p.Caps, capTok) {
				t.Errorf("docs: %q claims %q for %q, but its route policy requires %v",
					line, capTok, m, p.Caps)
			}
		}
	}
}
