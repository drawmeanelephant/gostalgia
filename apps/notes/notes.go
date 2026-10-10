// Package notes is Gostalgia's standard text editor application.
// It implements basic text editing, open, save, save-as, dirty state tracking,
// unsaved change prompts, external modification conflict detection, and
// bounded crash recovery staging in app-private storage.
package notes

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gostalgia/sdk"
)

const (
	// ID is the reverse-DNS identifier for Notes.
	ID = "com.gostalgia.notes"

	// AppPrivateRecoveryPath is the recovery staging path in app-private storage.
	AppPrivateRecoveryPath = "/apps/data/com.gostalgia.notes/recovery.json"

	// MaxContentLength is the maximum presentation field length supported in version 1.
	MaxContentLength = 4096

	// MaxAreaLength is the presentation version-3 multiline field budget,
	// matching the contract's larger document-editing allowance.
	MaxAreaLength = 32768

	// MaxDocumentSize is the maximum document size allowed for VFS saves (1 MiB).
	MaxDocumentSize = 1048576
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err) // invalid builtin manifest is a programming error
	}
	return m
}

// ManifestJSON returns a copy of the raw embedded JSON.
func ManifestJSON() []byte {
	return append([]byte(nil), manifestJSON...)
}

// PromptKind represents the active modal or confirmation state.
type PromptKind string

const (
	PromptNone     PromptKind = ""
	PromptRecovery PromptKind = "recovery"
	PromptUnsaved  PromptKind = "unsaved"
	PromptConflict PromptKind = "conflict"
)

// RecoveryDraft holds staged unsaved data in app-private storage.
type RecoveryDraft struct {
	Path        string    `json:"path"`
	Content     string    `json:"content"`
	BaseModTime string    `json:"base_mod_time"`
	BaseSize    int64     `json:"base_size"`
	StagedAt    time.Time `json:"staged_at"`
	Dirty       bool      `json:"dirty"`
}

// Notes is the running instance of the editor.
type Notes struct {
	app     *sdk.Context
	started time.Time
	mu      sync.Mutex

	// Document state
	path          string // environment path (e.g. "/users/guest/documents/note.txt") or ""
	content       string // buffer content
	diskContent   string // content on disk at last load/save
	loadedModTime string // disk ModTime at last load/save
	loadedSize    int64  // disk Size at last load/save
	dirty         bool   // whether buffer differs from disk or recovery draft restored
	docRev        uint64 // revision counter; incrementing resets shell input field IDs

	// Crash recovery state
	recoveryChecked bool
	recoveryDraft   *RecoveryDraft
	recoveryStaged  bool

	// Prompt state
	prompt              PromptKind
	pendingAction       string // "open" or "new"
	pendingPath         string // target path for pending open
	conflictDiskModTime string // external disk mod time on conflict

	// Banners and status
	lastError  string
	lastStatus string
}

// Factory constructs a fresh, uninitialized Notes instance.
func Factory() (sdk.Instance, error) {
	return &Notes{
		docRev: 1,
	}, nil
}

// Init registers application routes and presentation callbacks.
func (n *Notes) Init(app *sdk.Context) error {
	n.app = app
	n.started = time.Now()

	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"doc", n.doc},
		{"open", n.openRoute},
		{"save", n.saveRoute},
		{"save_as", n.saveAsRoute},
		{"new", n.newRoute},
		{"edit", n.editRoute},
		{"recover", n.recoverRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}

	return app.Present(n.view, n.act)
}

// Run executes the application background loop and periodical recovery staging.
func (n *Notes) Run(ctx context.Context) error {
	n.app.Log.Info("notes running")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n.mu.Lock()
			if n.dirty && !n.recoveryStaged {
				_ = n.stageRecoveryLocked(ctx)
			}
			n.mu.Unlock()
		}
	}
}

// Stop is called on process termination. If unsaved changes exist,
// we ensure the recovery draft is safely staged before stopping.
func (n *Notes) Stop(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dirty && !n.recoveryStaged {
		_ = n.stageRecoveryLocked(ctx)
	}
	return nil
}

// pathFieldID returns the current dynamic ID for the path input field.
func (n *Notes) pathFieldID() string {
	return fmt.Sprintf("path_%d", n.docRev)
}

// contentFieldID returns the current dynamic ID for the content input field.
func (n *Notes) contentFieldID() string {
	return fmt.Sprintf("content_%d", n.docRev)
}

func (n *Notes) docDisplayName() string {
	if n.path != "" {
		return path.Base(n.path)
	}
	return "[Untitled]"
}

func (n *Notes) docIdentity() string {
	if n.path != "" {
		return n.path
	}
	return "[Untitled]"
}

func truncateDetail(s string) string {
	if len(s) <= 4096 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if b.Len()+utf8.RuneLen(r) > 4093 {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString("...")
	return b.String()
}

func nonBlank(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// ensureRecoveryCheckedLocked inspects app-private storage for an unsaved recovery draft.
func (n *Notes) ensureRecoveryCheckedLocked(ctx context.Context) {
	if n.recoveryChecked {
		return
	}
	n.recoveryChecked = true

	var readOut struct {
		DataBase64 string `json:"data_base64"`
	}
	err := n.app.Call(ctx, "fs/read", map[string]any{
		"path": AppPrivateRecoveryPath,
	}, &readOut)
	if err != nil {
		return
	}

	data, err := base64.StdEncoding.DecodeString(readOut.DataBase64)
	if err != nil {
		return
	}

	var draft RecoveryDraft
	if err := json.Unmarshal(data, &draft); err != nil {
		return
	}

	if draft.Dirty && (draft.Content != "" || draft.Path != "") {
		n.recoveryDraft = &draft
		n.prompt = PromptRecovery
	}
}

// stageRecoveryLocked writes an atomic recovery draft to app-private storage.
func (n *Notes) stageRecoveryLocked(ctx context.Context) error {
	if !n.dirty {
		return nil
	}
	draft := RecoveryDraft{
		Path:        n.path,
		Content:     n.content,
		BaseModTime: n.loadedModTime,
		BaseSize:    n.loadedSize,
		StagedAt:    time.Now().UTC(),
		Dirty:       true,
	}
	data, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	err = n.app.Call(ctx, "fs/save", map[string]any{
		"path":        AppPrivateRecoveryPath,
		"data_base64": encoded,
	}, nil)
	if err == nil {
		n.recoveryStaged = true
	}
	return err
}

// clearRecoveryLocked removes the staged recovery draft from app-private storage.
func (n *Notes) clearRecoveryLocked(ctx context.Context) error {
	n.recoveryDraft = nil
	n.recoveryStaged = false
	return n.app.Call(ctx, "fs/remove", map[string]any{
		"path": AppPrivateRecoveryPath,
	}, nil)
}

// openLocked loads a file from VFS. If dirty and !force, returns an error.
func (n *Notes) openLocked(ctx context.Context, targetPath string, force bool) error {
	if n.dirty && !force {
		return fmt.Errorf("unsaved changes: document is modified; save or discard first")
	}

	var statOut struct {
		Name    string `json:"name"`
		ModTime string `json:"mod_time"`
		IsDir   bool   `json:"is_dir"`
		Size    int64  `json:"size"`
	}
	if err := n.app.Call(ctx, "fs/stat", map[string]string{"path": targetPath}, &statOut); err != nil {
		return fmt.Errorf("open %s: %w", targetPath, err)
	}
	if statOut.IsDir {
		return fmt.Errorf("open %s: path is a directory", targetPath)
	}

	var readOut struct {
		DataBase64 string `json:"data_base64"`
	}
	if err := n.app.Call(ctx, "fs/read", map[string]string{"path": targetPath}, &readOut); err != nil {
		return fmt.Errorf("read %s: %w", targetPath, err)
	}

	data, err := base64.StdEncoding.DecodeString(readOut.DataBase64)
	if err != nil {
		return fmt.Errorf("decode %s: %w", targetPath, err)
	}

	text := string(data)
	n.path = targetPath
	n.content = text
	n.diskContent = text
	n.loadedModTime = statOut.ModTime
	n.loadedSize = statOut.Size
	n.dirty = false
	n.docRev++
	_ = n.clearRecoveryLocked(ctx)
	n.prompt = PromptNone
	n.lastError = ""
	n.lastStatus = fmt.Sprintf("Opened %s (%d bytes)", targetPath, len(data))
	_ = n.app.Call(ctx, "doc/recents/add", map[string]string{"path": targetPath}, nil)
	return nil
}

// saveLocked saves the document using atomic safe saving.
// Checks external modifications on existing files, returning a conflict error if detected.
func (n *Notes) saveLocked(ctx context.Context, targetPath string, content string, force bool, overwrite bool) error {
	if content != "" || n.content == "" {
		n.content = content
	}
	if targetPath == "" {
		targetPath = n.path
	}
	if targetPath == "" {
		n.dirty = true
		return fmt.Errorf("save: path is required (enter path or use Save As)")
	}
	if len(n.content) > MaxDocumentSize {
		return fmt.Errorf("save: document size %d exceeds limit of %d bytes", len(n.content), MaxDocumentSize)
	}

	var statOut struct {
		Name    string `json:"name"`
		ModTime string `json:"mod_time"`
		IsDir   bool   `json:"is_dir"`
		Size    int64  `json:"size"`
	}
	statErr := n.app.Call(ctx, "fs/stat", map[string]string{"path": targetPath}, &statOut)
	if statErr == nil {
		if statOut.IsDir {
			n.dirty = true
			return fmt.Errorf("save %s: destination is a directory", targetPath)
		}
		// If the file exists and was previously loaded, check if modified externally on disk.
		if n.loadedModTime != "" && (statOut.ModTime != n.loadedModTime || (n.loadedSize != 0 && statOut.Size != n.loadedSize)) && !force {
			n.prompt = PromptConflict
			n.conflictDiskModTime = statOut.ModTime
			n.lastError = fmt.Sprintf("save conflict: file %s was modified externally on disk", targetPath)
			n.dirty = true
			return fmt.Errorf("save conflict: file %s was modified externally", targetPath)
		}
	}

	payload := base64.StdEncoding.EncodeToString([]byte(n.content))
	saveErr := n.app.Call(ctx, "fs/save", map[string]any{
		"path":        targetPath,
		"data_base64": payload,
		"overwrite":   overwrite,
	}, nil)
	if saveErr != nil {
		n.dirty = true // dirty state remains untouched on save failure!
		return fmt.Errorf("save %s: %w", targetPath, saveErr)
	}

	// Save succeeded: update disk metadata
	var newStat struct {
		ModTime string `json:"mod_time"`
		Size    int64  `json:"size"`
	}
	if err := n.app.Call(ctx, "fs/stat", map[string]string{"path": targetPath}, &newStat); err == nil {
		n.loadedModTime = newStat.ModTime
		n.loadedSize = newStat.Size
	}

	n.path = targetPath
	n.diskContent = n.content
	n.dirty = false
	n.prompt = PromptNone
	n.lastError = ""
	n.lastStatus = fmt.Sprintf("Saved %s (%d bytes)", targetPath, len(n.content))
	_ = n.clearRecoveryLocked(ctx)
	_ = n.app.Call(ctx, "doc/recents/add", map[string]string{"path": targetPath}, nil)
	return nil
}

// newLocked resets the document to an empty untitled note.
func (n *Notes) newLocked(ctx context.Context, force bool) error {
	if n.dirty && !force {
		return fmt.Errorf("unsaved changes: document is modified; save or discard first")
	}

	n.path = ""
	n.content = ""
	n.diskContent = ""
	n.loadedModTime = ""
	n.loadedSize = 0
	n.dirty = false
	n.docRev++
	_ = n.clearRecoveryLocked(ctx)
	n.prompt = PromptNone
	n.lastError = ""
	n.lastStatus = "New document created."
	return nil
}

// editLocked updates the in-memory content and synchronizes dirty/recovery state.
func (n *Notes) editLocked(ctx context.Context, text string) error {
	n.content = text
	if n.content != n.diskContent {
		n.dirty = true
		_ = n.stageRecoveryLocked(ctx)
	} else {
		n.dirty = false
		_ = n.clearRecoveryLocked(ctx)
	}
	return nil
}

// view constructs an sdk.View snapshot corresponding to the current state.
func (n *Notes) view(ctx context.Context) (sdk.View, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)
	return n.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (n *Notes) viewLocked(version int) sdk.View {
	dirtyMarker := ""
	if n.dirty {
		dirtyMarker = "*"
	}

	switch n.prompt {
	case PromptRecovery:
		filePath := "[Untitled]"
		stagedAt := ""
		preview := ""
		if n.recoveryDraft != nil {
			if n.recoveryDraft.Path != "" {
				filePath = n.recoveryDraft.Path
			}
			stagedAt = n.recoveryDraft.StagedAt.UTC().Format(time.RFC3339)
			preview = truncateDetail(n.recoveryDraft.Content)
		}
		return sdk.View{
			Title: "Notes - Crash Recovery Draft Found",
			State: sdk.ViewReady,
			Items: []sdk.Item{
				{ID: "recovery_file", Label: "File", Detail: filePath},
				{ID: "recovery_time", Label: "Staged At", Detail: nonBlank(stagedAt, "unknown")},
				{ID: "recovery_preview", Label: "Draft Content", Detail: nonBlank(preview, "(empty)")},
			},
			Actions: []sdk.Action{
				{ID: "restore", Label: "Restore Draft"},
				{ID: "discard_recovery", Label: "Discard Draft"},
			},
			Status: "An unsaved recovery draft from a previous session was found.",
			Error:  truncateDetail(n.lastError),
		}

	case PromptUnsaved:
		return sdk.View{
			Title: fmt.Sprintf("Notes - Unsaved Changes%s", dirtyMarker),
			State: sdk.ViewReady,
			Items: []sdk.Item{
				{ID: "unsaved_doc", Label: "Current Document", Detail: fmt.Sprintf("%s (unsaved changes)", n.docIdentity())},
				{ID: "pending_op", Label: "Pending Action", Detail: fmt.Sprintf("%s %s", n.pendingAction, n.pendingPath)},
			},
			Actions: []sdk.Action{
				{ID: "save_and_proceed", Label: "Save and Proceed"},
				{ID: "discard_and_proceed", Label: "Discard and Proceed"},
				{ID: "cancel", Label: "Cancel"},
			},
			Status: "Current note has unsaved changes. Save or discard before continuing.",
			Error:  truncateDetail(n.lastError),
		}

	case PromptConflict:
		return sdk.View{
			Title: fmt.Sprintf("Notes - Save Conflict%s", dirtyMarker),
			State: sdk.ViewReady,
			Items: []sdk.Item{
				{ID: "conflict_doc", Label: "Document", Detail: n.docIdentity()},
				{ID: "disk_mod", Label: "Disk Modified At", Detail: nonBlank(n.conflictDiskModTime, "unknown")},
				{ID: "loaded_mod", Label: "Loaded Modified At", Detail: nonBlank(n.loadedModTime, "unknown")},
			},
			Fields: []sdk.Field{
				{ID: n.pathFieldID(), Label: "Save Path", Value: truncateDetail(n.path)},
			},
			Actions: []sdk.Action{
				{ID: "force_save", Label: "Overwrite Disk Copy"},
				{ID: "save_as", Label: "Save As Different Path"},
				{ID: "cancel", Label: "Cancel"},
			},
			Status: "File was modified externally on disk. Overwrite or save to a different path.",
			Error:  truncateDetail(n.lastError),
		}

	default:
		title := fmt.Sprintf("Notes - %s%s", n.docDisplayName(), dirtyMarker)
		docDetail := fmt.Sprintf("%s [saved]", n.docIdentity())
		if n.dirty {
			docDetail = fmt.Sprintf("%s [modified]", n.docIdentity())
		}
		statusText := n.lastStatus
		if statusText == "" {
			if n.dirty {
				statusText = "Modified (unsaved changes)"
			} else {
				statusText = "Saved (up to date)"
			}
		}

		fields := []sdk.Field{
			{ID: n.pathFieldID(), Label: "Path", Value: truncateDetail(n.path)},
		}
		switch {
		case version >= 3 && len(n.content) <= MaxAreaLength:
			// Contract v3 edits real documents in place: the multiline field
			// carries the whole buffer instead of the single-line preview.
			fields = append(fields, sdk.Field{
				ID: n.contentFieldID(), Label: "Content", Value: n.content, Multiline: true,
			})
		case version >= 3:
			// Larger than the multiline budget stays a read-only summary
			// rather than an editable view that would silently truncate.
			statusText = nonBlank(statusText, "Ready") + " (content exceeds the multiline field limit; read-only)"
		default:
			fields = append(fields, sdk.Field{
				ID: n.contentFieldID(), Label: "Content", Value: truncateDetail(n.content),
			})
		}

		return sdk.View{
			Title: title,
			State: sdk.ViewReady,
			Items: []sdk.Item{
				{ID: "doc_info", Label: "Document", Detail: docDetail},
				{ID: "mod_time", Label: "Disk Modified", Detail: nonBlank(n.loadedModTime, "Not saved to disk")},
				{ID: "char_count", Label: "Size", Detail: fmt.Sprintf("%d runes · %d bytes", utf8.RuneCountInString(n.content), len(n.content))},
			},
			Fields: fields,
			Actions: []sdk.Action{
				{ID: "save", Label: "Save"},
				{ID: "save_as", Label: "Save As"},
				{ID: "open", Label: "Open"},
				{ID: "new", Label: "New"},
			},
			Status: truncateDetail(statusText),
			Error:  truncateDetail(n.lastError),
		}
	}
}

// act dispatches a semantic action request and returns an updated view.
func (n *Notes) act(ctx context.Context, p sdk.ActionRequest) (sdk.View, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Extract values from dynamic or fixed field IDs
	pathKey := n.pathFieldID()
	contentKey := n.contentFieldID()

	newPath, hasPath := p.Values[pathKey]
	if !hasPath {
		newPath, hasPath = p.Values["path"]
	}
	newContent, hasContent := p.Values[contentKey]
	if !hasContent {
		newContent, hasContent = p.Values["content"]
	}

	// Update buffer if content field was modified
	if n.prompt == PromptNone {
		if hasContent && newContent != n.content {
			n.content = newContent
			if n.content != n.diskContent {
				n.dirty = true
				_ = n.stageRecoveryLocked(ctx)
			} else {
				n.dirty = false
				_ = n.clearRecoveryLocked(ctx)
			}
		}
		if hasPath && newPath != "" && p.Action != "open" && p.Action != "save_as" {
			n.path = newPath
		}
	}

	switch p.Action {
	case "restore":
		if n.recoveryDraft != nil {
			n.path = n.recoveryDraft.Path
			n.content = n.recoveryDraft.Content
			n.loadedModTime = n.recoveryDraft.BaseModTime
			n.loadedSize = n.recoveryDraft.BaseSize
			n.dirty = true
			n.docRev++
			n.prompt = PromptNone
			n.lastStatus = "Recovery draft restored. Save to commit changes."
			n.lastError = ""
		}

	case "discard_recovery":
		_ = n.clearRecoveryLocked(ctx)
		n.prompt = PromptNone
		n.lastStatus = "Recovery draft discarded."
		n.lastError = ""

	case "save_and_proceed":
		target := n.path
		if target == "" && hasPath && newPath != "" {
			target = newPath
		}
		if err := n.saveLocked(ctx, target, n.content, false, true); err != nil {
			n.lastError = err.Error()
		} else {
			op, tgt := n.pendingAction, n.pendingPath
			n.pendingAction, n.pendingPath = "", ""
			n.prompt = PromptNone
			if op == "open" {
				if err := n.openLocked(ctx, tgt, true); err != nil {
					n.lastError = err.Error()
				}
			} else if op == "new" {
				_ = n.newLocked(ctx, true)
			}
		}

	case "discard_and_proceed":
		op, tgt := n.pendingAction, n.pendingPath
		n.pendingAction, n.pendingPath = "", ""
		n.prompt = PromptNone
		_ = n.clearRecoveryLocked(ctx)
		if op == "open" {
			if err := n.openLocked(ctx, tgt, true); err != nil {
				n.lastError = err.Error()
			}
		} else if op == "new" {
			_ = n.newLocked(ctx, true)
		}

	case "cancel":
		n.prompt = PromptNone
		n.pendingAction, n.pendingPath = "", ""
		n.lastError = ""

	case "force_save":
		target := n.path
		if target == "" && hasPath && newPath != "" {
			target = newPath
		}
		if err := n.saveLocked(ctx, target, n.content, true, true); err != nil {
			n.lastError = err.Error()
		} else {
			n.prompt = PromptNone
			n.lastError = ""
		}

	case "save":
		target := n.path
		if target == "" && hasPath && newPath != "" {
			target = newPath
		}
		if err := n.saveLocked(ctx, target, n.content, false, true); err != nil {
			n.lastError = err.Error()
		} else {
			n.lastError = ""
		}

	case "save_as":
		target := newPath
		if target == "" {
			target = n.path
		}
		if target == "" {
			n.lastError = "save as: path is required"
		} else {
			if err := n.saveLocked(ctx, target, n.content, false, true); err != nil {
				n.lastError = err.Error()
			} else {
				n.lastError = ""
			}
		}

	case "open":
		target := newPath
		if target == "" {
			n.lastError = "open: path is required"
		} else if n.dirty {
			n.prompt = PromptUnsaved
			n.pendingAction = "open"
			n.pendingPath = target
		} else {
			if err := n.openLocked(ctx, target, false); err != nil {
				n.lastError = err.Error()
			} else {
				n.lastError = ""
			}
		}

	case "new":
		if n.dirty {
			n.prompt = PromptUnsaved
			n.pendingAction = "new"
			n.pendingPath = ""
		} else {
			if err := n.newLocked(ctx, false); err != nil {
				n.lastError = err.Error()
			} else {
				n.lastError = ""
			}
		}

	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", p.Action)
	}

	return n.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

// Programmatic IPC route handlers

func (n *Notes) doc(ctx context.Context, raw json.RawMessage) (any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)

	return map[string]any{
		"path":               n.path,
		"content":            n.content,
		"dirty":              n.dirty,
		"mod_time":           n.loadedModTime,
		"size":               len(n.content),
		"recovery_available": n.recoveryDraft != nil,
		"prompt":             string(n.prompt),
	}, nil
}

func (n *Notes) openRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Path  string `json:"path"`
		Force bool   `json:"force"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)

	if err := n.openLocked(ctx, p.Path, p.Force); err != nil {
		return nil, err
	}

	return map[string]any{
		"path":     n.path,
		"mod_time": n.loadedModTime,
		"size":     len(n.content),
		"dirty":    n.dirty,
	}, nil
}

func (n *Notes) saveRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Path      string  `json:"path"`
		Content   *string `json:"content"`
		Force     bool    `json:"force"`
		Overwrite *bool   `json:"overwrite"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)

	content := n.content
	if p.Content != nil {
		content = *p.Content
	}
	target := p.Path
	if target == "" {
		target = n.path
	}
	overwrite := true
	if p.Overwrite != nil {
		overwrite = *p.Overwrite
	}

	if err := n.saveLocked(ctx, target, content, p.Force, overwrite); err != nil {
		return nil, err
	}

	return map[string]any{
		"saved":    true,
		"path":     n.path,
		"mod_time": n.loadedModTime,
		"size":     len(n.content),
		"dirty":    n.dirty,
	}, nil
}

func (n *Notes) saveAsRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Path      string  `json:"path"`
		Content   *string `json:"content"`
		Overwrite *bool   `json:"overwrite"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)

	content := n.content
	if p.Content != nil {
		content = *p.Content
	}
	overwrite := true
	if p.Overwrite != nil {
		overwrite = *p.Overwrite
	}

	if err := n.saveLocked(ctx, p.Path, content, false, overwrite); err != nil {
		return nil, err
	}

	return map[string]any{
		"saved":    true,
		"path":     n.path,
		"mod_time": n.loadedModTime,
		"size":     len(n.content),
		"dirty":    n.dirty,
	}, nil
}

func (n *Notes) editRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Content string `json:"content"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)

	if err := n.editLocked(ctx, p.Content); err != nil {
		return nil, err
	}

	return map[string]any{
		"dirty": n.dirty,
		"size":  len(n.content),
	}, nil
}

func (n *Notes) newRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Force bool `json:"force"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)

	if err := n.newLocked(ctx, p.Force); err != nil {
		return nil, err
	}

	return map[string]any{
		"path":  n.path,
		"size":  0,
		"dirty": false,
	}, nil
}

func (n *Notes) recoverRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Restore bool `json:"restore"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureRecoveryCheckedLocked(ctx)

	if n.recoveryDraft == nil {
		return nil, fmt.Errorf("no recovery draft available")
	}

	if p.Restore {
		n.path = n.recoveryDraft.Path
		n.content = n.recoveryDraft.Content
		n.loadedModTime = n.recoveryDraft.BaseModTime
		n.loadedSize = n.recoveryDraft.BaseSize
		n.dirty = true
		n.docRev++
		n.prompt = PromptNone
		n.lastStatus = "Recovery draft restored."
		n.lastError = ""
		return map[string]any{
			"restored": true,
			"path":     n.path,
			"size":     len(n.content),
			"dirty":    true,
		}, nil
	}

	_ = n.clearRecoveryLocked(ctx)
	n.prompt = PromptNone
	n.lastStatus = "Recovery draft discarded."
	n.lastError = ""
	return map[string]any{
		"discarded": true,
	}, nil
}
