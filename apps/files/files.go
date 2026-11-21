// Package files is Gostalgia's standard virtual filesystem browser application.
// It provides keyboard-first browsing, path breadcrumbs, sorting/filtering,
// file metadata display, bounded text previews, file/directory management
// (create, rename, copy, move, trash, restore, conflict detection), and
// open/handoff actions for registered applications like Notes.
package files

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"gostalgia/sdk"
)

const (
	// ID is the reverse-DNS identifier for Files.
	ID = "com.gostalgia.files"

	// DefaultBrowsePath is the default starting directory.
	DefaultBrowsePath = "/users/guest"

	// FallbackBrowsePath is used if the default path cannot be listed.
	FallbackBrowsePath = "/users/guest/documents"

	// RootBrowsePath is the ultimate fallback directory.
	RootBrowsePath = "/"

	// MaxPreviewLength is the maximum length of text preview shown in status.
	MaxPreviewLength = 200

	// MaxPreviewReadBytes is the maximum number of bytes read for previewing.
	MaxPreviewReadBytes = 1024

	// MaxDisplayedEntries is the maximum number of items rendered in View.Items (total limit 100).
	MaxDisplayedEntries = 98

	// NotesAppID is the identifier for the standard Notes text editor.
	NotesAppID = "com.gostalgia.notes"
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err)
	}
	return m
}

// ManifestJSON returns a copy of the raw embedded JSON.
func ManifestJSON() []byte {
	return append([]byte(nil), manifestJSON...)
}

// SortMode specifies entry ordering in the directory listing.
type SortMode string

const (
	SortName SortMode = "name"
	SortSize SortMode = "size"
	SortTime SortMode = "time"
)

// PromptState controls modal dialog presentation.
type PromptState string

const (
	PromptNone         PromptState = ""
	PromptConfirmTrash PromptState = "confirm_trash"
	PromptConflict     PromptState = "conflict"
	PromptTrashView    PromptState = "trash_view"
)

// fileEntry represents an entry returned from fs/list.
type fileEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time,omitempty"`
}

// trashItem represents a trashed entry from fs/trash/list.
type trashItem struct {
	ID           string `json:"id"`
	OriginalPath string `json:"original_path"`
	TrashPath    string `json:"trash_path"`
	Name         string `json:"name"`
	IsDir        bool   `json:"is_dir"`
	Size         int64  `json:"size"`
	TrashedAt    string `json:"trashed_at"`
}

// Files implements the VFS-backed Files browser application.
type Files struct {
	app *sdk.Context
	mu  sync.Mutex

	cwd           string
	entries       []fileEntry
	selectedName  string
	selectedIsDir bool
	selectedSize  int64
	filter        string
	targetInput   string
	sortMode      SortMode

	// Preview cache
	previewText string
	previewPath string

	// Trash cache for PromptTrashView
	trashEntries    []trashItem
	selectedTrashID string

	// Modal prompts and pending operation state
	prompt     PromptState
	pendingOp  string // "rename", "copy", "move"
	pendingSrc string
	pendingDst string

	lastStatus string
	lastError  string

	initialized bool
}

// Factory constructs a fresh Files application instance.
func Factory() (sdk.Instance, error) {
	return &Files{
		cwd:      DefaultBrowsePath,
		sortMode: SortName,
		prompt:   PromptNone,
	}, nil
}

// Init registers application routes and presentation callbacks.
func (f *Files) Init(app *sdk.Context) error {
	f.app = app

	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"browse", f.browseRoute},
		{"stat", f.statRoute},
		{"preview", f.previewRoute},
		{"open", f.openRoute},
		{"trash_list", f.trashListRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}

	return app.Present(f.view, f.act)
}

// Run executes the application background loop.
func (f *Files) Run(ctx context.Context) error {
	f.app.Log.Info("files manager running")
	<-ctx.Done()
	return nil
}

// Stop releases resources when the application terminates.
func (f *Files) Stop(ctx context.Context) error {
	return nil
}

// view constructs a snapshot of the current browsing or modal state.
func (f *Files) view(ctx context.Context) (sdk.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.initialized {
		f.initialized = true
		f.ensureInitialDirLocked(ctx)
	} else if f.prompt == PromptNone {
		// Refresh directory contents to show external modifications
		f.refreshEntriesLocked(ctx)
	}

	return f.viewLocked(ctx), nil
}

// ensureInitialDirLocked discovers the best available initial directory.
func (f *Files) ensureInitialDirLocked(ctx context.Context) {
	for _, candidate := range []string{DefaultBrowsePath, FallbackBrowsePath, RootBrowsePath} {
		if err := f.loadDirLocked(ctx, candidate); err == nil {
			return
		}
	}
	f.cwd = DefaultBrowsePath
	f.entries = nil
	f.selectedName = ""
}

// loadDirLocked changes directory and reloads entry listings.
func (f *Files) loadDirLocked(ctx context.Context, targetPath string) error {
	cleanPath := path.Clean(targetPath)
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}

	var listOut struct {
		Path    string      `json:"path"`
		Entries []fileEntry `json:"entries"`
	}
	err := f.app.Call(ctx, "fs/list", map[string]string{"path": cleanPath}, &listOut)
	if err != nil {
		f.lastError = fmt.Sprintf("browse %s: %v", cleanPath, err)
		return err
	}

	f.cwd = cleanPath
	f.entries = listOut.Entries
	f.sortEntriesLocked()
	f.adjustSelectionLocked()
	f.lastError = ""
	f.updatePreviewLocked(ctx)
	return nil
}

// refreshEntriesLocked reloads current directory entries without clearing errors.
func (f *Files) refreshEntriesLocked(ctx context.Context) {
	var listOut struct {
		Path    string      `json:"path"`
		Entries []fileEntry `json:"entries"`
	}
	err := f.app.Call(ctx, "fs/list", map[string]string{"path": f.cwd}, &listOut)
	if err != nil {
		f.lastError = fmt.Sprintf("refresh %s: %v", f.cwd, err)
		return
	}
	f.entries = listOut.Entries
	f.sortEntriesLocked()
	f.adjustSelectionLocked()
	f.updatePreviewLocked(ctx)
}

// sortEntriesLocked sorts directory entries based on current sortMode.
// Directories are always grouped first.
func (f *Files) sortEntriesLocked() {
	sort.SliceStable(f.entries, func(i, j int) bool {
		a, b := f.entries[i], f.entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir // directories first
		}
		switch f.sortMode {
		case SortSize:
			if a.Size != b.Size {
				return a.Size > b.Size // largest first
			}
		case SortTime:
			if a.ModTime != b.ModTime {
				return a.ModTime > b.ModTime // newest first
			}
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// filteredEntriesLocked returns directory entries matching current filter query.
func (f *Files) filteredEntriesLocked() []fileEntry {
	filterLower := strings.ToLower(strings.TrimSpace(f.filter))
	if filterLower == "" {
		return f.entries
	}
	var out []fileEntry
	for _, e := range f.entries {
		if strings.Contains(strings.ToLower(e.Name), filterLower) {
			out = append(out, e)
		}
	}
	return out
}

// adjustSelectionLocked keeps the current selection if it still exists,
// or resets selection to the first available item.
func (f *Files) adjustSelectionLocked() {
	visible := f.filteredEntriesLocked()
	if f.selectedName == ".." && f.cwd != "/" {
		f.selectedIsDir = true
		return
	}
	for _, e := range visible {
		if e.Name == f.selectedName {
			f.selectedIsDir = e.IsDir
			f.selectedSize = e.Size
			return
		}
	}
	if len(visible) > 0 {
		f.selectedName = visible[0].Name
		f.selectedIsDir = visible[0].IsDir
		f.selectedSize = visible[0].Size
	} else {
		f.selectedName = ""
		f.selectedIsDir = false
		f.selectedSize = 0
	}
}

// updatePreviewLocked fetches bounded text preview for the currently selected file.
func (f *Files) updatePreviewLocked(ctx context.Context) {
	if f.selectedName == "" || f.selectedName == ".." || f.selectedIsDir {
		if f.selectedName == ".." {
			f.previewText = "Parent directory"
		} else if f.selectedIsDir {
			f.previewText = "Directory"
		} else {
			f.previewText = ""
		}
		f.previewPath = ""
		return
	}

	targetPath := path.Join(f.cwd, f.selectedName)
	if f.previewPath == targetPath && f.previewText != "" {
		return
	}

	f.previewPath = targetPath
	if f.selectedSize == 0 {
		f.previewText = "(Empty file)"
		return
	}

	var readOut struct {
		DataBase64 string `json:"data_base64"`
		TotalSize  int64  `json:"total_size"`
	}
	err := f.app.Call(ctx, "fs/read", map[string]any{
		"path":   targetPath,
		"offset": 0,
		"limit":  MaxPreviewReadBytes,
	}, &readOut)
	if err != nil {
		f.previewText = fmt.Sprintf("[Preview: %v]", err)
		return
	}

	data, err := base64.StdEncoding.DecodeString(readOut.DataBase64)
	if err != nil {
		f.previewText = "[Error decoding preview]"
		return
	}

	if !isText(data) {
		f.previewText = fmt.Sprintf("[Binary data: %s]", formatSize(readOut.TotalSize))
		return
	}

	preview := sanitizeString(string(data))
	if utf8.RuneCountInString(preview) > MaxPreviewLength {
		preview = string([]rune(preview)[:MaxPreviewLength]) + "..."
	}
	f.previewText = fmt.Sprintf("Preview: %q", preview)
}

// viewLocked renders the snapshot according to the active prompt.
func (f *Files) viewLocked(ctx context.Context) sdk.View {
	switch f.prompt {
	case PromptConfirmTrash:
		return sdk.View{
			Title: "Files - Confirm Move to Trash",
			State: sdk.ViewReady,
			Items: []sdk.Item{
				{ID: "item_target", Label: "Target", Detail: truncateString(f.pendingSrc, 4000)},
				{ID: "item_notice", Label: "Action", Detail: "Item will be moved to the trash and can be restored later."},
			},
			Actions: []sdk.Action{
				{ID: "confirm_trash", Label: "Confirm Move to Trash"},
				{ID: "cancel", Label: "Cancel"},
			},
			Status: sanitizeString(f.lastStatus),
			Error:  truncateString(f.lastError, 4000),
		}

	case PromptConflict:
		return sdk.View{
			Title: "Files - Destination Exists",
			State: sdk.ViewReady,
			Items: []sdk.Item{
				{ID: "item_op", Label: "Operation", Detail: f.pendingOp},
				{ID: "item_src", Label: "Source", Detail: truncateString(f.pendingSrc, 4000)},
				{ID: "item_dst", Label: "Destination Exists", Detail: truncateString(f.pendingDst, 4000)},
			},
			Actions: []sdk.Action{
				{ID: "confirm_overwrite", Label: "Overwrite Destination"},
				{ID: "cancel", Label: "Cancel"},
			},
			Status: sanitizeString(f.lastStatus),
			Error:  truncateString(f.lastError, 4000),
		}

	case PromptTrashView:
		var items []sdk.Item
		if len(f.trashEntries) == 0 {
			items = append(items, sdk.Item{
				ID:     "item_empty",
				Label:  "Trash Bin",
				Detail: "(No items in trash)",
			})
		} else {
			limit := len(f.trashEntries)
			if limit > MaxDisplayedEntries {
				limit = MaxDisplayedEntries
			}
			for i := 0; i < limit; i++ {
				e := f.trashEntries[i]
				items = append(items, sdk.Item{
					ID:     fmt.Sprintf("trash_%d", i),
					Label:  "[TRASH] " + truncateString(e.Name, 240),
					Detail: fmt.Sprintf("Original: %s · Trashed: %s · Size: %s", truncateString(e.OriginalPath, 100), e.TrashedAt, formatSize(e.Size)),
				})
			}
			if len(f.trashEntries) > limit {
				items = append(items, sdk.Item{
					ID:     "item_more",
					Label:  fmt.Sprintf("[+ %d more entries]", len(f.trashEntries)-limit),
					Detail: "Trash listing truncated",
				})
			}
		}

		return sdk.View{
			Title: "Files - Trash Bin",
			State: sdk.ViewReady,
			Items: items,
			Actions: []sdk.Action{
				{ID: "restore", Label: "Restore Item", Disabled: len(f.trashEntries) == 0},
				{ID: "empty_trash", Label: "Empty Trash", Disabled: len(f.trashEntries) == 0},
				{ID: "back", Label: "Back to Files"},
			},
			Status: sanitizeString(f.lastStatus),
			Error:  truncateString(f.lastError, 4000),
		}

	default:
		visible := f.filteredEntriesLocked()
		var items []sdk.Item

		if f.cwd != "/" {
			items = append(items, sdk.Item{
				ID:     "item_parent",
				Label:  "[..] Up",
				Detail: "Parent Directory",
			})
		}

		if len(visible) == 0 {
			detailText := "No files in directory"
			if strings.TrimSpace(f.filter) != "" {
				detailText = fmt.Sprintf("No files match filter %q", f.filter)
			}
			items = append(items, sdk.Item{
				ID:     "item_empty",
				Label:  "(Empty)",
				Detail: detailText,
			})
		} else {
			limit := len(visible)
			if limit > MaxDisplayedEntries {
				limit = MaxDisplayedEntries
			}
			for i := 0; i < limit; i++ {
				e := visible[i]
				var label, detail string
				if e.IsDir {
					label = "[DIR] " + truncateString(e.Name, 240)
					detail = "Directory"
				} else {
					label = "[FILE] " + truncateString(e.Name, 240)
					detail = formatSize(e.Size)
					if e.ModTime != "" {
						detail += " · " + e.ModTime
					}
				}
				items = append(items, sdk.Item{
					ID:     fmt.Sprintf("item_%d", i),
					Label:  label,
					Detail: detail,
				})
			}
			if len(visible) > limit {
				items = append(items, sdk.Item{
					ID:     "item_more",
					Label:  fmt.Sprintf("[+ %d more entries]", len(visible)-limit),
					Detail: "Directory listing truncated",
				})
			}
		}

		title := fmt.Sprintf("Files - %s", f.cwd)
		if strings.TrimSpace(f.filter) != "" {
			title = fmt.Sprintf("Files - %s [filter: %s]", f.cwd, f.filter)
		}

		var sortLabel string
		switch f.sortMode {
		case SortSize:
			sortLabel = "Sort: Size"
		case SortTime:
			sortLabel = "Sort: Date"
		default:
			sortLabel = "Sort: Name"
		}

		statusText := f.lastStatus
		if statusText == "" {
			if f.selectedName != "" {
				if f.previewText != "" {
					statusText = fmt.Sprintf("Selected: %s | %s", f.selectedName, f.previewText)
				} else {
					statusText = fmt.Sprintf("Selected: %s", f.selectedName)
				}
			} else {
				statusText = fmt.Sprintf("%d items in %s", len(visible), f.cwd)
			}
		} else if f.selectedName != "" && f.previewText != "" {
			statusText = fmt.Sprintf("%s | %s", statusText, f.previewText)
		}

		return sdk.View{
			Title: truncateString(title, 256),
			State: sdk.ViewReady,
			Items: items,
			Fields: []sdk.Field{
				{ID: "filter", Label: "Filter", Value: f.filter},
				{ID: "path", Label: "Path", Value: f.cwd},
				{ID: "target", Label: "Target", Value: f.targetInput},
			},
			Actions: []sdk.Action{
				{ID: "open", Label: "Open"},
				{ID: "up", Label: "Up", Disabled: f.cwd == "/"},
				{ID: "preview", Label: "Preview"},
				{ID: "new_file", Label: "New File"},
				{ID: "new_dir", Label: "New Folder"},
				{ID: "rename", Label: "Rename", Disabled: f.selectedName == "" || f.selectedName == ".."},
				{ID: "copy", Label: "Copy", Disabled: f.selectedName == "" || f.selectedName == ".."},
				{ID: "move", Label: "Move", Disabled: f.selectedName == "" || f.selectedName == ".."},
				{ID: "trash", Label: "Trash", Disabled: f.selectedName == "" || f.selectedName == ".."},
				{ID: "trash_bin", Label: "Trash Bin"},
				{ID: "sort", Label: sortLabel},
				{ID: "refresh", Label: "Refresh"},
			},
			Status: truncateString(statusText, 4000),
			Error:  truncateString(f.lastError, 4000),
		}
	}
}

// act dispatches semantic user action requests.
func (f *Files) act(ctx context.Context, p sdk.ActionRequest) (sdk.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Clear transient error banner on each action
	f.lastError = ""

	// Ingest field values
	if f.prompt == PromptNone {
		if v, ok := p.Values["filter"]; ok {
			f.filter = v
		}
		if v, ok := p.Values["target"]; ok {
			f.targetInput = v
		}
		if v, ok := p.Values["path"]; ok && v != "" && v != f.cwd {
			// If path field changed and action is open or refresh, navigate there
			if p.Action == "open" || p.Action == "refresh" {
				_ = f.loadDirLocked(ctx, v)
				return f.viewLocked(ctx), nil
			}
		}
	}

	// Ingest selection from p.ItemID
	if f.prompt == PromptNone {
		if p.ItemID == "item_parent" {
			f.selectedName = ".."
			f.selectedIsDir = true
			f.selectedSize = 0
		} else if strings.HasPrefix(p.ItemID, "item_") && p.ItemID != "item_more" && p.ItemID != "item_empty" {
			var idx int
			if _, err := fmt.Sscanf(p.ItemID, "item_%d", &idx); err == nil {
				visible := f.filteredEntriesLocked()
				if idx >= 0 && idx < len(visible) {
					f.selectedName = visible[idx].Name
					f.selectedIsDir = visible[idx].IsDir
					f.selectedSize = visible[idx].Size
				}
			}
		}
	} else if f.prompt == PromptTrashView {
		if strings.HasPrefix(p.ItemID, "trash_") && p.ItemID != "item_more" && p.ItemID != "item_empty" {
			var idx int
			if _, err := fmt.Sscanf(p.ItemID, "trash_%d", &idx); err == nil {
				if idx >= 0 && idx < len(f.trashEntries) {
					f.selectedTrashID = f.trashEntries[idx].ID
				}
			}
		}
	}

	switch p.Action {
	case "open":
		f.handleOpenLocked(ctx)

	case "up":
		if f.cwd != "/" {
			_ = f.loadDirLocked(ctx, path.Dir(f.cwd))
			f.lastStatus = fmt.Sprintf("Browsing %s", f.cwd)
		}

	case "preview":
		f.updatePreviewLocked(ctx)
		f.lastStatus = f.previewText

	case "new_file":
		f.handleNewFileLocked(ctx)

	case "new_dir":
		f.handleNewDirLocked(ctx)

	case "rename":
		f.handleRenameLocked(ctx)

	case "copy":
		f.handleCopyLocked(ctx)

	case "move":
		f.handleMoveLocked(ctx)

	case "trash":
		f.handleTrashPromptLocked()

	case "confirm_trash":
		f.handleConfirmTrashLocked(ctx)

	case "confirm_overwrite":
		f.handleConfirmOverwriteLocked(ctx)

	case "cancel":
		f.prompt = PromptNone
		f.pendingOp = ""
		f.pendingSrc = ""
		f.pendingDst = ""
		f.lastStatus = "Operation canceled"

	case "trash_bin":
		f.prompt = PromptTrashView
		f.loadTrashLocked(ctx)

	case "restore":
		f.handleRestoreLocked(ctx)

	case "empty_trash":
		f.handleEmptyTrashLocked(ctx)

	case "back":
		f.prompt = PromptNone
		_ = f.loadDirLocked(ctx, f.cwd)

	case "sort":
		switch f.sortMode {
		case SortName:
			f.sortMode = SortSize
		case SortSize:
			f.sortMode = SortTime
		case SortTime:
			f.sortMode = SortName
		}
		f.sortEntriesLocked()
		f.lastStatus = fmt.Sprintf("Sorted by %s", f.sortMode)

	case "refresh":
		_ = f.loadDirLocked(ctx, f.cwd)
		f.lastStatus = fmt.Sprintf("Refreshed %s", f.cwd)
	}

	return f.viewLocked(ctx), nil
}

func (f *Files) handleOpenLocked(ctx context.Context) {
	if f.selectedName == ".." || f.selectedName == "" {
		if f.cwd != "/" {
			_ = f.loadDirLocked(ctx, path.Dir(f.cwd))
			f.lastStatus = fmt.Sprintf("Browsing %s", f.cwd)
		}
		return
	}

	targetPath := path.Clean(path.Join(f.cwd, f.selectedName))
	if f.selectedIsDir {
		_ = f.loadDirLocked(ctx, targetPath)
		f.lastStatus = fmt.Sprintf("Browsing %s", f.cwd)
		return
	}

	// Selected item is a file: open via document handoff if possible
	var res sdk.HandoffResult
	err := f.app.Call(ctx, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    targetPath,
		Mode:    "read-write",
	}, &res)
	if err == nil {
		appName := "Notes"
		if res.AppID != NotesAppID && res.AppID != "" {
			appName = res.AppID
		}
		f.lastStatus = fmt.Sprintf("Opened %s in %s", f.selectedName, appName)
		return
	}

	// Selected item is a file: fallback to direct Notes open
	_ = f.app.Call(ctx, "app/launch", map[string]string{"id": NotesAppID}, nil)
	err = f.app.Call(ctx, "app/"+NotesAppID+"/open", map[string]any{
		"path":  targetPath,
		"force": false,
	}, nil)
	if err != nil {
		f.lastError = fmt.Sprintf("Open in Notes failed: %v", err)
	} else {
		f.lastStatus = fmt.Sprintf("Opened %s in Notes", f.selectedName)
	}
}

func (f *Files) handleNewFileLocked(ctx context.Context) {
	target := strings.TrimSpace(f.targetInput)
	if target == "" {
		f.lastError = "Target name is required to create a file"
		return
	}
	targetPath := path.Clean(path.Join(f.cwd, target))

	var statOut struct {
		Name string `json:"name"`
	}
	if err := f.app.Call(ctx, "fs/stat", map[string]string{"path": targetPath}, &statOut); err == nil {
		f.lastError = fmt.Sprintf("File %s already exists", target)
		return
	}

	err := f.app.Call(ctx, "fs/save", map[string]any{
		"path":        targetPath,
		"data_base64": "",
	}, nil)
	if err != nil {
		f.lastError = fmt.Sprintf("Create file failed: %v", err)
		return
	}

	f.lastStatus = fmt.Sprintf("Created file %s", target)
	f.targetInput = ""
	f.selectedName = target
	_ = f.loadDirLocked(ctx, f.cwd)
}

func (f *Files) handleNewDirLocked(ctx context.Context) {
	target := strings.TrimSpace(f.targetInput)
	if target == "" {
		f.lastError = "Target name is required to create a folder"
		return
	}
	targetPath := path.Clean(path.Join(f.cwd, target))

	err := f.app.Call(ctx, "fs/mkdir", map[string]string{"path": targetPath}, nil)
	if err != nil {
		f.lastError = fmt.Sprintf("Create folder failed: %v", err)
		return
	}

	f.lastStatus = fmt.Sprintf("Created folder %s", target)
	f.targetInput = ""
	f.selectedName = target
	_ = f.loadDirLocked(ctx, f.cwd)
}

func (f *Files) handleRenameLocked(ctx context.Context) {
	if f.selectedName == "" || f.selectedName == ".." {
		f.lastError = "Select a file or folder to rename"
		return
	}
	target := strings.TrimSpace(f.targetInput)
	if target == "" {
		f.lastError = "Target name is required for rename"
		return
	}

	srcPath := path.Clean(path.Join(f.cwd, f.selectedName))
	dstPath := path.Clean(path.Join(f.cwd, target))
	if srcPath == dstPath {
		f.lastStatus = "Source and destination are identical"
		return
	}

	var statOut struct {
		Name string `json:"name"`
	}
	if err := f.app.Call(ctx, "fs/stat", map[string]string{"path": dstPath}, &statOut); err == nil {
		// Destination exists: enter conflict resolution
		f.pendingOp = "rename"
		f.pendingSrc = srcPath
		f.pendingDst = dstPath
		f.prompt = PromptConflict
		f.lastStatus = fmt.Sprintf("Destination %s exists. Overwrite?", target)
		return
	}

	err := f.app.Call(ctx, "fs/rename", map[string]any{
		"src":       srcPath,
		"dst":       dstPath,
		"overwrite": false,
	}, nil)
	if err != nil {
		f.lastError = fmt.Sprintf("Rename failed: %v", err)
		return
	}

	f.lastStatus = fmt.Sprintf("Renamed %s to %s", f.selectedName, target)
	f.selectedName = target
	f.targetInput = ""
	_ = f.loadDirLocked(ctx, f.cwd)
}

func (f *Files) handleCopyLocked(ctx context.Context) {
	if f.selectedName == "" || f.selectedName == ".." {
		f.lastError = "Select a file or folder to copy"
		return
	}
	target := strings.TrimSpace(f.targetInput)
	if target == "" {
		f.lastError = "Target destination is required for copy"
		return
	}

	srcPath := path.Clean(path.Join(f.cwd, f.selectedName))
	var dstPath string
	if strings.HasPrefix(target, "/") {
		dstPath = path.Clean(target)
	} else {
		dstPath = path.Clean(path.Join(f.cwd, target))
	}

	// If destination is an existing directory, copy into it
	var statOut struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	}
	if err := f.app.Call(ctx, "fs/stat", map[string]string{"path": dstPath}, &statOut); err == nil && statOut.IsDir {
		dstPath = path.Clean(path.Join(dstPath, f.selectedName))
	}

	if err := f.app.Call(ctx, "fs/stat", map[string]string{"path": dstPath}, &statOut); err == nil {
		f.pendingOp = "copy"
		f.pendingSrc = srcPath
		f.pendingDst = dstPath
		f.prompt = PromptConflict
		f.lastStatus = fmt.Sprintf("Destination %s exists. Overwrite?", dstPath)
		return
	}

	err := f.app.Call(ctx, "fs/copy", map[string]any{
		"src":       srcPath,
		"dst":       dstPath,
		"overwrite": false,
	}, nil)
	if err != nil {
		f.lastError = fmt.Sprintf("Copy failed: %v", err)
		return
	}

	f.lastStatus = fmt.Sprintf("Copied %s to %s", f.selectedName, dstPath)
	f.targetInput = ""
	_ = f.loadDirLocked(ctx, f.cwd)
}

func (f *Files) handleMoveLocked(ctx context.Context) {
	if f.selectedName == "" || f.selectedName == ".." {
		f.lastError = "Select a file or folder to move"
		return
	}
	target := strings.TrimSpace(f.targetInput)
	if target == "" {
		f.lastError = "Target destination is required for move"
		return
	}

	srcPath := path.Clean(path.Join(f.cwd, f.selectedName))
	var dstPath string
	if strings.HasPrefix(target, "/") {
		dstPath = path.Clean(target)
	} else {
		dstPath = path.Clean(path.Join(f.cwd, target))
	}

	var statOut struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	}
	if err := f.app.Call(ctx, "fs/stat", map[string]string{"path": dstPath}, &statOut); err == nil && statOut.IsDir {
		dstPath = path.Clean(path.Join(dstPath, f.selectedName))
	}

	if err := f.app.Call(ctx, "fs/stat", map[string]string{"path": dstPath}, &statOut); err == nil {
		f.pendingOp = "move"
		f.pendingSrc = srcPath
		f.pendingDst = dstPath
		f.prompt = PromptConflict
		f.lastStatus = fmt.Sprintf("Destination %s exists. Overwrite?", dstPath)
		return
	}

	err := f.app.Call(ctx, "fs/move", map[string]any{
		"src":       srcPath,
		"dst":       dstPath,
		"overwrite": false,
	}, nil)
	if err != nil {
		f.lastError = fmt.Sprintf("Move failed: %v", err)
		return
	}

	f.lastStatus = fmt.Sprintf("Moved %s to %s", f.selectedName, dstPath)
	f.targetInput = ""
	_ = f.loadDirLocked(ctx, f.cwd)
}

func (f *Files) handleTrashPromptLocked() {
	if f.selectedName == "" || f.selectedName == ".." {
		f.lastError = "Select a file or folder to trash"
		return
	}
	f.pendingSrc = path.Clean(path.Join(f.cwd, f.selectedName))
	f.prompt = PromptConfirmTrash
	f.lastStatus = fmt.Sprintf("Move %s to trash?", f.selectedName)
}

func (f *Files) handleConfirmTrashLocked(ctx context.Context) {
	if f.pendingSrc == "" {
		f.prompt = PromptNone
		return
	}

	err := f.app.Call(ctx, "fs/trash", map[string]string{"path": f.pendingSrc}, nil)
	if err != nil {
		f.lastError = fmt.Sprintf("Trash failed: %v", err)
	} else {
		f.lastStatus = fmt.Sprintf("Moved %s to trash", path.Base(f.pendingSrc))
		f.selectedName = ""
	}
	f.prompt = PromptNone
	f.pendingSrc = ""
	_ = f.loadDirLocked(ctx, f.cwd)
}

func (f *Files) handleConfirmOverwriteLocked(ctx context.Context) {
	var err error
	switch f.pendingOp {
	case "rename":
		err = f.app.Call(ctx, "fs/rename", map[string]any{
			"src":       f.pendingSrc,
			"dst":       f.pendingDst,
			"overwrite": true,
		}, nil)
	case "copy":
		err = f.app.Call(ctx, "fs/copy", map[string]any{
			"src":       f.pendingSrc,
			"dst":       f.pendingDst,
			"overwrite": true,
		}, nil)
	case "move":
		err = f.app.Call(ctx, "fs/move", map[string]any{
			"src":       f.pendingSrc,
			"dst":       f.pendingDst,
			"overwrite": true,
		}, nil)
	}

	if err != nil {
		f.lastError = fmt.Sprintf("Overwrite failed: %v", err)
	} else {
		f.lastStatus = fmt.Sprintf("%s overwritten: %s", f.pendingOp, path.Base(f.pendingDst))
		f.selectedName = path.Base(f.pendingDst)
		f.targetInput = ""
	}

	f.prompt = PromptNone
	f.pendingOp = ""
	f.pendingSrc = ""
	f.pendingDst = ""
	_ = f.loadDirLocked(ctx, f.cwd)
}

func (f *Files) loadTrashLocked(ctx context.Context) {
	var out struct {
		Entries []trashItem `json:"entries"`
	}
	err := f.app.Call(ctx, "fs/trash/list", nil, &out)
	if err != nil {
		f.lastError = fmt.Sprintf("List trash failed: %v", err)
		f.trashEntries = nil
		return
	}
	f.trashEntries = out.Entries
	if len(f.trashEntries) > 0 {
		f.selectedTrashID = f.trashEntries[0].ID
	} else {
		f.selectedTrashID = ""
	}
	f.lastStatus = fmt.Sprintf("%d items in trash", len(f.trashEntries))
}

func (f *Files) handleRestoreLocked(ctx context.Context) {
	if f.selectedTrashID == "" {
		f.lastError = "Select a trashed item to restore"
		return
	}

	var restoreOut struct {
		ID           string `json:"id"`
		RestoredPath string `json:"restored_path"`
	}
	err := f.app.Call(ctx, "fs/restore", map[string]any{
		"id":        f.selectedTrashID,
		"overwrite": false,
	}, &restoreOut)
	if err != nil {
		// If collision, try with overwrite
		err = f.app.Call(ctx, "fs/restore", map[string]any{
			"id":        f.selectedTrashID,
			"overwrite": true,
		}, &restoreOut)
	}

	f.loadTrashLocked(ctx)
	if err != nil {
		f.lastError = fmt.Sprintf("Restore failed: %v", err)
	} else {
		f.lastStatus = fmt.Sprintf("Restored %s", restoreOut.RestoredPath)
	}
}

func (f *Files) handleEmptyTrashLocked(ctx context.Context) {
	var out struct {
		Count int `json:"count"`
	}
	err := f.app.Call(ctx, "fs/trash/empty", nil, &out)
	f.loadTrashLocked(ctx)
	if err != nil {
		f.lastError = fmt.Sprintf("Empty trash failed: %v", err)
	} else {
		f.lastStatus = fmt.Sprintf("Trash emptied (%d items permanently removed)", out.Count)
	}
}

// Programmatic IPC handlers

func (f *Files) browseRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	targetPath := p.Path
	if targetPath == "" {
		targetPath = DefaultBrowsePath
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.loadDirLocked(ctx, targetPath); err != nil {
		return nil, err
	}
	return map[string]any{
		"cwd":      f.cwd,
		"entries":  f.entries,
		"total":    len(f.entries),
		"selected": f.selectedName,
	}, nil
}

func (f *Files) statRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}

	var statOut any
	if err := f.app.Call(ctx, "fs/stat", map[string]string{"path": p.Path}, &statOut); err != nil {
		return nil, err
	}
	return statOut, nil
}

func (f *Files) previewRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	limit := p.Limit
	if limit <= 0 || limit > 4096 {
		limit = MaxPreviewReadBytes
	}

	var readOut struct {
		DataBase64 string `json:"data_base64"`
		TotalSize  int64  `json:"total_size"`
	}
	if err := f.app.Call(ctx, "fs/read", map[string]any{
		"path":   p.Path,
		"offset": 0,
		"limit":  limit,
	}, &readOut); err != nil {
		return nil, err
	}

	data, err := base64.StdEncoding.DecodeString(readOut.DataBase64)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"path":       p.Path,
		"total_size": readOut.TotalSize,
		"is_text":    isText(data),
		"preview":    sanitizeString(string(data)),
	}, nil
}

func (f *Files) openRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Path  string `json:"path"`
		AppID string `json:"app_id"`
		Mode  string `json:"mode"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}

	mode := p.Mode
	if mode == "" {
		mode = "read-write"
	}

	var res sdk.HandoffResult
	err := f.app.Call(ctx, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    p.Path,
		AppID:   p.AppID,
		Mode:    mode,
	}, &res)
	if err == nil {
		return map[string]any{
			"path":     res.Path,
			"app_id":   res.AppID,
			"grant_id": res.GrantID,
			"opened":   res.Success,
		}, nil
	}

	appID := p.AppID
	if appID == "" {
		appID = NotesAppID
	}

	_ = f.app.Call(ctx, "app/launch", map[string]string{"id": appID}, nil)
	var out any
	if err := f.app.Call(ctx, "app/"+appID+"/open", map[string]any{"path": p.Path, "force": false}, &out); err != nil {
		return nil, err
	}
	return map[string]any{
		"path":   p.Path,
		"app_id": appID,
		"opened": true,
	}, nil
}

func (f *Files) trashListRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var out struct {
		Entries []trashItem `json:"entries"`
	}
	if err := f.app.Call(ctx, "fs/trash/list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Utility functions

func truncateString(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func sanitizeString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || r == ' ' || unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}

func formatSize(b int64) string {
	if b < 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
