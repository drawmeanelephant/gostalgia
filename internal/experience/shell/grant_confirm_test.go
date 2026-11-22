package shell

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/experience/windows"
	"gostalgia/internal/runtime"
	"gostalgia/sdk"
)

func notesFixtureView(errText string, grantReq *sdk.PathGrantRequest) sdk.View {
	return sdk.View{
		Version:  sdk.PresentationVersion,
		Instance: "notes-launch",
		Title:    "Notes - test-note.txt*",
		State:    sdk.ViewReady,
		Items: []sdk.Item{
			{ID: "doc_info", Label: "Document", Detail: "/users/guest/documents/test-note.txt [modified]"},
		},
		Fields: []sdk.Field{
			{ID: "path_1", Label: "Path", Value: "/users/guest/documents/test-note.txt"},
			{ID: "content_1", Label: "Content", Value: "hello", Multiline: true},
		},
		Actions: []sdk.Action{
			{ID: "save", Label: "Save"},
			{ID: "save_as", Label: "Save As"},
		},
		GrantRequest: grantReq,
		Status:       "Modified (unsaved changes)",
		Error:        errText,
	}
}

func notesFixtureApp() appStatus {
	var a appStatus
	a.Manifest.ID = "com.gostalgia.notes"
	a.Manifest.Name = "Notes"
	a.Running = true
	a.PID = 19
	return a
}

func runAllCmds(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			runAllCmds(t, m, c)
		}
	case tea.QuitMsg:
	case viewTickMsg, tickMsg:
	case viewSubscribedMsg:
		if msg.sub != nil {
			_ = msg.sub.Close(context.Background())
		}
	default:
		_, next := m.Update(msg)
		runAllCmds(t, m, next)
	}
}

func windowWords(m *Model) string {
	var words []string
	for _, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		trimmed := strings.TrimSpace(strings.ReplaceAll(line, "┃", " "))
		if trimmed != "" {
			words = append(words, strings.Fields(trimmed)...)
		}
	}
	return strings.Join(words, " ")
}

func TestGrantConfirmFlowApproveDeclineAndLoopGuard(t *testing.T) {
	const targetPath = "/users/guest/documents/test-note.txt"

	var mu sync.Mutex
	var grantCalls []map[string]any
	var actionCalls []sdk.ActionRequest
	granted := false
	loopGrant := false

	client := callerFunc(func(ctx context.Context, method string, params, out any) error {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "fs/stat":
			return &sdk.Error{Code: sdk.CodeNotFound, Message: "not found"}
		case "fs/grant":
			raw, _ := json.Marshal(params)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			grantCalls = append(grantCalls, m)
			granted = true
			return nil
		case "app/com.gostalgia.notes/action":
			req := params.(sdk.ActionRequest)
			actionCalls = append(actionCalls, req)
			if !granted {
				v := notesFixtureView(
					"Permission denied: Notes has no write grant for /users/guest/documents/test-note.txt. Select Save to allow access to this file, or save under /apps/data/com.gostalgia.notes/.",
					&sdk.PathGrantRequest{
						Path:   targetPath,
						Access: "read-write",
						Action: "save",
						Reason: "Save document",
					},
				)
				return writeJSON(out, v)
			}
			v := notesFixtureView("", nil)
			v.Title = "Notes - test-note.txt"
			v.Status = "Saved " + targetPath + " (11 bytes)"
			if loopGrant {
				v.GrantRequest = &sdk.PathGrantRequest{
					Path:   "/users/guest/documents/other.txt",
					Access: "read-write",
					Action: "save",
				}
			}
			return writeJSON(out, v)
		}
		return writeJSON(out, notesFixtureView("", nil))
	})

	m := NewWithTheme(context.Background(), client, nil, theme.Nostalgia(), ui.Plain)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
	a := notesFixtureApp()
	m.apps = []appStatus{a}
	m.showWindow(&appView{
		id:       a.Manifest.ID,
		pid:      a.PID,
		epoch:    1,
		version:  sdk.PresentationVersion,
		instance: "notes-launch",
		data:     notesFixtureView("", nil),
		values: map[string]string{
			"path_1":    targetPath,
			"content_1": "hello notes",
		},
		focus: 2, // Save action
	})

	// 1. Trigger Save -> dialog opens with Cancel focused; no fs/grant sent yet.
	_, cmd := m.Update(enterKey)
	runAllCmds(t, m, cmd)
	settleFiniteMotion(m)

	if m.Focus() != FocusDialog || m.confirm == nil || m.confirm.kind != confirmGrantPath {
		t.Fatalf("expected grant confirmation dialog, focus=%v confirm=%+v", m.Focus(), m.confirm)
	}
	if m.confirm.active != confirmCancel {
		t.Fatalf("expected Cancel focused by default, active=%d", m.confirm.active)
	}
	words := dialogWords(m)
	for _, want := range []string{"Allow Notes file access?", "com.gostalgia.notes", "read-write", targetPath, "this session"} {
		if !strings.Contains(words, want) {
			t.Fatalf("grant dialog missing %q:\n%s", want, dialogText(m))
		}
	}
	if len(grantCalls) != 0 {
		t.Fatalf("opening dialog issued grants: %+v", grantCalls)
	}

	// 2. Pressing Enter immediately selects Cancel and issues no grant.
	act(t, m, enterKey)
	if m.confirm != nil {
		t.Fatal("Enter on Cancel left dialog open")
	}
	if len(grantCalls) != 0 {
		t.Fatalf("Cancel issued grants: %+v", grantCalls)
	}
	screen := ansi.Strip(m.View().Content)
	if !strings.Contains(screen, "no write grant") || !strings.Contains(screen, "/apps/data/com.gostalgia.notes/") {
		t.Fatalf("declined screen missing clear cause or next step:\n%s", screen)
	}

	// 3. Trigger Save again, Esc declines without issuing a grant.
	_, cmd = m.Update(enterKey)
	runAllCmds(t, m, cmd)
	settleFiniteMotion(m)
	if m.confirm == nil {
		t.Fatal("second Save did not reopen grant dialog")
	}
	act(t, m, escKey)
	if m.confirm != nil || len(grantCalls) != 0 {
		t.Fatalf("Esc left dialog open or issued grant: %+v", grantCalls)
	}

	// 4. Trigger Save again, Tab to Allow, held repeat Enter does nothing, single Enter approves and retries!
	loopGrant = true
	_, cmd = m.Update(enterKey)
	runAllCmds(t, m, cmd)
	settleFiniteMotion(m)
	act(t, m, tabKey)
	if m.confirm.active != confirmProceed {
		t.Fatalf("Tab did not focus Allow: %d", m.confirm.active)
	}
	act(t, m, repeatEnter)
	if m.confirm == nil || len(grantCalls) != 0 {
		t.Fatal("repeat Enter confirmed grant dialog")
	}
	_, cmd = m.Update(enterKey)
	runAllCmds(t, m, cmd)
	settleFiniteMotion(m)

	if len(grantCalls) != 1 {
		t.Fatalf("expected exactly 1 fs/grant call, got %d: %+v", len(grantCalls), grantCalls)
	}
	g := grantCalls[0]
	if g["app_id"] != "com.gostalgia.notes" || g["path"] != targetPath || g["access"] != "read-write" || g["recursive"] != false || g["session_bound"] != true {
		t.Fatalf("grant call not strictly minimal/session-bound: %+v", g)
	}
	if m.confirm != nil {
		t.Fatal("retried action unexpectedly chained a second grant dialog")
	}
	if v := m.focusedView(); v == nil || v.data.Title != "Notes - test-note.txt" || v.data.Error != "" {
		t.Fatalf("view did not update after retried save: %+v", v)
	}
	if got := actionCalls[len(actionCalls)-1].Values["content_1"]; got != "hello notes" {
		t.Fatalf("retried action lost field values: %+v", actionCalls[len(actionCalls)-1].Values)
	}
}

func TestGrantConfirmRejectsUnsolicitedUngrantableDirectoryAndStaleWindow(t *testing.T) {
	t.Run("unsolicited grant request on view load or poll is ignored", func(t *testing.T) {
		m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), ui.Plain)
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
		a := notesFixtureApp()
		m.apps = []appStatus{a}
		m.openWindow(a)
		v := m.focusedView()

		req := &sdk.PathGrantRequest{Path: "/users/guest/documents/a.txt", Access: "read-write", Action: "save"}
		viewData := notesFixtureView("", req)

		// Initial view load (action == false).
		m.Update(viewMsg{id: windows.ID(a.Manifest.ID), epoch: v.epoch, version: 3, data: viewData, action: false})
		if m.confirm != nil {
			t.Fatal("initial view load opened grant dialog")
		}
		// Background poll (viewCheckMsg).
		m.Update(viewCheckMsg{id: windows.ID(a.Manifest.ID), epoch: v.epoch, seq: v.seq, apps: []appStatus{a}, data: &viewData, version: 3})
		if m.confirm != nil {
			t.Fatal("background poll opened grant dialog")
		}
		// Push refresh (refresh == true).
		m.Update(viewMsg{id: windows.ID(a.Manifest.ID), epoch: v.epoch, seq: v.seq, version: 3, data: viewData, refresh: true})
		if m.confirm != nil {
			t.Fatal("push refresh opened grant dialog")
		}
	})

	t.Run("ungrantable paths never open dialog", func(t *testing.T) {
		for _, badPath := range []string{
			"/users/guest/.trash/info/1.json",
			"/apps/data/com.gostalgia.echo/secret.txt",
			"/apps/data",
			"/",
		} {
			m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), ui.Plain)
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
			a := notesFixtureApp()
			m.apps = []appStatus{a}
			v := &appView{
				id: a.Manifest.ID, pid: a.PID, epoch: 1, version: 3, instance: "notes-launch",
				data: notesFixtureView("", nil), values: map[string]string{},
			}
			m.showWindow(v)
			m.openGrantConfirm(v, sdk.ActionRequest{Action: "save"}, sdk.PathGrantRequest{
				Path: badPath, Access: "read-write", Action: "save",
			})
			if m.confirm != nil {
				t.Fatalf("opened grant dialog for ungrantable path %q", badPath)
			}
		}
	})

	t.Run("directory target refuses fs/grant and shows clear banner", func(t *testing.T) {
		grantCalled := false
		client := callerFunc(func(ctx context.Context, method string, params, out any) error {
			switch method {
			case "fs/stat":
				return writeJSON(out, map[string]any{"is_dir": true})
			case "fs/grant":
				grantCalled = true
				return nil
			}
			return nil
		})
		m := NewWithTheme(context.Background(), client, nil, theme.Nostalgia(), ui.Plain)
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
		a := notesFixtureApp()
		m.apps = []appStatus{a}
		v := &appView{
			id: a.Manifest.ID, pid: a.PID, epoch: 1, version: 3, instance: "notes-launch",
			data: notesFixtureView("", nil), values: map[string]string{"path_1": "/users/guest/documents"},
		}
		m.showWindow(v)
		m.openGrantConfirm(v, sdk.ActionRequest{Action: "save", Instance: "notes-launch"}, sdk.PathGrantRequest{
			Path: "/users/guest/documents", Access: "read-write", Action: "save",
		})
		settleFiniteMotion(m)
		act(t, m, tabKey)
		_, cmd := m.Update(enterKey)
		runAllCmds(t, m, cmd)
		if grantCalled {
			t.Fatal("fs/grant was called for an existing directory")
		}
		if !strings.Contains(v.banner, "directory") || !strings.Contains(v.banner, "note.txt") {
			t.Fatalf("expected clear directory error in banner, got %q", v.banner)
		}
	})

	t.Run("closing window blocks confirmation of stale grant dialog", func(t *testing.T) {
		grantCalled := false
		client := callerFunc(func(ctx context.Context, method string, params, out any) error {
			if method == "fs/grant" {
				grantCalled = true
			}
			return nil
		})
		m := NewWithTheme(context.Background(), client, nil, theme.Nostalgia(), ui.Plain)
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
		a := notesFixtureApp()
		m.apps = []appStatus{a}
		v := &appView{
			id: a.Manifest.ID, pid: a.PID, epoch: 1, version: 3, instance: "notes-launch",
			data: notesFixtureView("", nil), values: map[string]string{},
		}
		m.showWindow(v)
		m.openGrantConfirm(v, sdk.ActionRequest{Action: "save", Instance: "notes-launch"}, sdk.PathGrantRequest{
			Path: "/users/guest/documents/test.txt", Access: "read-write", Action: "save",
		})
		settleFiniteMotion(m)
		m.closeWindow(windows.ID(a.Manifest.ID))
		if msg := m.confirmBlocked(*m.confirm); msg == "" {
			t.Fatal("expected confirmBlocked after window close")
		}
		act(t, m, tabKey)
		act(t, m, enterKey)
		if grantCalled {
			t.Fatal("stale grant dialog issued fs/grant after window closed")
		}
	})
}

func TestPresentationLongErrorWrapsWithoutTruncation(t *testing.T) {
	longErr := `Permission denied for /users/guest/.trash/info/1.json: cannot access profile trash. This path cannot be granted; choose a file in /users/guest/documents/ or /apps/data/com.gostalgia.notes/.`

	for _, size := range [][2]int{{100, 32}, {80, 24}} {
		m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), ui.Plain)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		v := &appView{
			id: "com.gostalgia.notes", epoch: 1, version: 3, instance: "notes-launch",
			data:   notesFixtureView(longErr, nil),
			values: map[string]string{"path_1": "/users/guest/.trash/info/1.json", "content_1": "hi"},
		}
		m.showWindow(v)
		rendered := ansi.Strip(m.View().Content)
		if lipgloss.Width(rendered) > size[0] || lipgloss.Height(rendered) > size[1] {
			t.Fatalf("%dx%d escaped terminal bounds (%dx%d)", size[0], size[1], lipgloss.Width(rendered), lipgloss.Height(rendered))
		}
		joined := windowWords(m)
		for _, want := range []string{
			"Permission denied for /users/guest/.trash/info/1.json:",
			"cannot access profile trash.",
			"This path cannot be granted;",
			"/users/guest/documents/",
			"/apps/data/com.gostalgia.notes/.",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("%dx%d screen truncated error (missing %q):\n%s", size[0], size[1], want, rendered)
			}
		}
	}

	// Oversized error stays bounded even on 30x10 and keeps controls reachable.
	m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), ui.Plain)
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	v := &appView{
		id: "com.gostalgia.notes", epoch: 1, version: 3, instance: "notes-launch",
		data:   notesFixtureView(strings.Repeat("denied ", 500), nil),
		values: map[string]string{"path_1": "/a", "content_1": "b"},
		focus:  2,
	}
	m.showWindow(v)
	rendered := ansi.Strip(m.View().Content)
	if lipgloss.Width(rendered) > 30 || lipgloss.Height(rendered) > 10 {
		t.Fatalf("30x10 oversized error escaped bounds: %dx%d", lipgloss.Width(rendered), lipgloss.Height(rendered))
	}
}

// TestNotesScopedSaveInFreshWorkspace reproduces Issue #292 end-to-end against
// a real booted Gostalgia runtime in a fresh empty root at 100x32:
//  1. Declining the grant leaves the file unwritten, document dirty, zero grants,
//     and displays the full cause + next step without truncation.
//  2. Approving the grant writes the document using a single-path, non-recursive,
//     session-bound grant; sibling paths remain denied; stopping Notes revokes the grant.
//  3. Ungrantable paths (.trash, other app private storage) fail safely without
//     opening a grant dialog and show the full cause + next step without truncation.
func TestNotesScopedSaveInFreshWorkspace(t *testing.T) {
	root := t.TempDir()
	rt, client, err := bootShellTestRuntime(root, runtime.Options{LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		rt.Shutdown("notes scoped save test")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Launch Notes in the fresh workspace (no pre-existing grants).
	if err := client.Call(ctx, "app/launch", map[string]string{"id": "com.gostalgia.notes"}, nil); err != nil {
		t.Fatalf("launch notes: %v", err)
	}
	var apps []appStatus
	if err := client.Call(ctx, "app/list", nil, &apps); err != nil {
		t.Fatalf("app/list: %v", err)
	}
	var notesApp appStatus
	for _, a := range apps {
		if a.Manifest.ID == "com.gostalgia.notes" {
			notesApp = a
			break
		}
	}
	if !notesApp.Running {
		t.Fatal("Notes is not running after launch")
	}

	m := NewWithTheme(ctx, client, rt.Done(), theme.Nostalgia(), ui.Plain)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
	m.apps = apps

	// Open Notes window (equivalent to F4).
	openCmd := m.openWindow(notesApp)
	runAllCmds(t, m, openCmd)
	settleFiniteMotion(m)

	v := m.focusedView()
	if v == nil || v.id != "com.gostalgia.notes" || v.data.State != sdk.ViewReady {
		t.Fatalf("Notes window failed to open: %+v", v)
	}

	const docPath = "/users/guest/documents/test-note.txt"
	const docBody = "Hello from fresh workspace!"

	// Enter path in Path field (focus 0), Tab to Content field (focus 1), type content, Tab to Save (focus 2).
	m.Update(typed(docPath))
	m.Update(tabKey)
	m.Update(typed(docBody))
	m.Update(tabKey)

	// Step 1: Select Save -> confirmation dialog opens.
	_, saveCmd := m.Update(enterKey)
	runAllCmds(t, m, saveCmd)
	settleFiniteMotion(m)

	if m.confirm == nil || m.confirm.kind != confirmGrantPath {
		t.Fatalf("expected scoped grant dialog on Save, got confirm=%+v viewErr=%q", m.confirm, v.data.Error)
	}
	if got := dialogWords(m); !strings.Contains(got, docPath) || !strings.Contains(got, "read-write") || !strings.Contains(got, "this session") {
		t.Fatalf("unexpected dialog text:\n%s", dialogText(m))
	}

	// Decline with Esc -> zero grants, file absent, document still dirty, full error visible on 100x32 screen.
	act(t, m, escKey)
	if m.confirm != nil {
		t.Fatal("Esc did not dismiss grant dialog")
	}
	if !strings.HasSuffix(v.data.Title, "*") {
		t.Fatalf("expected document to remain modified after decline, title=%q", v.data.Title)
	}
	var statOut map[string]any
	if err := client.Call(ctx, "fs/stat", map[string]string{"path": docPath}, &statOut); err == nil {
		t.Fatal("file should not exist after declining grant")
	}
	var grantsOut struct {
		Grants []struct {
			ID           string `json:"id"`
			Path         string `json:"path"`
			Access       string `json:"access"`
			Recursive    bool   `json:"recursive"`
			SessionBound bool   `json:"session_bound"`
			Revoked      bool   `json:"revoked"`
		} `json:"grants"`
	}
	if err := client.Call(ctx, "fs/grant/list", map[string]string{"app_id": "com.gostalgia.notes"}, &grantsOut); err != nil {
		t.Fatalf("fs/grant/list: %v", err)
	}
	if len(grantsOut.Grants) != 0 {
		t.Fatalf("expected 0 grants after decline, got %+v", grantsOut.Grants)
	}
	declinedScreen := windowWords(m)
	for _, want := range []string{"Permission denied:", "no write grant for " + docPath, "Select Save to allow", "/apps/data/com.gostalgia.notes/."} {
		if !strings.Contains(declinedScreen, want) {
			t.Fatalf("declined 100x32 screen truncated error (missing %q):\n%s", want, ansi.Strip(m.View().Content))
		}
	}

	// Step 2: Select Save again and approve (Tab -> Enter).
	_, saveCmd = m.Update(enterKey)
	runAllCmds(t, m, saveCmd)
	settleFiniteMotion(m)
	if m.confirm == nil || m.confirm.kind != confirmGrantPath {
		t.Fatal("second Save did not open grant dialog")
	}
	act(t, m, tabKey)
	_, allowCmd := m.Update(enterKey)
	runAllCmds(t, m, allowCmd)
	settleFiniteMotion(m)

	if v.data.Title != "Notes - test-note.txt" {
		t.Fatalf("title after approved save = %q, want %q (error=%q banner=%q)", v.data.Title, "Notes - test-note.txt", v.data.Error, v.banner)
	}
	if v.data.Error != "" || v.banner != "" {
		t.Fatalf("expected no error after approved save, error=%q banner=%q", v.data.Error, v.banner)
	}

	// Verify file contents on VFS.
	var readOut struct {
		DataBase64 string `json:"data_base64"`
	}
	if err := client.Call(ctx, "fs/read", map[string]string{"path": docPath}, &readOut); err != nil {
		t.Fatalf("fs/read saved file: %v", err)
	}
	gotBytes, err := base64.StdEncoding.DecodeString(readOut.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBytes) != docBody {
		t.Fatalf("saved content = %q, want %q", string(gotBytes), docBody)
	}

	// Verify grant scope is strictly minimal: single file, non-recursive, session-bound.
	grantsOut.Grants = nil
	if err := client.Call(ctx, "fs/grant/list", map[string]string{"app_id": "com.gostalgia.notes"}, &grantsOut); err != nil {
		t.Fatalf("fs/grant/list: %v", err)
	}
	if len(grantsOut.Grants) != 1 {
		t.Fatalf("expected exactly 1 grant, got %+v", grantsOut.Grants)
	}
	g := grantsOut.Grants[0]
	if g.Path != docPath || g.Access != "read-write" || g.Recursive || !g.SessionBound || g.Revoked {
		t.Fatalf("grant scope is not minimal/session-bound: %+v", g)
	}

	// Verify Notes STILL cannot save to a sibling path in /users/guest/documents.
	if err := client.Call(ctx, "app/com.gostalgia.notes/save", map[string]any{
		"path":    "/users/guest/documents/sibling-denied.txt",
		"content": "should fail",
	}, nil); err == nil {
		t.Fatal("Notes was unexpectedly allowed to write sibling path in /users/guest/documents")
	}

	// Step 3: Ungrantable paths fail safely without opening a grant dialog and without truncating the error.
	for _, tc := range []struct {
		path    string
		wantSub string
	}{
		{"/users/guest/.trash/evil.txt", "cannot access profile trash"},
		{"/apps/data/com.gostalgia.echo/secret.txt", "cross-app access"},
	} {
		// Replace path field value and trigger Save As.
		v.values[v.data.Fields[0].ID] = tc.path
		v.focus = len(v.data.Fields) + 1 // Save As action
		_, cmd := m.Update(enterKey)
		runAllCmds(t, m, cmd)
		settleFiniteMotion(m)

		if m.confirm != nil {
			t.Fatalf("grant dialog unexpectedly opened for ungrantable path %q", tc.path)
		}
		screen := windowWords(m)
		if !strings.Contains(strings.ToLower(screen), tc.wantSub) || !strings.Contains(screen, "This path cannot be granted;") {
			t.Fatalf("ungrantable path %q screen missing cause or next step:\n%s", tc.path, ansi.Strip(m.View().Content))
		}
	}

	// Step 4: Stopping Notes automatically revokes the session-bound grant.
	if err := client.Call(ctx, "app/stop", map[string]string{"id": "com.gostalgia.notes"}, nil); err != nil {
		t.Fatalf("stop notes: %v", err)
	}
	grantsOut.Grants = nil
	if err := client.Call(ctx, "fs/grant/list", map[string]string{"app_id": "com.gostalgia.notes"}, &grantsOut); err != nil {
		t.Fatalf("fs/grant/list after stop: %v", err)
	}
	for _, gr := range grantsOut.Grants {
		if !gr.Revoked {
			t.Fatalf("session-bound grant survived app stop: %+v", gr)
		}
	}
}
