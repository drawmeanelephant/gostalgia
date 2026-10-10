package services

// Regression for #139: config/preview only required config.read while
// docs say config.write, and the preview layer was a single global map —
// a read-only app could stage values that a config.write app or operator
// then committed (cross-app confused deputy). Staged previews are now
// owned per caller: only the staging caller can commit or cancel them.

import (
	"context"
	"encoding/json"
	"testing"

	"gostalgia/internal/config"
	"gostalgia/internal/security"
)

func TestAuditConfigPreviewCrossAppCommit(t *testing.T) {
	env := newTestConfigService(t)

	reader := security.AppPrincipal("com.test.reader", 301, "sess-r", security.User{ID: "u-1", Name: "appuser"})
	writer := security.AppPrincipal("com.test.writer", 302, "sess-w", security.User{ID: "u-1", Name: "appuser"})
	other := security.AppPrincipal("com.test.other", 303, "sess-o", security.User{ID: "u-1", Name: "appuser"})
	readerCaps := security.NewCapabilities(security.CapIPC, security.CapConfigRead)
	writerCaps := security.NewCapabilities(security.CapIPC, security.CapConfigRead, security.CapConfigWrite)

	effectiveTheme := func() getResp {
		t.Helper()
		resp := env.call(context.Background(), security.AdminCapabilities(), "config/get",
			map[string]any{"path": "theme"})
		if !resp.OK {
			t.Fatalf("config/get theme: %s", resp.Error)
		}
		var out getResp
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Part 1: a config.read-only app cannot stage preview overrides, and
	// cannot drive commit or cancel either — docs require config.write.
	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"config/preview", map[string]any{"path": "theme", "value": "midnight"}},
		{"config/commit_preview", map[string]any{"layer": "user"}},
		{"config/cancel_preview", nil},
	} {
		if resp := env.callAs(context.Background(), reader, readerCaps, tc.method, tc.params); resp.OK {
			t.Fatalf("%s accepted a config.read-only app", tc.method)
		}
	}

	// A writer committing now must persist nothing the reader "staged".
	resp := env.callAs(context.Background(), writer, writerCaps, "config/commit_preview",
		map[string]any{"layer": "user"})
	if !resp.OK {
		t.Fatalf("config/commit_preview failed: %s", resp.Error)
	}
	if got := effectiveTheme(); got.Value != "nostalgia" || got.Layer != config.LayerDefault {
		t.Fatalf("reader's value leaked into user layer: %+v", got)
	}

	// Part 2: isolation among write-capable callers. Writer stages a theme;
	// another app's commit and cancel must not touch it.
	resp = env.callAs(context.Background(), writer, writerCaps, "config/preview",
		map[string]any{"path": "theme", "value": "midnight"})
	if !resp.OK {
		t.Fatalf("writer config/preview failed: %s", resp.Error)
	}
	if got := effectiveTheme(); got.Value != "midnight" || got.Layer != config.LayerPreview {
		t.Fatalf("writer's preview not effective: %+v", got)
	}

	// Another write-capable app commits — it has nothing staged, so the
	// writer's staged value must stay in-memory only.
	resp = env.callAs(context.Background(), other, writerCaps, "config/commit_preview",
		map[string]any{"layer": "user"})
	if !resp.OK {
		t.Fatalf("other config/commit_preview failed: %s", resp.Error)
	}
	if got := effectiveTheme(); got.Value != "midnight" || got.Layer != config.LayerPreview {
		t.Fatalf("other app's commit persisted the writer's staged value: %+v", got)
	}

	// Another write-capable app cancels — writer's staged value survives.
	resp = env.callAs(context.Background(), other, writerCaps, "config/cancel_preview", nil)
	if !resp.OK {
		t.Fatalf("other config/cancel_preview failed: %s", resp.Error)
	}
	if got := effectiveTheme(); got.Value != "midnight" || got.Layer != config.LayerPreview {
		t.Fatalf("other app's cancel discarded the writer's staged value: %+v", got)
	}

	// The writer commits its own staged value — that lands on the user layer.
	resp = env.callAs(context.Background(), writer, writerCaps, "config/commit_preview",
		map[string]any{"layer": "user"})
	if !resp.OK {
		t.Fatalf("writer config/commit_preview failed: %s", resp.Error)
	}
	if got := effectiveTheme(); got.Value != "midnight" || got.Layer != config.LayerUser {
		t.Fatalf("writer's own commit did not persist: %+v", got)
	}

	// Operator lifecycle still works end-to-end.
	resp = env.call(context.Background(), security.AdminCapabilities(), "config/preview",
		map[string]any{"path": "theme", "value": "monochrome"})
	if !resp.OK {
		t.Fatalf("operator config/preview failed: %s", resp.Error)
	}
	resp = env.call(context.Background(), security.AdminCapabilities(), "config/cancel_preview", nil)
	if !resp.OK {
		t.Fatalf("operator config/cancel_preview failed: %s", resp.Error)
	}
	if got := effectiveTheme(); got.Value != "midnight" || got.Layer != config.LayerUser {
		t.Fatalf("operator cancel did not revert preview: %+v", got)
	}
}
