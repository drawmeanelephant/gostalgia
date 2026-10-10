package e2e

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"gostalgia/internal/runtime"
	"gostalgia/sdk"
)

// #110: over a real socket, a .markdown handoff resolves to Markview by
// association, the app launches with only a scoped session-bound read
// grant, the document renders as structured items, and the same app is
// denied on any path the grant does not cover.
func TestMarkviewHandoffRendersOverSocket(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root})
	must(t, err)
	t.Cleanup(func() { rt.Shutdown("markview test") })
	client := dialRunning(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	doc := "/users/guest/documents/guide.markdown"
	secret := "/users/guest/documents/secret.md"
	body := "# Guide\n\nIntro *text*.\n\n- one\n- two\n\n```go\nfmt.Println(1)\n```\n\n> a quote\n"
	must(t, client.Call(ctx, "fs/write", map[string]string{
		"path":        doc,
		"data_base64": base64.StdEncoding.EncodeToString([]byte(body)),
	}, nil))
	must(t, client.Call(ctx, "fs/write", map[string]string{
		"path":        secret,
		"data_base64": base64.StdEncoding.EncodeToString([]byte("# Secret\n")),
	}, nil))

	// .markdown resolves to markview as its default handler; on .md,
	// markview is the open-with alternative to Notes' default.
	var resolve struct {
		DefaultApp string `json:"default_app"`
		Handlers   []struct {
			AppID string `json:"app_id"`
		} `json:"handlers"`
	}
	must(t, client.Call(ctx, "doc/associations/resolve", map[string]string{"path": doc}, &resolve))
	if resolve.DefaultApp != "com.gostalgia.markview" {
		t.Fatalf("markdown default = %q, want com.gostalgia.markview", resolve.DefaultApp)
	}
	must(t, client.Call(ctx, "doc/associations/resolve", map[string]string{"path": "/users/guest/documents/x.md"}, &resolve))
	if resolve.DefaultApp != "com.gostalgia.notes" {
		t.Fatalf("md default = %q, want notes kept", resolve.DefaultApp)
	}
	var haveNotes, haveMarkview bool
	for _, h := range resolve.Handlers {
		if h.AppID == "com.gostalgia.notes" {
			haveNotes = true
		}
		if h.AppID == "com.gostalgia.markview" {
			haveMarkview = true
		}
	}
	if !haveNotes || !haveMarkview {
		t.Fatalf(".md should offer notes + markview, got %+v", resolve.Handlers)
	}

	// A bare handoff — no explicit app — routes to markview for .markdown.
	var h sdk.HandoffResult
	must(t, client.Call(ctx, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    doc,
	}, &h))
	if !h.Success || h.AppID != "com.gostalgia.markview" || h.GrantID == "" {
		t.Fatalf("handoff = %+v", h)
	}

	// The running app answers its view over the socket: on contract v3 the
	// rendered document lives in the scroll region, on older contracts in
	// items. Either way the full structure must be present.
	var v sdk.View
	must(t, client.Call(ctx, "app/com.gostalgia.markview/view", sdk.ViewRequest{Version: sdk.PresentationVersion}, &v))
	var haveHeading, haveList, haveCode, haveQuote bool
	mark := func(label string) {
		switch label {
		case "# Guide":
			haveHeading = true
		case "• one":
			haveList = true
		case "┃ fmt.Println(1)":
			haveCode = true
		case "│ a quote":
			haveQuote = true
		}
	}
	if v.Region != nil {
		for _, line := range strings.Split(v.Region.Text, "\n") {
			mark(strings.TrimSpace(line))
		}
	}
	for _, it := range v.Items {
		mark(it.Label)
	}
	if !haveHeading || !haveList || !haveCode || !haveQuote {
		t.Fatalf("rendered structure incomplete: region=%v items=%+v", v.Region != nil, v.Items)
	}

	// The app's own open route is denied on a path outside the grant, and a
	// handoff to binary content fails honestly.
	err = client.Call(ctx, "app/com.gostalgia.markview/open", map[string]string{"path": secret}, nil)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("ungranted open = %v, want denial", err)
	}
	bin := "/users/guest/documents/blob.md"
	must(t, client.Call(ctx, "fs/write", map[string]string{
		"path":        bin,
		"data_base64": base64.StdEncoding.EncodeToString([]byte{'#', 0x00, 0x01}),
	}, nil))
	err = client.Call(ctx, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    bin,
		AppID:   "com.gostalgia.markview",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary handoff = %v, want honest rejection", err)
	}
}
