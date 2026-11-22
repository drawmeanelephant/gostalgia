package notes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gostalgia/sdk"
)

type mockVFS struct {
	mu       sync.Mutex
	files    map[string][]byte
	mods     map[string]time.Time
	saveFail error
}

func newMockVFS() *mockVFS {
	return &mockVFS{
		files: make(map[string][]byte),
		mods:  make(map[string]time.Time),
	}
}

func (v *mockVFS) call(ctx context.Context, method string, params, out any) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}

	switch method {
	case "fs/stat":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(rawParams, &p); err != nil {
			return err
		}
		data, ok := v.files[p.Path]
		if !ok {
			return errors.New("file not found")
		}
		modTime := v.mods[p.Path].Format(time.RFC3339Nano)
		res := map[string]any{
			"name":     path.Base(p.Path),
			"mod_time": modTime,
			"is_dir":   false,
			"size":     int64(len(data)),
		}
		raw, _ := json.Marshal(res)
		return json.Unmarshal(raw, out)

	case "fs/read":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(rawParams, &p); err != nil {
			return err
		}
		data, ok := v.files[p.Path]
		if !ok {
			return errors.New("file not found")
		}
		res := map[string]any{
			"data_base64": base64.StdEncoding.EncodeToString(data),
		}
		raw, _ := json.Marshal(res)
		return json.Unmarshal(raw, out)

	case "fs/save":
		if v.saveFail != nil {
			return v.saveFail
		}
		var p struct {
			Path       string `json:"path"`
			DataBase64 string `json:"data_base64"`
			Overwrite  bool   `json:"overwrite"`
		}
		if err := json.Unmarshal(rawParams, &p); err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(p.DataBase64)
		if err != nil {
			return err
		}
		v.files[p.Path] = decoded
		v.mods[p.Path] = time.Now()
		return nil

	case "fs/remove":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(rawParams, &p); err != nil {
			return err
		}
		delete(v.files, p.Path)
		delete(v.mods, p.Path)
		return nil

	default:
		return fmt.Errorf("mockVFS: unsupported method %s", method)
	}
}

type notesTestHarness struct {
	notes    *Notes
	vfs      *mockVFS
	ctx      *sdk.Context
	handlers map[string]sdk.Handler
	reqSeq   uint64
}

func (h *notesTestHarness) view(ctx context.Context) (sdk.View, error) {
	return h.viewReq(ctx, sdk.ViewRequest{Version: 1})
}

func (h *notesTestHarness) viewReq(ctx context.Context, req sdk.ViewRequest) (sdk.View, error) {
	vHandler, ok := h.handlers["view"]
	if !ok {
		return sdk.View{}, errors.New("view handler not registered")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return sdk.View{}, err
	}
	res, err := vHandler(ctx, raw)
	if err != nil {
		return sdk.View{}, err
	}
	return res.(sdk.View), nil
}

func (h *notesTestHarness) act(ctx context.Context, action string, values map[string]string) (sdk.View, error) {
	currView, err := h.view(ctx)
	if err != nil {
		return sdk.View{}, err
	}
	aHandler, ok := h.handlers["action"]
	if !ok {
		return sdk.View{}, errors.New("action handler not registered")
	}
	seq := atomic.AddUint64(&h.reqSeq, 1)
	req := sdk.ActionRequest{
		Version:   sdk.PresentationVersion,
		Instance:  currView.Instance,
		RequestID: fmt.Sprintf("req_%d", seq),
		Action:    action,
		Values:    values,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return sdk.View{}, err
	}
	res, err := aHandler(ctx, raw)
	if err != nil {
		return sdk.View{}, err
	}
	return res.(sdk.View), nil
}

func setupNotesTest(t *testing.T) *notesTestHarness {
	t.Helper()
	vfs := newMockVFS()
	handlers := make(map[string]sdk.Handler)

	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	n := inst.(*Notes)

	call := func(ctx context.Context, method string, params, out any) error {
		return vfs.call(ctx, method, params, out)
	}
	handle := func(name string, h sdk.Handler) error {
		handlers[name] = h
		return nil
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := sdk.NewContext(Manifest(), logger, call, handle)

	if err := n.Init(ctx); err != nil {
		t.Fatal(err)
	}

	return &notesTestHarness{
		notes:    n,
		vfs:      vfs,
		ctx:      ctx,
		handlers: handlers,
	}
}

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("Manifest validation failed: %v", err)
	}
	if m.ID != ID {
		t.Errorf("manifest id = %s, want %s", m.ID, ID)
	}
	if m.Entrypoint != "notes" {
		t.Errorf("manifest entrypoint = %s, want notes", m.Entrypoint)
	}
	raw := ManifestJSON()
	if len(raw) == 0 {
		t.Fatal("ManifestJSON returned empty")
	}
}

func TestInitialStateAndNewDoc(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	v, err := h.view(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("View validation failed: %v", err)
	}
	if v.Title != "Notes - [Untitled]" {
		t.Errorf("Title = %q, want %q", v.Title, "Notes - [Untitled]")
	}
	if strings.Contains(v.Title, "*") {
		t.Errorf("Title should not have dirty indicator initially: %s", v.Title)
	}
}

func TestOpenAndSaveFlow(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	docPath := "/users/guest/documents/test.txt"
	h.vfs.files[docPath] = []byte("Hello Gostalgia!\nLine two.")
	h.vfs.mods[docPath] = time.Now().Add(-1 * time.Hour)

	// Open via action
	v, err := h.act(ctx, "open", map[string]string{
		h.notes.pathFieldID(): docPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if h.notes.dirty {
		t.Errorf("expected clean state after open, got dirty")
	}
	if h.notes.content != "Hello Gostalgia!\nLine two." {
		t.Errorf("content = %q, want 'Hello Gostalgia!\nLine two.'", h.notes.content)
	}
	if v.Title != "Notes - test.txt" {
		t.Errorf("title = %q, want 'Notes - test.txt'", v.Title)
	}

	// Edit content and save via action
	editedContent := "Hello Gostalgia!\nLine two.\nLine three: modified."
	v, err = h.act(ctx, "save", map[string]string{
		h.notes.contentFieldID(): editedContent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}

	// After save, dirty should be false and disk should have edited content
	if h.notes.dirty {
		t.Errorf("expected clean state after save, got dirty")
	}
	if string(h.vfs.files[docPath]) != editedContent {
		t.Errorf("saved content on disk = %q, want %q", string(h.vfs.files[docPath]), editedContent)
	}
	if !strings.Contains(v.Status, "Saved") {
		t.Errorf("status = %q, want Saved...", v.Status)
	}
}

func TestUnicodeHandling(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	docPath := "/users/guest/documents/日本語ノート.txt"
	unicodeText := "こんにちは世界！ 🚀 🌟\nAccents: café, naïve, résumé.\nMath: ∑(n=1..∞) 1/n² = π²/6."

	h.vfs.files[docPath] = []byte(unicodeText)
	h.vfs.mods[docPath] = time.Now()

	// Open unicode file
	v, err := h.act(ctx, "open", map[string]string{
		h.notes.pathFieldID(): docPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if h.notes.content != unicodeText {
		t.Fatalf("content = %q, want %q", h.notes.content, unicodeText)
	}

	// Add more unicode content
	moreUnicode := unicodeText + "\nNew line with emoji: 🎉 🎈"
	v, err = h.act(ctx, "save", map[string]string{
		h.notes.contentFieldID(): moreUnicode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if string(h.vfs.files[docPath]) != moreUnicode {
		t.Fatalf("saved content = %q, want %q", string(h.vfs.files[docPath]), moreUnicode)
	}
}

func TestDirtyStateTracking(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	docPath := "/users/guest/documents/clean.txt"
	initialText := "Base content"
	h.vfs.files[docPath] = []byte(initialText)
	h.vfs.mods[docPath] = time.Now()

	// Open document
	_, err := h.act(ctx, "open", map[string]string{
		h.notes.pathFieldID(): docPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.notes.dirty {
		t.Fatal("expected dirty = false after open")
	}

	// Edit content via programmatic route
	_, err = h.notes.editRoute(ctx, json.RawMessage(`{"content":"Base content with edit"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !h.notes.dirty {
		t.Fatal("expected dirty = true after edit")
	}

	// Recovery draft should be staged in app-private storage
	draftRaw, ok := h.vfs.files[AppPrivateRecoveryPath]
	if !ok {
		t.Fatal("expected recovery draft to be staged in app-private storage")
	}
	var draft RecoveryDraft
	if err := json.Unmarshal(draftRaw, &draft); err != nil {
		t.Fatalf("decode recovery draft: %v", err)
	}
	if draft.Content != "Base content with edit" {
		t.Errorf("draft content = %q, want %q", draft.Content, "Base content with edit")
	}

	// View should show dirty indicator
	v, err := h.view(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(v.Title, "*") {
		t.Errorf("expected title to end with '*', got %q", v.Title)
	}

	// Edit back to original content: should clear dirty state and remove recovery draft!
	_, err = h.notes.editRoute(ctx, json.RawMessage(`{"content":"Base content"}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.notes.dirty {
		t.Fatal("expected dirty = false after editing back to original content")
	}
	if _, exists := h.vfs.files[AppPrivateRecoveryPath]; exists {
		t.Fatal("expected recovery draft to be removed after reverting to clean content")
	}
}

func TestSaveConflictDetection(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	docPath := "/users/guest/documents/shared.txt"
	h.vfs.files[docPath] = []byte("Initial text")
	h.vfs.mods[docPath] = time.Now().Add(-10 * time.Minute)

	// Open document
	_, err := h.act(ctx, "open", map[string]string{
		h.notes.pathFieldID(): docPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another process modifies the file on disk!
	h.vfs.files[docPath] = []byte("Modified by another process!")
	h.vfs.mods[docPath] = time.Now()

	// Notes attempts to save its local edit
	v, err := h.act(ctx, "save", map[string]string{
		h.notes.contentFieldID(): "My local edits.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}

	// Save conflict should be triggered: prompt is PromptConflict
	if h.notes.prompt != PromptConflict {
		t.Fatalf("expected prompt %s, got %s", PromptConflict, h.notes.prompt)
	}
	if !h.notes.dirty {
		t.Fatal("expected document to remain dirty on save conflict")
	}
	if v.Title != "Notes - Save Conflict*" {
		t.Errorf("title = %q, want 'Notes - Save Conflict*'", v.Title)
	}

	// Disk copy should NOT be overwritten yet
	if string(h.vfs.files[docPath]) != "Modified by another process!" {
		t.Fatalf("disk copy was prematurely overwritten: %s", string(h.vfs.files[docPath]))
	}

	// User decides to force overwrite
	v, err = h.act(ctx, "force_save", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}

	// After force_save: conflict resolved, dirty false, disk has local edits
	if h.notes.prompt != PromptNone {
		t.Errorf("expected prompt %s, got %s", PromptNone, h.notes.prompt)
	}
	if h.notes.dirty {
		t.Fatal("expected dirty = false after force_save")
	}
	if string(h.vfs.files[docPath]) != "My local edits." {
		t.Fatalf("disk content = %q, want 'My local edits.'", string(h.vfs.files[docPath]))
	}
}

func TestUnsavedChangesPromptOnOpenAndNew(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	file1 := "/users/guest/documents/file1.txt"
	file2 := "/users/guest/documents/file2.txt"
	h.vfs.files[file1] = []byte("File 1 content")
	h.vfs.files[file2] = []byte("File 2 content")
	h.vfs.mods[file1] = time.Now()
	h.vfs.mods[file2] = time.Now()

	// Open file1
	_, err := h.act(ctx, "open", map[string]string{
		h.notes.pathFieldID(): file1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Edit file1 without saving
	_, err = h.act(ctx, "open", map[string]string{
		h.notes.contentFieldID(): "Unsaved edits in file1",
		h.notes.pathFieldID():    file2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should enter PromptUnsaved
	if h.notes.prompt != PromptUnsaved {
		t.Fatalf("expected prompt %s, got %s", PromptUnsaved, h.notes.prompt)
	}
	if h.notes.pendingAction != "open" || h.notes.pendingPath != file2 {
		t.Fatalf("pending: action=%s path=%s", h.notes.pendingAction, h.notes.pendingPath)
	}

	// Test Cancel
	v, err := h.act(ctx, "cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if h.notes.prompt != PromptNone {
		t.Fatalf("expected prompt None after cancel, got %s", h.notes.prompt)
	}
	if h.notes.path != file1 {
		t.Fatalf("expected path to remain file1 after cancel, got %s", h.notes.path)
	}
	if !h.notes.dirty {
		t.Fatal("expected dirty to remain true after cancel")
	}

	// Try to open file2 again
	_, err = h.act(ctx, "open", map[string]string{
		h.notes.pathFieldID(): file2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.notes.prompt != PromptUnsaved {
		t.Fatalf("expected prompt %s", PromptUnsaved)
	}

	// Choose discard_and_proceed
	v, err = h.act(ctx, "discard_and_proceed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if h.notes.path != file2 {
		t.Fatalf("expected path to be file2, got %s", h.notes.path)
	}
	if h.notes.content != "File 2 content" {
		t.Fatalf("expected content of file2, got %q", h.notes.content)
	}
	if h.notes.dirty {
		t.Fatal("expected clean state after discard_and_proceed")
	}
}

func TestCrashRecoveryRestorationAndDiscard(t *testing.T) {
	// Case 1: Restore recovery draft
	{
		h := setupNotesTest(t)
		ctx := context.Background()

		// Simulate pre-existing recovery draft from an interrupted / crashed session
		draft := RecoveryDraft{
			Path:        "/users/guest/documents/crashed.txt",
			Content:     "Important unsaved work during crash!",
			BaseModTime: time.Now().Add(-1 * time.Hour).Format(time.RFC3339Nano),
			StagedAt:    time.Now().Add(-5 * time.Minute),
			Dirty:       true,
		}
		draftRaw, _ := json.Marshal(draft)
		h.vfs.files[AppPrivateRecoveryPath] = draftRaw
		h.vfs.mods[AppPrivateRecoveryPath] = time.Now()

		v, err := h.view(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
		if h.notes.prompt != PromptRecovery {
			t.Fatalf("expected prompt %s on launch with recovery draft, got %s", PromptRecovery, h.notes.prompt)
		}

		// Restore draft
		v, err = h.act(ctx, "restore", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
		if h.notes.prompt != PromptNone {
			t.Fatalf("expected prompt None after restore, got %s", h.notes.prompt)
		}
		if h.notes.path != "/users/guest/documents/crashed.txt" {
			t.Fatalf("path = %s, want crashed.txt", h.notes.path)
		}
		if h.notes.content != "Important unsaved work during crash!" {
			t.Fatalf("content = %q", h.notes.content)
		}
		if !h.notes.dirty {
			t.Fatal("expected dirty = true after restoring draft")
		}
	}

	// Case 2: Discard recovery draft
	{
		h := setupNotesTest(t)
		ctx := context.Background()

		draft := RecoveryDraft{
			Path:     "/users/guest/documents/crashed2.txt",
			Content:  "Work to discard",
			StagedAt: time.Now(),
			Dirty:    true,
		}
		draftRaw, _ := json.Marshal(draft)
		h.vfs.files[AppPrivateRecoveryPath] = draftRaw

		v, err := h.view(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if h.notes.prompt != PromptRecovery {
			t.Fatalf("expected PromptRecovery, got %s", h.notes.prompt)
		}

		v, err = h.act(ctx, "discard_recovery", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
		if h.notes.prompt != PromptNone {
			t.Fatalf("expected PromptNone, got %s", h.notes.prompt)
		}
		if _, exists := h.vfs.files[AppPrivateRecoveryPath]; exists {
			t.Fatal("expected recovery draft to be removed after discard")
		}
		if h.notes.dirty {
			t.Fatal("expected clean state after discard")
		}
	}
}

func TestSaveFailurePreservesDirtyState(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	h.notes.path = "/users/guest/documents/fail.txt"
	h.notes.content = "Unsaved content"
	h.notes.dirty = true

	// Simulate VFS save failure (e.g. read-only permission or disk full)
	h.vfs.saveFail = errors.New("vfs: permission denied")

	_, err := h.act(ctx, "save", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Notes should report the error banner, but remain dirty!
	if !h.notes.dirty {
		t.Fatal("dirty state was incorrectly cleared on save failure")
	}
	if h.notes.lastError == "" {
		t.Fatal("expected lastError to contain save failure details")
	}
}

func itemDetail(t *testing.T, v sdk.View, id string) string {
	t.Helper()
	for _, it := range v.Items {
		if it.ID == id {
			return it.Detail
		}
	}
	t.Fatalf("item %q not found in view %+v", id, v.Items)
	return ""
}

func TestLiveDraftPreviewReflectsUnsavedStatusAndSize(t *testing.T) {
	h := setupNotesTest(t)
	ctx := context.Background()

	// Initial v3 view exposes a live multiline Content field and clean state.
	v, err := h.viewReq(ctx, sdk.ViewRequest{Version: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(v.Fields) != 2 || v.Fields[0].Live || !v.Fields[1].Multiline || !v.Fields[1].Live {
		t.Fatalf("unexpected v3 fields: %+v", v.Fields)
	}

	// Typing into the Content draft marks the view modified and updates size
	// without mutating committed Notes state.
	draftText := "Hello\ncafé 🚀"
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): draftText},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - [Untitled]*" {
		t.Fatalf("draft title = %q, want %q", v.Title, "Notes - [Untitled]*")
	}
	if got := itemDetail(t, v, "doc_info"); got != "[Untitled] [modified]" {
		t.Fatalf("draft doc_info = %q, want %q", got, "[Untitled] [modified]")
	}
	if got := itemDetail(t, v, "char_count"); got != "12 runes · 16 bytes" {
		t.Fatalf("draft size = %q, want %q", got, "12 runes · 16 bytes")
	}
	if v.Status != "Modified (unsaved changes)" {
		t.Fatalf("draft status = %q, want %q", v.Status, "Modified (unsaved changes)")
	}
	if h.notes.dirty || h.notes.content != "" {
		t.Fatalf("read-only draft view mutated notes state: dirty=%v content=%q", h.notes.dirty, h.notes.content)
	}

	// Editing back to empty reverts cleanly.
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - [Untitled]" || itemDetail(t, v, "doc_info") != "[Untitled] [saved]" ||
		itemDetail(t, v, "char_count") != "0 runes · 0 bytes" || v.Status != "Saved (up to date)" {
		t.Fatalf("reverted untitled view = title:%q doc:%q size:%q status:%q",
			v.Title, itemDetail(t, v, "doc_info"), itemDetail(t, v, "char_count"), v.Status)
	}

	// Opened and saved files also reflect live edits and clear stale Saved/Opened footers.
	docPath := "/users/guest/documents/live.txt"
	h.vfs.files[docPath] = []byte("Base")
	h.vfs.mods[docPath] = time.Now().Add(-time.Minute)

	_, err = h.act(ctx, "open", map[string]string{h.notes.pathFieldID(): docPath})
	if err != nil {
		t.Fatal(err)
	}

	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): "Base edited"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - live.txt*" || itemDetail(t, v, "doc_info") != docPath+" [modified]" ||
		itemDetail(t, v, "char_count") != "11 runes · 11 bytes" || v.Status != "Modified (unsaved changes)" {
		t.Fatalf("edited opened doc view = title:%q doc:%q size:%q status:%q",
			v.Title, itemDetail(t, v, "doc_info"), itemDetail(t, v, "char_count"), v.Status)
	}

	// Editing back to the saved content restores clean status and size.
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): "Base"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - live.txt" || itemDetail(t, v, "doc_info") != docPath+" [saved]" ||
		itemDetail(t, v, "char_count") != "4 runes · 4 bytes" || !strings.HasPrefix(v.Status, "Opened ") {
		t.Fatalf("reverted opened doc view = title:%q doc:%q size:%q status:%q",
			v.Title, itemDetail(t, v, "doc_info"), itemDetail(t, v, "char_count"), v.Status)
	}

	// After a successful save, further edits replace the "Saved ..." footer with modified status.
	_, err = h.act(ctx, "save", map[string]string{h.notes.contentFieldID(): "Base saved"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): "Base saved!"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - live.txt*" || itemDetail(t, v, "doc_info") != docPath+" [modified]" ||
		itemDetail(t, v, "char_count") != "11 runes · 11 bytes" || v.Status != "Modified (unsaved changes)" {
		t.Fatalf("post-save edited view = title:%q doc:%q size:%q status:%q",
			v.Title, itemDetail(t, v, "doc_info"), itemDetail(t, v, "char_count"), v.Status)
	}

	// Clearing the buffer completely still marks a non-empty file modified with 0 size.
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - live.txt*" || itemDetail(t, v, "char_count") != "0 runes · 0 bytes" ||
		v.Status != "Modified (unsaved changes)" {
		t.Fatalf("cleared buffer view = title:%q size:%q status:%q",
			v.Title, itemDetail(t, v, "char_count"), v.Status)
	}

	// A failed save clears stale "Saved ..." status, and editing the draft back
	// to disk content clears the modified state in the live preview.
	h.vfs.saveFail = errors.New("vfs: permission denied")
	v, err = h.act(ctx, "save", map[string]string{h.notes.contentFieldID(): "Base fail"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "Modified (unsaved changes)" {
		t.Fatalf("failed save status = %q, want %q", v.Status, "Modified (unsaved changes)")
	}
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): "Base saved"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - live.txt" || itemDetail(t, v, "doc_info") != docPath+" [saved]" ||
		itemDetail(t, v, "char_count") != "10 runes · 10 bytes" || v.Status != "Saved (up to date)" {
		t.Fatalf("reverted-after-failed-save view = title:%q doc:%q size:%q status:%q",
			v.Title, itemDetail(t, v, "doc_info"), itemDetail(t, v, "char_count"), v.Status)
	}

	// Stale field IDs from an earlier docRev are cleanly ignored after New.
	oldFieldID := h.notes.contentFieldID()
	h.vfs.saveFail = nil
	_, err = h.act(ctx, "new", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.act(ctx, "discard_and_proceed", nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{oldFieldID: "stale draft"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Notes - [Untitled]" || itemDetail(t, v, "doc_info") != "[Untitled] [saved]" ||
		itemDetail(t, v, "char_count") != "0 runes · 0 bytes" {
		t.Fatalf("stale field ID leaked into new doc view: title:%q doc:%q size:%q",
			v.Title, itemDetail(t, v, "doc_info"), itemDetail(t, v, "char_count"))
	}

	// Oversized read-only documents ignore manual draft overrides.
	h.notes.content = strings.Repeat("x", MaxAreaLength+10)
	h.notes.diskContent = h.notes.content
	h.notes.dirty = false
	v, err = h.viewReq(ctx, sdk.ViewRequest{
		Version: 3,
		Values:  map[string]string{h.notes.contentFieldID(): "short"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSize := fmt.Sprintf("%d runes · %d bytes", MaxAreaLength+10, MaxAreaLength+10)
	if itemDetail(t, v, "char_count") != wantSize || strings.HasSuffix(v.Title, "*") {
		t.Fatalf("read-only oversized doc accepted draft override: title:%q size:%q",
			v.Title, itemDetail(t, v, "char_count"))
	}
}
