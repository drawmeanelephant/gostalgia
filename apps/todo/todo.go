// Package todo is Gostalgia's persistent task list: add, complete, delete,
// and clear tasks that survive relaunch and reboot. It exists to prove
// app-private persistence — the list lives in
// /apps/data/com.gostalgia.todo/tasks.json, written atomically through
// fs/save after every mutation, and the surface is the version-1
// presentation contract: tasks as items, one new-task field, and labeled
// actions. The grant never reaches user documents; the app's own
// partition is the only path it touches.
package todo

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Todo.
const ID = "com.gostalgia.todo"

// statePath is the single persisted document inside the app-private
// partition. fs/save stages and renames atomically, so a crash mid-write
// leaves the previous list intact.
const statePath = "/apps/data/com.gostalgia.todo/tasks.json"

const stateVersion = 1

// Task bounds. Titles are single-line and short; the stored list is
// bounded so the save file stays small and snapshots stay fast.
const (
	maxTaskTitle  = 120
	maxTasksTotal = 250
)

// maxListItems bounds the rows a snapshot carries. The presentation
// contract tolerates more, but a task list is only useful while it stays
// scannable; past the cap the last row is an honest "… and N more"
// indicator rather than silently dropped tasks.
const maxListItems = 64

// Filter subsets for the snapshot and the list route.
const (
	filterAll    = "all"
	filterActive = "active"
	filterDone   = "done"
)

// Reserved item IDs that are notices, not tasks. sanitizeLocked reassigns
// any loaded task that collides with one.
const (
	itemEmpty = "empty"
	itemMore  = "more"
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
func ManifestJSON() []byte { return append([]byte(nil), manifestJSON...) }

// Task is one to-do item.
type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
}

// diskState is the persisted document: the task list plus the id counter
// that keeps future ids unique.
type diskState struct {
	Version int    `json:"version"`
	Tasks   []Task `json:"tasks"`
	Seq     int    `json:"seq"`
}

// Todo is the running in-process instance. All state is guarded by mu.
type Todo struct {
	app *sdk.Context
	now func() time.Time

	mu         sync.Mutex
	loaded     bool
	tasks      []Task
	seq        int
	filter     string // all | active | done; view state, not persisted
	dirty      bool   // in-memory change has not reached disk
	statusNote string // transient feedback line; cleared on each action
	lastError  string
}

// Factory constructs a fresh, uninitialized Todo instance.
func Factory() (sdk.Instance, error) {
	return &Todo{now: time.Now, filter: filterAll}, nil
}

// setClock injects a deterministic clock for tests.
func setClock(t *Todo, now func() time.Time) { t.now = now }

// Init registers routes and the presentation callbacks. State loads
// lazily on first use so Init stays prompt.
func (t *Todo) Init(app *sdk.Context) error {
	t.app = app
	if t.now == nil {
		t.now = time.Now
	}
	if t.filter == "" {
		t.filter = filterAll
	}
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"list", t.listRoute},
		{"add", t.addRoute},
		{"complete", t.completeRoute},
		{"delete", t.deleteRoute},
		{"clear_done", t.clearDoneRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(t.view, t.act)
}

// Run blocks until the instance is canceled. There is no live tick:
// tasks change only when someone changes them.
func (t *Todo) Run(ctx context.Context) error {
	t.app.Log.Info("todo running")
	<-ctx.Done()
	return nil
}

// Stop flushes a pending save — the retry after an earlier save failure.
// A failed flush is logged, never fatal.
func (t *Todo) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded && t.dirty {
		if err := t.persistLocked(ctx); err != nil && t.app.Log != nil {
			t.app.Log.Warn("todo: final save failed", "err", err)
		}
	}
	return nil
}

// ensureLoadedLocked reads and decodes tasks.json once. A missing file is
// simply an empty list. A corrupt file is an honest error banner over an
// empty list; the bad bytes are left alone until the next real change
// saves over them. A denied read is also reported, since nothing will
// persist without the grant. Any other read failure is NOT "no save yet":
// the app reports an honest error and stays unloaded so the next call
// retries — an empty list must never overwrite a save it could not read.
func (t *Todo) ensureLoadedLocked(ctx context.Context) error {
	if t.loaded {
		return nil
	}

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := t.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// no save yet: empty list
		case errors.Is(err, fs.ErrPermission):
			t.lastError = fmt.Sprintf("storage access denied (%v) — tasks will not be saved", err)
		default:
			return fmt.Errorf("saved tasks could not be loaded: %w", err)
		}
		t.loaded = true
		return nil
	}
	t.loaded = true
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		t.lastError = "saved task list is unreadable — starting empty; the next change saves over it"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		t.lastError = "saved task list is unreadable — starting empty; the next change saves over it"
		return nil
	}
	t.tasks = st.Tasks
	t.seq = st.Seq
	if t.sanitizeLocked() {
		// The loaded file needed repair; write the healed copy back now so
		// a crash does not reload the same damage. Best-effort: the in-memory
		// list is already sound, so a failure only shows up on the banner.
		if err := t.persistLocked(ctx); err != nil {
			t.lastError = fmt.Sprintf("save failed: %v", err)
		}
	}
	return nil
}

// sanitizeLocked repairs loaded state in place and reports whether it
// changed anything: seq pushed past every task_N id, duplicate or
// reserved ids reassigned, titles normalized, timestamps pinned inside
// [created, now], and the list bounded to maxTasksTotal.
func (t *Todo) sanitizeLocked() bool {
	changed := false
	for _, task := range t.tasks {
		var n int
		if _, err := fmt.Sscanf(task.ID, "task_%d", &n); err == nil && n > t.seq {
			t.seq = n
		}
	}
	if len(t.tasks) > maxTasksTotal {
		t.tasks = t.tasks[:maxTasksTotal]
		changed = true
	}
	now := t.now()
	seen := map[string]bool{itemEmpty: true, itemMore: true}
	kept := t.tasks[:0]
	for _, task := range t.tasks {
		title := sanitizeTitle(task.Title)
		if title == "" {
			title = "(untitled task)"
		}
		if title != task.Title {
			changed = true
		}
		task.Title = title
		if task.ID == "" || seen[task.ID] {
			t.seq++
			task.ID = fmt.Sprintf("task_%d", t.seq)
			changed = true
		}
		seen[task.ID] = true
		if task.CreatedAt.IsZero() || task.CreatedAt.After(now) {
			task.CreatedAt = now
			changed = true
		}
		if task.Done {
			if task.DoneAt.IsZero() || task.DoneAt.After(now) || task.DoneAt.Before(task.CreatedAt) {
				task.DoneAt = task.CreatedAt
				changed = true
			}
		} else if !task.DoneAt.IsZero() {
			task.DoneAt = time.Time{}
			changed = true
		}
		kept = append(kept, task)
	}
	t.tasks = kept
	return changed
}

// persistLocked writes the whole task list atomically. Callers hold mu.
func (t *Todo) persistLocked(ctx context.Context) error {
	tasks := t.tasks
	if tasks == nil {
		tasks = []Task{}
	}
	data, err := json.Marshal(diskState{
		Version: stateVersion,
		Tasks:   tasks,
		Seq:     t.seq,
	})
	if err != nil {
		return err
	}
	err = t.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
	if err == nil {
		t.dirty = false
	}
	return err
}

// saveErrorLocked persists and wraps a failure so the report says what
// failed, not just why the fs call did. Callers hold mu.
func (t *Todo) saveErrorLocked(ctx context.Context) error {
	if err := t.persistLocked(ctx); err != nil {
		return fmt.Errorf("save failed: %w", err)
	}
	return nil
}

// --- mutations: error-returning, shared by presentation and routes ---

func (t *Todo) addTaskLocked(ctx context.Context, title string) error {
	title = sanitizeTitle(title)
	if title == "" {
		return fmt.Errorf("give the task a name first")
	}
	if len(t.tasks) >= maxTasksTotal {
		return fmt.Errorf("the list is full (%d tasks) - clear done tasks first", maxTasksTotal)
	}
	t.seq++
	t.tasks = append(t.tasks, Task{
		ID:        fmt.Sprintf("task_%d", t.seq),
		Title:     title,
		CreatedAt: t.now(),
	})
	t.statusNote = fmt.Sprintf("added %q", title)
	t.dirty = true
	return t.saveErrorLocked(ctx)
}

func (t *Todo) completeTaskLocked(ctx context.Context, itemID string) error {
	if itemID == "" {
		return fmt.Errorf("select a task to complete")
	}
	for i := range t.tasks {
		if t.tasks[i].ID == itemID {
			if t.tasks[i].Done {
				return fmt.Errorf("%q is already done", t.tasks[i].Title)
			}
			t.tasks[i].Done = true
			t.tasks[i].DoneAt = t.now()
			t.statusNote = fmt.Sprintf("completed %q", t.tasks[i].Title)
			t.dirty = true
			return t.saveErrorLocked(ctx)
		}
	}
	return fmt.Errorf("no task %q on this list", itemID)
}

func (t *Todo) deleteTaskLocked(ctx context.Context, itemID string) error {
	if itemID == "" {
		return fmt.Errorf("select a task to delete")
	}
	for i := range t.tasks {
		if t.tasks[i].ID == itemID {
			title := t.tasks[i].Title
			t.tasks = append(t.tasks[:i], t.tasks[i+1:]...)
			t.statusNote = fmt.Sprintf("deleted %q", title)
			t.dirty = true
			return t.saveErrorLocked(ctx)
		}
	}
	return fmt.Errorf("no task %q on this list", itemID)
}

// clearDoneLocked drops every completed task and reports how many went.
func (t *Todo) clearDoneLocked(ctx context.Context) (int, error) {
	cleared := 0
	kept := t.tasks[:0]
	for _, task := range t.tasks {
		if task.Done {
			cleared++
			continue
		}
		kept = append(kept, task)
	}
	if cleared == 0 {
		return 0, fmt.Errorf("no completed tasks to clear")
	}
	t.tasks = kept
	t.statusNote = fmt.Sprintf("cleared %d completed task%s", cleared, plural(cleared))
	t.dirty = true
	return cleared, t.saveErrorLocked(ctx)
}

// cycleFilterLocked rotates all -> active -> done -> all. The filter is
// view state, not domain state, so it is deliberately not persisted.
func (t *Todo) cycleFilterLocked() {
	switch t.filter {
	case filterAll:
		t.filter = filterActive
	case filterActive:
		t.filter = filterDone
	default:
		t.filter = filterAll
	}
	t.statusNote = fmt.Sprintf("showing %s tasks", t.filter)
}

// --- presentation ---

func (t *Todo) view(ctx context.Context) (sdk.View, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Todo", State: sdk.ViewError, Error: err.Error()}, nil
	}
	return t.viewLocked(), nil
}

func (t *Todo) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Todo", State: sdk.ViewError, Error: err.Error()}, nil
	}
	t.lastError = ""
	t.statusNote = ""

	var err error
	switch req.Action {
	case "add_task":
		err = t.addTaskLocked(ctx, req.Values["new_task"])
	case "complete":
		err = t.completeTaskLocked(ctx, req.ItemID)
	case "delete":
		err = t.deleteTaskLocked(ctx, req.ItemID)
	case "clear_done":
		_, err = t.clearDoneLocked(ctx)
	case "filter":
		t.cycleFilterLocked()
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		// Domain rejections land in the status line, not the error banner:
		// the list is still fully usable.
		t.statusNote = err.Error()
	}
	return t.viewLocked(), nil
}

// visibleLocked applies the current filter to the task list.
func (t *Todo) visibleLocked() []Task {
	visible := make([]Task, 0, len(t.tasks))
	for _, task := range t.tasks {
		switch t.filter {
		case filterActive:
			if task.Done {
				continue
			}
		case filterDone:
			if !task.Done {
				continue
			}
		}
		visible = append(visible, task)
	}
	return visible
}

func (t *Todo) viewLocked() sdk.View {
	now := t.now()
	pending, done := 0, 0
	for _, task := range t.tasks {
		if task.Done {
			done++
		} else {
			pending++
		}
	}

	visible := t.visibleLocked()
	shown := visible
	truncated := 0
	if len(visible) > maxListItems {
		shown = visible[:maxListItems-1]
		truncated = len(visible) - len(shown)
	}

	items := make([]sdk.Item, 0, len(shown)+1)
	for _, task := range shown {
		items = append(items, taskItem(task, now))
	}
	if truncated > 0 {
		items = append(items, sdk.Item{
			ID:     itemMore,
			Label:  fmt.Sprintf("… and %d more", truncated),
			Detail: "the list is capped — clear done tasks or narrow the filter to see the rest",
		})
	}
	if len(items) == 0 {
		items = append(items, t.emptyItemLocked())
	}

	return sdk.View{
		Title:  "Todo",
		State:  sdk.ViewReady,
		Items:  items,
		Fields: []sdk.Field{{ID: "new_task", Label: "New task"}},
		Actions: []sdk.Action{
			{ID: "add_task", Label: "Add task", Disabled: len(t.tasks) >= maxTasksTotal},
			{ID: "complete", Label: "Complete", Disabled: len(shown) == 0 || t.filter == filterDone},
			{ID: "delete", Label: "Delete", Disabled: len(shown) == 0},
			{ID: "clear_done", Label: "Clear done", Disabled: done == 0},
			{ID: "filter", Label: "Filter: " + t.filter},
		},
		Status: t.statusLocked(pending, done),
		Error:  t.lastError,
	}
}

// taskItem renders one row: the checkbox lives in the label, the timing
// in the detail, so done state reads at a glance.
func taskItem(task Task, now time.Time) sdk.Item {
	if task.Done {
		return sdk.Item{
			ID:     task.ID,
			Label:  "[x] " + task.Title,
			Detail: fmt.Sprintf("done %s · added %s", ago(now.Sub(task.DoneAt)), ago(now.Sub(task.CreatedAt))),
		}
	}
	return sdk.Item{
		ID:     task.ID,
		Label:  "[ ] " + task.Title,
		Detail: "added " + ago(now.Sub(task.CreatedAt)),
	}
}

// emptyItemLocked is the friendly notice shown instead of a blank area.
func (t *Todo) emptyItemLocked() sdk.Item {
	if len(t.tasks) == 0 {
		return sdk.Item{
			ID:     itemEmpty,
			Label:  "No tasks yet",
			Detail: "type in the field below and press Add task",
		}
	}
	return sdk.Item{
		ID:     itemEmpty,
		Label:  "Nothing to show",
		Detail: fmt.Sprintf("no %s tasks — press Filter to look at another slice", t.filter),
	}
}

// statusLocked picks the line under the list: transient notes win, then
// the honest summary of what the filter is showing.
func (t *Todo) statusLocked(pending, done int) string {
	if t.statusNote != "" {
		return t.statusNote
	}
	total := pending + done
	if total == 0 {
		return "no tasks yet — add the first one below"
	}
	if t.filter == filterAll {
		return fmt.Sprintf("%d pending · %d done", pending, done)
	}
	return fmt.Sprintf("%d pending · %d done · showing %s", pending, done, t.filter)
}

// ago renders a duration compactly: "just now", "34m ago", "6h ago", "2d ago".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sanitizeTitle normalizes user input: trimmed, single-line, bounded.
func sanitizeTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if len(s) > maxTaskTitle {
		s = strings.TrimSpace(s[:maxTaskTitle])
	}
	return s
}

// --- programmatic IPC routes ---

func (t *Todo) listRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Filter string `json:"filter"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	switch params.Filter {
	case "", filterAll, filterActive, filterDone:
	default:
		return nil, fmt.Errorf("unknown filter %q", params.Filter)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	pending, done := 0, 0
	tasks := make([]Task, 0, len(t.tasks))
	for _, task := range t.tasks {
		if task.Done {
			done++
		} else {
			pending++
		}
		switch params.Filter {
		case filterActive:
			if task.Done {
				continue
			}
		case filterDone:
			if !task.Done {
				continue
			}
		}
		tasks = append(tasks, task)
	}
	return map[string]any{
		"tasks":   tasks,
		"total":   len(t.tasks),
		"pending": pending,
		"done":    done,
	}, nil
}

func (t *Todo) addRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Title string `json:"title"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := t.addTaskLocked(ctx, params.Title); err != nil {
		return nil, err
	}
	last := t.tasks[len(t.tasks)-1]
	return map[string]any{"id": last.ID, "title": last.Title, "added": true}, nil
}

func (t *Todo) completeRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := t.completeTaskLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "done": true}, nil
}

func (t *Todo) deleteRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := t.deleteTaskLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "deleted": true}, nil
}

func (t *Todo) clearDoneRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	cleared, err := t.clearDoneLocked(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"cleared": cleared}, nil
}
