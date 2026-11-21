package ipc

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gostalgia/internal/security"
)

// Policy declares the minimum authority needed to invoke a route. Dispatch
// enforces it before the handler runs, so a route's authorization lives at
// registration instead of inside handler code. Handlers keep only the
// checks a static declaration cannot express: per-path access rules,
// per-layer escalation, and caller-identity scoping.
//
// The zero value admits any caller — it is only reachable through Handle
// and HandleBatch, which register routes without a policy decision. Routes
// registered by services must declare one: the route-authorization test
// fails for any route whose policy was never decided.
type Policy struct {
	// Caps admits a caller holding any one of the listed capabilities.
	Caps []string
	// Kind, when set, requires the caller's principal kind to match.
	Kind security.PrincipalKind
	// Operator admits operator principals without a capability grant.
	Operator bool
	// App admits application principals without a capability grant; the
	// handler applies the per-caller scoping a capability cannot express
	// (app-private storage, path grants, caller-filtered views).
	App bool
}

// Baseline is the floor every credential already carries. Routes with no
// method-specific grant declare it explicitly so "no capability decision"
// and "intentionally open to any authenticated caller" are distinguishable.
var Baseline = Policy{Caps: []string{security.CapIPC}}

// Cap requires any one of the listed capabilities.
func Cap(capabilities ...string) Policy { return Policy{Caps: capabilities} }

// OperatorOr admits a caller holding any listed capability, or an operator
// principal regardless of grants.
func OperatorOr(capabilities ...string) Policy {
	return Policy{Caps: capabilities, Operator: true}
}

// AppScoped admits a caller holding any listed capability, and application
// principals outright: the handler applies the per-path or per-caller
// scope a blanket capability would over-deny.
func AppScoped(capabilities ...string) Policy {
	return Policy{Caps: capabilities, App: true}
}

// AppOnly restricts a route to application principals holding any one of
// the listed capabilities.
func AppOnly(capabilities ...string) Policy {
	return Policy{Caps: capabilities, Kind: security.PrincipalKindApp}
}

// Authorize reports whether the call context carries the authority the
// policy demands. Denials carry CodePermissionDenied like RequireCap, so
// peers cannot tell router enforcement from a handler check.
func (p Policy) Authorize(ctx context.Context) error {
	principal := CallerPrincipal(ctx)
	if p.Kind != "" && principal.Kind != p.Kind {
		return &Error{
			Code:    CodePermissionDenied,
			Message: fmt.Sprintf("permission denied: %s principal required", p.Kind),
		}
	}
	if len(p.Caps) == 0 && !p.Operator && !p.App {
		return nil
	}
	if caps := Capabilities(ctx); caps != nil {
		for _, c := range p.Caps {
			if caps.Has(c) {
				return nil
			}
		}
	}
	if p.Operator && principal.IsOperator() {
		return nil
	}
	if p.App && principal.IsApp() {
		return nil
	}
	msg := "permission denied"
	if len(p.Caps) > 0 {
		msg = "permission denied: missing capability " + quotedCaps(p.Caps)
	}
	return &Error{Code: CodePermissionDenied, Message: msg}
}

func quotedCaps(caps []string) string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = strconv.Quote(c)
	}
	return strings.Join(out, " or ")
}
