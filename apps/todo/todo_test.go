package todo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gostalgia/sdk"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// harness models the app-private VFS partition with an injectable clock.
// deny simulates a missing fs grant. No test sleeps: time moves via h.now.
type harness struct {
	now     time.Time
	deny    bool  // fs/read+fs/save denied: simulates missing grants
	readErr error // if set, the next fs/read fails once with this error
	seq     atomic.Int64

	fmu   sync.Mutex // guards files; concurrent handlers share the map
	files map[string][]byte

	app      *Todo
	handlers map[string]sdk.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		now:      t0,
		files:    map[string][]byte{},
		handlers: map[string]sdk.Handler{},
	}
	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	td := inst.(*Todo)
	setClock(td, func() time.Time { return h.now })
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := td.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = td
	return h
}

func (h *harness) call(_ context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p struct {
		Path string `json:"path"`
		Data string `json:"data_base64"`
	}
	_ = json.Unmarshal(raw, &p)
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	h.fmu.Lock()
	defer h.fmu.Unlock()
	if h.deny {
		return &sdk.Error{Code: sdk.CodePermissionDenied, Message: "permission denied: caller not authorized"}
	}
	switch method {
	case "fs/read":
		if h.readErr != nil {
			err := h.readErr
			h.readErr = nil
			return err
		}
		data, ok := h.files[p.Path]
		if !ok {
			return &sdk.Error{Code: sdk.CodeNotFound, Message: fmt.Sprintf("not_found: %s", p.Path)}
		}
		return reply(map[string]any{"path": p.Path, "data_base64": base64.StdEncoding.EncodeToString(data)})
	case "fs/save":
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return err
		}
		h.files[p.Path] = data
		return reply(map[string]any{"path": p.Path, "saved": true})
	}
	return fmt.Errorf("unknown method %s", method)
}

func (h *harness) savedFile(t *testing.T) []byte {
	t.Helper()
	h.fmu.Lock()
	defer h.fmu.Unlock()
	data, ok := h.files[statePath]
	if !ok {
		t.Fatalf("no saved state at %s", statePath)
	}
	return append([]byte(nil), data...)
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) view(t *testing.T, version int) sdk.View {
	t.Helper()
	res, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		t.Fatalf("view v%d: %v", version, err)
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		t.Fatalf("view v%d does not validate: %v", version, err)
	}
	return v
}

func (h *harness) act(t *testing.T, version int, action, item string, values map[string]string) sdk.View {
	t.Helper()
	v, err := h.tryAct(version, action, item, values)
	if err != nil {
		t.Fatalf("action %s: %v", action, err)
	}
	return v
}

func (h *harness) tryAct(version int, action, item string, values map[string]string) (sdk.View, error) {
	cur, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		return sdk.View{}, err
	}
	req := sdk.ActionRequest{
		Version:   version,
		Instance:  cur.(sdk.View).Instance,
		RequestID: fmt.Sprintf("req_%d", h.seq.Add(1)),
		Action:    action,
		ItemID:    item,
		Values:    values,
	}
	raw, _ := json.Marshal(req)
	res, err := h.handlers["action"](context.Background(), raw)
	if err != nil {
		return sdk.View{}, err
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		return sdk.View{}, fmt.Errorf("action view does not validate: %w", err)
	}
	return v, nil
}

func (h *harness) route(t *testing.T, name string, params, out any) {
	t.Helper()
	res, err := h.tryRoute(name, params)
	if err != nil {
		t.Fatalf("route %s: %v", name, err)
	}
	if out != nil {
		b, _ := json.Marshal(res)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("route %s decode: %v", name, err)
		}
	}
}

func (h *harness) tryRoute(name string, params any) (any, error) {
	hd, ok := h.handlers[name]
	if !ok {
		return nil, fmt.Errorf("route %q not registered", name)
	}
	raw, _ := json.Marshal(params)
	return hd(context.Background(), raw)
}

func findAction(v sdk.View, id string) *sdk.Action {
	for i := range v.Actions {
		if v.Actions[i].ID == id {
			return &v.Actions[i]
		}
	}
	return nil
}

func findItem(v sdk.View, id string) *sdk.Item {
	for i := range v.Items {
		if v.Items[i].ID == id {
			return &v.Items[i]
		}
	}
	return nil
}

// --- manifest ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "todo" {
		t.Errorf("manifest = %s/%s", m.ID, m.Entrypoint)
	}
	want := map[string]bool{"ipc": true, "fs.read": true, "fs.write": true}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions = %v", m.Permissions)
	}
	for _, c := range m.Permissions {
		if !want[c] {
			t.Errorf("unexpected permission %q", c)
		}
	}
	if !json.Valid(ManifestJSON()) {
		t.Error("ManifestJSON is not valid JSON")
	}
}

// --- presentation behavior ---

func TestEmptyStateIsFriendlyNotice(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 1)
	if v.State != sdk.ViewReady {
		t.Fatalf("empty view state = %q", v.State)
	}
	if len(v.Items) != 1 || v.Items[0].ID != itemEmpty {
		t.Fatalf("empty view items = %+v", v.Items)
	}
	if !strings.Contains(v.Status, "no tasks yet") {
		t.Errorf("empty status = %q, want a friendly notice", v.Status)
	}
	if a := findAction(v, "add_task"); a == nil || a.Disabled {
		t.Error("add_task should be available on the empty state")
	}
	for _, id := range []string{"complete", "delete", "clear_done"} {
		if a := findAction(v, id); a == nil || !a.Disabled {
			t.Errorf("action %s should be disabled with nothing to act on", id)
		}
	}
	// Version-2 requests get the same v1 surface: nothing is hidden behind
	// the newer contract.
	if v2 := h.view(t, 2); len(v2.Blocks) != 0 || len(v2.Meters) != 0 || v2.Grid != nil {
		t.Error("v2 view should carry no version-2 elements")
	}
}

func TestAddCompleteDeleteClearDoneFlow(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 1, "add_task", "", map[string]string{"new_task": "buy milk"})
	item := findItem(v, "task_1")
	if item == nil || item.Label != "[ ] buy milk" {
		t.Fatalf("added item = %+v", item)
	}
	if !strings.Contains(item.Detail, "added just now") {
		t.Errorf("pending detail = %q", item.Detail)
	}
	if !strings.Contains(v.Status, "added") {
		t.Errorf("add status = %q", v.Status)
	}

	h.advance(2 * time.Hour)
	v = h.act(t, 1, "complete", "task_1", nil)
	item = findItem(v, "task_1")
	if item == nil || item.Label != "[x] buy milk" {
		t.Fatalf("completed item = %+v", item)
	}
	if !strings.Contains(item.Detail, "done just now") || !strings.Contains(item.Detail, "added 2h ago") {
		t.Errorf("done detail = %q, want done/added timing", item.Detail)
	}
	if a := findAction(v, "clear_done"); a.Disabled {
		t.Error("clear_done should enable once a task is done")
	}

	v = h.act(t, 1, "add_task", "", map[string]string{"new_task": "walk dog"})
	v = h.act(t, 1, "clear_done", "", nil)
	if findItem(v, "task_1") != nil {
		t.Error("clear_done should remove completed tasks")
	}
	if findItem(v, "task_2") == nil {
		t.Error("clear_done should keep pending tasks")
	}
	if !strings.Contains(v.Status, "cleared 1 completed task") {
		t.Errorf("clear status = %q", v.Status)
	}

	v = h.act(t, 1, "delete", "task_2", nil)
	if len(v.Items) != 1 || v.Items[0].ID != itemEmpty {
		t.Fatalf("after deleting the last task items = %+v, want empty notice", v.Items)
	}
}

func TestDomainRejectionsLandInStatusLine(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 1, "add_task", "", map[string]string{"new_task": "   "})
	if !strings.Contains(v.Status, "name") {
		t.Errorf("empty add status = %q", v.Status)
	}
	if v.State != sdk.ViewReady || v.Error != "" {
		t.Error("a domain rejection should not become an error view")
	}

	h.act(t, 1, "add_task", "", map[string]string{"new_task": "real"})
	v = h.act(t, 1, "complete", "task_1", nil)
	v = h.act(t, 1, "complete", "task_1", nil)
	if !strings.Contains(v.Status, "already done") {
		t.Errorf("re-complete status = %q", v.Status)
	}
	// Whitespace and overlong titles are normalized, not trusted.
	v = h.act(t, 1, "add_task", "", map[string]string{"new_task": "  two\nlines\t " + strings.Repeat("x", 200)})
	item := findItem(v, "task_2")
	if item == nil || len(item.Label) > maxTaskTitle+4 || strings.ContainsAny(item.Label, "\n\t") {
		t.Errorf("sanitized item = %+v", item)
	}
}

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(1, "launch_missiles", "", nil); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

func TestFilterCyclesSlices(t *testing.T) {
	h := newHarness(t)
	h.act(t, 1, "add_task", "", map[string]string{"new_task": "one"})
	h.act(t, 1, "add_task", "", map[string]string{"new_task": "two"})
	h.act(t, 1, "complete", "task_1", nil)

	v := h.act(t, 1, "filter", "", nil)
	if a := findAction(v, "filter"); a.Label != "Filter: "+filterActive {
		t.Fatalf("filter label = %q", a.Label)
	}
	if findItem(v, "task_1") != nil || findItem(v, "task_2") == nil {
		t.Error("active filter should hide done tasks")
	}
	if !strings.Contains(v.Status, "showing "+filterActive) {
		t.Errorf("filter status = %q", v.Status)
	}

	v = h.act(t, 1, "filter", "", nil)
	if findItem(v, "task_1") == nil || findItem(v, "task_2") != nil {
		t.Error("done filter should show only completed tasks")
	}
	if a := findAction(v, "complete"); !a.Disabled {
		t.Error("complete should be disabled when only done tasks are visible")
	}

	v = h.act(t, 1, "filter", "", nil)
	if findItem(v, "task_1") == nil || findItem(v, "task_2") == nil {
		t.Error("all filter should show every task again")
	}

	// A filter that matches nothing renders an honest placeholder, not a
	// blank area.
	h.act(t, 1, "delete", "task_1", nil)
	v = h.act(t, 1, "filter", "", nil) // -> active... two steps to done
	v = h.act(t, 1, "filter", "", nil) // -> done
	if len(v.Items) != 1 || v.Items[0].ID != itemEmpty {
		t.Fatalf("empty filtered view items = %+v", v.Items)
	}
}

func TestTruncationIndicatorIsHonest(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxListItems+6; i++ {
		h.route(t, "add", map[string]string{"title": fmt.Sprintf("task %02d", i)}, nil)
	}
	v := h.view(t, 1)
	if len(v.Items) != maxListItems {
		t.Fatalf("items = %d, want capped at %d", len(v.Items), maxListItems)
	}
	last := v.Items[len(v.Items)-1]
	if last.ID != itemMore || !strings.Contains(last.Label, "7 more") {
		t.Fatalf("truncation indicator = %+v", last)
	}
	// The indicator is a notice: picking it is an honest domain error, and
	// the IPC list route still sees the whole list.
	if _, err := h.tryRoute("complete", map[string]string{"id": itemMore}); err == nil {
		t.Error("completing the notice should be a domain error")
	}
	var out struct {
		Total int `json:"total"`
	}
	h.route(t, "list", nil, &out)
	if out.Total != maxListItems+6 {
		t.Errorf("list total = %d, want %d", out.Total, maxListItems+6)
	}
}

func TestListFullRejectsAdds(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxTasksTotal; i++ {
		h.route(t, "add", map[string]string{"title": fmt.Sprintf("t%d", i)}, nil)
	}
	if _, err := h.tryRoute("add", map[string]string{"title": "overflow"}); err == nil {
		t.Error("add past the cap should be rejected")
	}
	v := h.view(t, 1)
	if a := findAction(v, "add_task"); !a.Disabled {
		t.Error("add_task should be disabled when the list is full")
	}
	if len(v.Items) != maxListItems {
		t.Errorf("items = %d, want cap %d", len(v.Items), maxListItems)
	}
}

// --- persistence ---

func TestPersistenceRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.act(t, 1, "add_task", "", map[string]string{"new_task": "first"})
	h.act(t, 1, "add_task", "", map[string]string{"new_task": "second"})
	h.act(t, 1, "complete", "task_1", nil)
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// New instance, same partition, three hours later.
	h2 := newHarness(t)
	h2.files = h.files
	h2.now = t0.Add(3 * time.Hour)
	v := h2.view(t, 1)
	done := findItem(v, "task_1")
	pending := findItem(v, "task_2")
	if done == nil || pending == nil {
		t.Fatalf("tasks did not round-trip: %+v", v.Items)
	}
	if done.Label != "[x] first" || !strings.Contains(done.Detail, "added 3h ago") {
		t.Errorf("restored done item = %+v", done)
	}
	if pending.Label != "[ ] second" {
		t.Errorf("restored pending item = %+v", pending)
	}
	// The id counter survived too: no collision with restored tasks.
	v = h2.act(t, 1, "add_task", "", map[string]string{"new_task": "third"})
	if findItem(v, "task_3") == nil {
		t.Errorf("next id after reload should be task_3: %+v", v.Items)
	}
}

// A transient fs/read failure (a canceled call racing the first view, a
// host I/O error, the read-size bound) is not "no save yet": the app must
// say so honestly, retry the load, and never let the next change overwrite
// the saved list.
func TestTransientReadErrorNeverOverwritesSavedTasks(t *testing.T) {
	h := newHarness(t)
	data, _ := json.Marshal(diskState{
		Version: stateVersion,
		Seq:     3,
		Tasks: []Task{
			{ID: "task_1", Title: "renew passport", CreatedAt: t0},
			{ID: "task_2", Title: "file taxes", CreatedAt: t0},
			{ID: "task_3", Title: "call the vet", CreatedAt: t0},
		},
	})
	h.files[statePath] = data
	h.readErr = context.Canceled // first fs/read fails once, transiently

	res, err := h.handlers["view"](context.Background(), json.RawMessage(`{"version":1}`))
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	v := res.(sdk.View)
	if v.State != sdk.ViewError || v.Error == "" {
		t.Fatalf("transient read failure view = %+v, want an honest error", v)
	}

	// The retry loads the real file; an add then extends it, not replaces it.
	v = h.act(t, 1, "add_task", "", map[string]string{"new_task": "buy milk"})
	if findItem(v, "task_4") == nil {
		t.Fatalf("add after transient failure = %+v", v.Items)
	}
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Tasks) != 4 {
		t.Fatalf("%d task(s) on disk after one transient read error and one add, want 4: %+v",
			len(st.Tasks), st.Tasks)
	}
}

func TestCorruptStateIsHonestError(t *testing.T) {
	for _, bad := range [][]byte{
		[]byte("{not json"),
		[]byte(`{"version":99,"tasks":[]}`),
		[]byte(""),
	} {
		h := newHarness(t)
		h.files[statePath] = bad
		v := h.view(t, 1)
		if v.State != sdk.ViewReady {
			t.Fatalf("corrupt state (%q) should not take down the view: %q", bad, v.State)
		}
		if !strings.Contains(v.Error, "unreadable") {
			t.Errorf("corrupt state (%q) error = %q, want an honest banner", bad, v.Error)
		}
		if len(v.Items) != 1 || v.Items[0].ID != itemEmpty {
			t.Errorf("corrupt state (%q) should fall back to the empty notice", bad)
		}
		// The app stays usable, and the first real change heals the file.
		v = h.act(t, 1, "add_task", "", map[string]string{"new_task": "fresh start"})
		if findItem(v, "task_1") == nil || v.Error != "" {
			t.Errorf("post-corrupt add should work and clear the banner: %+v", v)
		}
		var st diskState
		if err := json.Unmarshal(h.savedFile(t), &st); err != nil || len(st.Tasks) != 1 {
			t.Errorf("healed save = %v/%+v", err, st)
		}
	}
}

func TestCorruptFileSurvivesUntouchedShutdown(t *testing.T) {
	h := newHarness(t)
	bad := []byte("{not json")
	h.files[statePath] = bad
	h.view(t, 1)
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fmu.Lock()
	got := h.files[statePath]
	h.fmu.Unlock()
	if string(got) != string(bad) {
		t.Error("an untouched corrupt file should not be silently overwritten")
	}
}

func TestSanitizeRepairsLoadedState(t *testing.T) {
	h := newHarness(t)
	future := t0.Add(48 * time.Hour)
	st := diskState{
		Version: stateVersion,
		Seq:     3,
		Tasks: []Task{
			{ID: "task_9", Title: "kept", CreatedAt: t0},
			{ID: "task_9", Title: "dupe", CreatedAt: t0},                       // duplicate id
			{ID: itemMore, Title: "reserved id", Done: true, CreatedAt: t0},    // reserved id, no done_at
			{ID: "odd", Title: "", CreatedAt: t0},                              // untitled
			{ID: "task_1", Title: "future", CreatedAt: future, DoneAt: future}, // clock skew, and not done
		},
	}
	data, _ := json.Marshal(st)
	h.files[statePath] = data

	v := h.view(t, 1)
	if v.State != sdk.ViewReady || v.Error != "" {
		t.Fatalf("repairable state should load clean: %q", v.Error)
	}
	seen := map[string]bool{}
	for _, task := range h.app.tasks {
		if seen[task.ID] || task.ID == itemEmpty || task.ID == itemMore {
			t.Fatalf("unrepaired task id %q", task.ID)
		}
		seen[task.ID] = true
		if strings.TrimSpace(task.Title) == "" {
			t.Error("empty title should have been repaired")
		}
		if task.CreatedAt.After(t0) {
			t.Error("future created_at should clamp to now")
		}
		if task.Done && task.DoneAt.IsZero() {
			t.Error("done task should gain a done_at")
		}
		if !task.Done && !task.DoneAt.IsZero() {
			t.Error("pending task should not carry done_at")
		}
	}
	if h.app.seq < 9 {
		t.Errorf("seq = %d, want pushed past task_9", h.app.seq)
	}
	// The repair was written back: the file now parses to sound state.
	var healed diskState
	if err := json.Unmarshal(h.savedFile(t), &healed); err != nil {
		t.Fatalf("healed save should parse: %v", err)
	}
}

func TestDeniedStorageKeepsWorking(t *testing.T) {
	h := newHarness(t)
	h.deny = true
	v := h.view(t, 1)
	if v.State != sdk.ViewReady {
		t.Fatalf("denied storage should not be a crash: %q", v.State)
	}
	if !strings.Contains(v.Error, "denied") {
		t.Errorf("denied read should be an honest banner: %q", v.Error)
	}
	v = h.act(t, 1, "add_task", "", map[string]string{"new_task": "volatile"})
	if findItem(v, "task_1") == nil {
		t.Error("tasks should still work in memory while storage is denied")
	}
	if !strings.Contains(v.Status, "save failed") {
		t.Errorf("failed save should be an honest status: %q", v.Status)
	}
	h.fmu.Lock()
	_, wrote := h.files[statePath]
	h.fmu.Unlock()
	if wrote {
		t.Error("denied save should not have produced a file")
	}

	// The grant arrives later: the next change persists everything.
	h.deny = false
	h.act(t, 1, "add_task", "", map[string]string{"new_task": "durable"})
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil || len(st.Tasks) != 2 {
		t.Fatalf("after grant, save should hold both tasks: %v/%+v", err, st)
	}
}

func TestStopFlushesPendingSave(t *testing.T) {
	h := newHarness(t)
	h.deny = true
	h.act(t, 1, "add_task", "", map[string]string{"new_task": "buffered"})
	h.deny = false
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil || len(st.Tasks) != 1 {
		t.Fatalf("stop should flush the pending save: %v/%+v", err, st)
	}
}

// --- routes ---

func TestRoutesValidateParams(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryRoute("add", map[string]string{"title": ""}); err == nil {
		t.Error("add with empty title should error")
	}
	if _, err := h.tryRoute("add", map[string]string{}); err == nil {
		t.Error("add without title should error")
	}
	if _, err := h.tryRoute("complete", map[string]string{}); err == nil {
		t.Error("complete without id should error")
	}
	if _, err := h.tryRoute("delete", map[string]string{"id": "task_999"}); err == nil {
		t.Error("delete of unknown task should error")
	}
	if _, err := h.tryRoute("clear_done", nil); err == nil {
		t.Error("clear_done with nothing done should error")
	}
	if _, err := h.tryRoute("list", map[string]string{"filter": "bogus"}); err == nil {
		t.Error("list with unknown filter should error")
	}
}

func TestRoutesRoundTrip(t *testing.T) {
	h := newHarness(t)
	var addOut struct {
		ID    string `json:"id"`
		Added bool   `json:"added"`
	}
	h.route(t, "add", map[string]string{"title": "route task"}, &addOut)
	if addOut.ID != "task_1" || !addOut.Added {
		t.Fatalf("add = %+v", addOut)
	}
	var doneOut struct {
		Done bool `json:"done"`
	}
	h.route(t, "complete", map[string]string{"id": "task_1"}, &doneOut)
	if !doneOut.Done {
		t.Error("complete should report done")
	}
	var listOut struct {
		Tasks   []Task `json:"tasks"`
		Total   int    `json:"total"`
		Pending int    `json:"pending"`
		Done    int    `json:"done"`
	}
	h.route(t, "add", map[string]string{"title": "second"}, nil)
	h.route(t, "list", map[string]string{"filter": "done"}, &listOut)
	if len(listOut.Tasks) != 1 || listOut.Tasks[0].ID != "task_1" || listOut.Pending != 1 || listOut.Done != 1 {
		t.Fatalf("filtered list = %+v", listOut)
	}
	var clearOut struct {
		Cleared int `json:"cleared"`
	}
	h.route(t, "clear_done", nil, &clearOut)
	if clearOut.Cleared != 1 {
		t.Errorf("cleared = %+v", clearOut)
	}
	var delOut struct {
		Deleted bool `json:"deleted"`
	}
	h.route(t, "delete", map[string]string{"id": "task_2"}, &delOut)
	if !delOut.Deleted {
		t.Error("delete should report deleted")
	}
	h.route(t, "list", nil, &listOut)
	if listOut.Total != 0 {
		t.Errorf("list after delete = %+v, want empty", listOut)
	}
}

// --- concurrency ---

func TestConcurrentHandlersAndActions(t *testing.T) {
	h := newHarness(t)
	// Establish the presentation instance before goroutines so action
	// requests share it.
	v := h.view(t, 1)
	instance := v.Instance

	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				raw, _ := json.Marshal(map[string]string{"title": fmt.Sprintf("g%d task %d", g, i)})
				if _, err := h.handlers["add"](context.Background(), raw); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := h.handlers["view"](context.Background(), json.RawMessage(`{"version":1}`)); err != nil {
					errs <- err
				}
			}
		}()
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				req := sdk.ActionRequest{
					Version:   1,
					Instance:  instance,
					RequestID: fmt.Sprintf("c_%d_%d", g, i),
					Action:    "filter",
				}
				raw, _ := json.Marshal(req)
				if _, err := h.handlers["action"](context.Background(), raw); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent handler error: %v", err)
	}
	var out struct {
		Total int `json:"total"`
	}
	h.route(t, "list", nil, &out)
	if out.Total != 80 {
		t.Errorf("total after concurrent adds = %d, want 80", out.Total)
	}
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil || len(st.Tasks) != 80 {
		t.Fatalf("persisted state should hold all 80 tasks: %v/%d", err, len(st.Tasks))
	}
}
