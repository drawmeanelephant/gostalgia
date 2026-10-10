package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gostalgia/apps/markview"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

// #110: Markview is the Markdown open-with companion. Its manifest declares
// .md/.markdown associations, doc/handoff routes documents to it with a
// scoped session-bound read grant, and the rendered structure lands in the
// presentation contract's items. Without a grant the app holds nothing.
func TestMarkviewHandoffGrantFlow(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminCaps := security.AdminCapabilities()
	dfs := env.ctx.VFS.(vfs.DocumentFS)
	v := env.ctx.VFS.(*vfs.VFS)

	doc := "/users/guest/documents/guide.md"
	sib := "/users/guest/documents/sibling.md"
	body := "# Guide\n\nIntro text.\n\n- one\n- two\n\n```go\nfmt.Println(1)\n```\n"
	must(t, dfs.SaveAtomic(doc, []byte(body), 0o644))
	must(t, dfs.SaveAtomic(sib, []byte("# Sibling\n"), 0o644))

	// Associations: markview is an alternative .md handler alongside Notes
	// and the default handler for .markdown.
	resp := env.call(ctx, adminCaps, "doc/associations/resolve", map[string]string{"path": doc})
	if !resp.OK {
		t.Fatalf("resolve failed: %s", resp.Error)
	}
	var resolveOut struct {
		DefaultApp string                        `json:"default_app"`
		Handlers   []sdk.DocumentTypeAssociation `json:"handlers"`
		Found      bool                          `json:"found"`
	}
	must(t, json.Unmarshal(resp.Data, &resolveOut))
	if !resolveOut.Found || resolveOut.DefaultApp != "com.gostalgia.notes" {
		t.Fatalf("notes must keep the .md default, got %+v", resolveOut)
	}
	foundMarkview := false
	for _, h := range resolveOut.Handlers {
		if h.AppID == markview.ID {
			foundMarkview = true
		}
	}
	if !foundMarkview {
		t.Fatalf("markview not registered as .md handler: %+v", resolveOut.Handlers)
	}
	resp = env.call(ctx, adminCaps, "doc/associations/resolve", map[string]string{"path": "doc.markdown"})
	must(t, json.Unmarshal(resp.Data, &resolveOut))
	if !resolveOut.Found || resolveOut.DefaultApp != markview.ID {
		t.Fatalf("markview should own .markdown, got %+v", resolveOut)
	}

	// Hand off the .md explicitly to markview.
	resp = env.call(ctx, adminCaps, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    doc,
		AppID:   markview.ID,
	})
	if !resp.OK {
		t.Fatalf("doc/handoff failed: %s", resp.Error)
	}
	var hResult sdk.HandoffResult
	must(t, json.Unmarshal(resp.Data, &hResult))
	if !hResult.Success || hResult.AppID != markview.ID || hResult.GrantID == "" {
		t.Fatalf("handoff result = %+v", hResult)
	}
	if !env.ctx.Apps.IsRunning(markview.ID) {
		t.Fatalf("markview was not launched by handoff")
	}

	// The grant is scoped to the single document, session-bound, read.
	g, ok := v.Grants().Get(hResult.GrantID)
	if !ok || !g.SessionBound || g.Revoked || g.Path != doc || g.Recursive || g.Access != vfs.AccessRead {
		t.Fatalf("handoff grant = %+v (ok=%v), want session-bound read on %s", g, ok, doc)
	}
	if err := v.Grants().CheckAccess(markview.ID, sib, vfs.AccessRead); err == nil {
		t.Fatalf("SECURITY VIOLATION: markview was granted access to sibling file %s", sib)
	}
	if err := v.Grants().CheckAccess(markview.ID, "/users/guest/documents", vfs.AccessRead); err == nil {
		t.Fatalf("SECURITY VIOLATION: markview was granted parent-directory access")
	}

	// The F4 view carries the rendered Markdown structure: as items on the
	// item-list contracts, inside the version-3 scroll region on the newest.
	resp = env.call(ctx, adminCaps, "app/"+markview.ID+"/view", sdk.ViewRequest{Version: sdk.PresentationVersion})
	if !resp.OK {
		t.Fatalf("markview view failed: %s", resp.Error)
	}
	var view sdk.View
	must(t, json.Unmarshal(resp.Data, &view))
	if err := view.Validate(); err != nil {
		t.Fatalf("view does not validate: %v", err)
	}
	var haveHeading, haveList, haveCode bool
	if view.Region != nil {
		haveHeading = strings.Contains(view.Region.Text, "# Guide")
		haveList = strings.Contains(view.Region.Text, "• one")
		haveCode = strings.Contains(view.Region.Text, "┃ fmt.Println(1)")
	} else {
		for _, it := range view.Items {
			switch it.Label {
			case "# Guide":
				haveHeading = true
			case "• one":
				haveList = true
			case "┃ fmt.Println(1)":
				haveCode = true
			}
		}
	}
	if !haveHeading || !haveList || !haveCode {
		t.Fatalf("view missing rendered structure (heading=%v list=%v code=%v): %+v", haveHeading, haveList, haveCode, view.Items)
	}

	// Without a grant for the sibling, the app's own open route denies.
	resp = env.call(ctx, adminCaps, "app/"+markview.ID+"/open", map[string]string{"path": sib})
	if resp.OK || !strings.Contains(resp.Error, "denied") {
		t.Fatalf("sibling open = ok:%v err:%q, want denial", resp.OK, resp.Error)
	}

	// The session-bound grant dies with the app run.
	must(t, env.ctx.Apps.Stop(markview.ID, 5*time.Second))
	g, ok = v.Grants().Get(hResult.GrantID)
	if !ok || !g.Revoked {
		t.Fatalf("grant after app exit = %+v, want revoked", g)
	}
	if err := v.Grants().CheckAccess(markview.ID, doc, vfs.AccessRead); err == nil {
		t.Fatal("handoff grant still authorizes access after app exit")
	}
}

// A handoff to a binary .md fails honestly: markview rejects the content,
// the dispatch errors, and the scoped grant is revoked rather than leaked.
func TestMarkviewHandoffRejectsBinary(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminCaps := security.AdminCapabilities()
	dfs := env.ctx.VFS.(vfs.DocumentFS)
	v := env.ctx.VFS.(*vfs.VFS)

	bin := "/users/guest/documents/blob.md"
	must(t, dfs.SaveAtomic(bin, []byte{'#', 'x', 0x00, 0x01, 0x02, 0x03}, 0o644))

	resp := env.call(ctx, adminCaps, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    bin,
		AppID:   markview.ID,
	})
	if resp.OK {
		t.Fatalf("binary handoff succeeded, want honest rejection")
	}
	if !strings.Contains(resp.Error, "binary") {
		t.Fatalf("handoff error = %q, want binary rejection", resp.Error)
	}
	if len(v.Grants().List(markview.ID)) != 0 {
		// Any issued grant must already be revoked.
		for _, g := range v.Grants().List(markview.ID) {
			if !g.Revoked {
				t.Fatalf("rejected handoff leaked a live grant: %+v", g)
			}
		}
	}
	if err := v.Grants().CheckAccess(markview.ID, bin, vfs.AccessRead); err == nil {
		t.Fatal("rejected handoff left bin accessible to markview")
	}
}
