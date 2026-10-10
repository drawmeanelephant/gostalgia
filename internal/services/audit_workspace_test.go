package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gostalgia/internal/security"
	"gostalgia/internal/session"
)

// Regression for #144: workspace stores persist per user
// (/users/<name>/config/workspace.json), so sessions of the same user must
// share one store. Two sessions holding independent in-memory copies of the
// same file lose each other's interleaved updates (last-writer-wins on the
// whole JSON document).
func TestAuditWorkspaceSetLostUpdatesAcrossSessions(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	admin := security.AdminCapabilities()

	// The default guest session exists; find its id.
	resp := env.call(ctx, admin, "session/list", nil)
	if !resp.OK {
		t.Fatalf("session/list failed: %s", resp.Error)
	}
	var sessions []SessionDetail
	must(t, json.Unmarshal(resp.Data, &sessions))
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want the one default session", len(sessions))
	}
	shellA := sessions[0].ID

	// A second session for the same user, as a second shell would hold.
	resp = env.call(ctx, admin, "session/create", map[string]string{
		"user_id": "u-guest", "user_name": "guest",
	})
	if !resp.OK {
		t.Fatalf("session/create failed: %s", resp.Error)
	}
	var second SessionDetail
	must(t, json.Unmarshal(resp.Data, &second))
	shellB := second.ID
	if shellB == shellA {
		t.Fatalf("second session got id %q, same as first", shellB)
	}

	// Interleave workspace/set calls the way two shells emit them.
	steps := []struct{ sess, cmd string }{
		{shellA, "cmd-from-shell-A-1"},
		{shellB, "cmd-from-shell-B-1"},
		{shellA, "cmd-from-shell-A-2"},
		{shellB, "cmd-from-shell-B-2"},
	}
	for _, step := range steps {
		resp = env.call(ctx, admin, "session/workspace/set", map[string]string{
			"session_id": step.sess, "cmd": step.cmd,
		})
		if !resp.OK {
			t.Fatalf("workspace/set(%s) failed: %s", step.cmd, resp.Error)
		}
	}

	// Both sessions must observe every command — the store is shared.
	resp = env.call(ctx, admin, "session/workspace/get", map[string]string{"session_id": shellA})
	if !resp.OK {
		t.Fatalf("workspace/get failed: %s", resp.Error)
	}
	var state session.WorkspaceState
	must(t, json.Unmarshal(resp.Data, &state))
	for _, step := range steps {
		found := false
		for _, h := range state.History {
			if h == step.cmd {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("history missing %q — sessions of one user lost interleaved updates: %v", step.cmd, state.History)
		}
	}

	// And the persisted per-user document contains them all.
	raw, err := env.ctx.VFS.ReadFile("/users/guest/config/workspace.json")
	must(t, err)
	for _, step := range steps {
		if !strings.Contains(string(raw), step.cmd) {
			t.Fatalf("persisted workspace.json missing %q", step.cmd)
		}
	}
}
