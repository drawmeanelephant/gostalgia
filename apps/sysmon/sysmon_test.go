package sysmon

import (
	"context"
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

// harness models the diagnostics surface the monitor reads: sys/status,
// proc/list, proc/info, proc/history, proc/stop, and session/whoami. deny
// simulates a missing grant per method; proc/stop is recorded, not
// performed. No test sleeps: time moves via h.now.
type harness struct {
	now time.Time

	mu       sync.Mutex
	deny     map[string]bool // method -> permission denied
	status   sysStatus
	procs    []procInfo
	hist     []procInfo
	selfPID  int32
	stopCall []int32 // proc/stop invocations, in order

	seq      atomic.Int64
	app      *Sysmon
	handlers map[string]sdk.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessManifest(t, Manifest())
}

// newHarnessManifest builds the instance against an explicit manifest so
// tests can drop the proc.stop grant without editing the embedded file.
func newHarnessManifest(t *testing.T, m sdk.Manifest) *harness {
	t.Helper()
	h := &harness{
		now:      t0,
		deny:     map[string]bool{},
		status:   sysStatus{Version: "0.7.0", User: "guest", UptimeSeconds: 5400},
		selfPID:  7,
		handlers: map[string]sdk.Handler{},
	}
	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	sm := inst.(*Sysmon)
	setClock(sm, func() time.Time { return h.now })
	ctx := sdk.NewContext(m, slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := sm.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = sm
	return h
}

func (h *harness) call(_ context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p struct {
		ID int32 `json:"id"`
	}
	_ = json.Unmarshal(raw, &p)
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Denials land inside each handler, the way RequireCap fires after the
	// route is invoked — proc/stop records the attempt before checking.
	if h.deny[method] && method != "proc/stop" {
		return fmt.Errorf("permission denied: missing capability %q", method)
	}
	switch method {
	case "sys/status":
		return reply(h.status)
	case "proc/list":
		return reply(h.procs)
	case "proc/info":
		for _, pr := range h.procs {
			if pr.ID == p.ID {
				return reply(pr)
			}
		}
		for _, e := range h.hist {
			if e.ID == p.ID {
				return reply(e)
			}
		}
		return fmt.Errorf("process: no such process %d", p.ID)
	case "proc/history":
		return reply(h.hist)
	case "proc/stop":
		h.stopCall = append(h.stopCall, p.ID)
		if h.deny[method] {
			return fmt.Errorf("permission denied: missing capability %q", "proc.stop")
		}
		for i := range h.procs {
			if h.procs[i].ID == p.ID {
				h.procs[i].State = "stopped"
				h.procs[i].ExitedAt = h.now
				return reply(map[string]any{"id": p.ID, "stopped": true})
			}
		}
		return fmt.Errorf("process: no such process %d", p.ID)
	case "session/whoami":
		return reply(map[string]any{
			"principal":    "app",
			"app_id":       ID,
			"process_id":   h.selfPID,
			"user":         "guest",
			"session":      "s1",
			"capabilities": []string{"ipc", "proc.list", "proc.stop"},
		})
	}
	return fmt.Errorf("unknown method %s", method)
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

func (h *harness) act(t *testing.T, version int, action, item string) sdk.View {
	t.Helper()
	v, err := h.tryAct(version, action, item)
	if err != nil {
		t.Fatalf("action %s: %v", action, err)
	}
	return v
}

func (h *harness) tryAct(version int, action, item string) (sdk.View, error) {
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
	hd, ok := h.handlers[name]
	if !ok {
		t.Fatalf("route %q not registered", name)
	}
	raw, _ := json.Marshal(params)
	res, err := hd(context.Background(), raw)
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

func procIDs(v sdk.View) []int32 {
	var ids []int32
	for _, it := range v.Items {
		if pid, kind := parseItemPID(it.ID); pid != 0 && kind == "proc" {
			ids = append(ids, pid)
		}
	}
	return ids
}

// seedSelf installs Sysmon's own in-proc row, matching what the runtime
// registers for the launch.
func (h *harness) seedSelf() {
	h.procs = append(h.procs, procInfo{ID: h.selfPID, Name: ID, Kind: "inproc", State: "running", StartedAt: h.now.Add(-time.Hour)})
}

func running(pid int32, name, kind string, h *harness) procInfo {
	return procInfo{ID: pid, Name: name, Kind: kind, State: "running", StartedAt: h.now.Add(-5 * time.Minute)}
}

// --- manifest ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "sysmon" {
		t.Errorf("manifest = %s/%s", m.ID, m.Entrypoint)
	}
	want := map[string]bool{"ipc": true, "proc.list": true, "proc.stop": true}
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

// --- snapshot assembly ---

func TestLiveTableMatchesProcList(t *testing.T) {
	h := newHarness(t)
	h.seedSelf()
	h.procs = append(h.procs,
		procInfo{ID: 9, Name: "worker", Kind: "child", State: "running", StartedAt: h.now.Add(-2 * time.Minute),
			Resources: resourceUsage{Supported: true, MemoryBytes: 4 * 1024 * 1024, CPUUserMs: 200, CPUSysMs: 10}},
		procInfo{ID: 12, Name: "com.gostalgia.echo", Kind: "inproc", State: "running", StartedAt: h.now.Add(-30 * time.Second)},
	)

	v := h.view(t, 1)
	if v.State != sdk.ViewReady || v.Error != "" {
		t.Fatalf("view = %q/%q", v.State, v.Error)
	}
	up := findItem(v, itemUptime)
	if up == nil || !strings.Contains(up.Detail, "1h 30m") || !strings.Contains(up.Detail, "guest") {
		t.Fatalf("uptime item = %+v", up)
	}
	self := findItem(v, procItemID(7))
	if self == nil || !strings.Contains(self.Label, "(this monitor)") {
		t.Fatalf("self row = %+v", self)
	}
	w := findItem(v, "proc_9")
	if w == nil || !strings.Contains(w.Detail, "running") || !strings.Contains(w.Detail, "child") ||
		!strings.Contains(w.Detail, "cpu 210ms") || !strings.Contains(w.Detail, "mem 4.0 MB") {
		t.Fatalf("child row = %+v", w)
	}
	e := findItem(v, "proc_12")
	if e == nil || !strings.Contains(e.Detail, "inproc") || !strings.Contains(e.Detail, "cpu -") {
		t.Fatalf("inproc row = %+v", e)
	}
	if !strings.Contains(v.Status, "3 live") || !strings.Contains(v.Status, "sorted by pid") {
		t.Errorf("status = %q", v.Status)
	}
	// Pure v1 surface: version-2 requests get no extra elements.
	if v2 := h.view(t, 2); len(v2.Blocks) != 0 || len(v2.Meters) != 0 || v2.Grid != nil {
		t.Error("v2 view should carry no version-2 elements")
	}
}

func TestEmptyStateIsFriendlyNotice(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 1)
	if len(v.Items) != 2 || v.Items[0].ID != itemUptime || v.Items[1].ID != itemEmpty {
		t.Fatalf("empty view items = %+v", v.Items)
	}
	if !strings.Contains(v.Items[1].Detail, "appear here as they launch") {
		t.Errorf("empty detail = %q", v.Items[1].Detail)
	}
	if a := findAction(v, "details"); a == nil || !a.Disabled {
		t.Error("details should be disabled with no rows")
	}
	if a := findAction(v, "kill"); a == nil || !a.Disabled {
		t.Error("kill should be disabled with no killable rows")
	}
}

func TestExitedAndHistoryRows(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{
		{ID: 5, Name: "oldapp", Kind: "inproc", State: "stopped", ExitCode: 0,
			StartedAt: h.now.Add(-time.Hour), ExitedAt: h.now.Add(-30 * time.Minute)},
		{ID: 8, Name: "flaky", Kind: "child", State: "failed", ExitCode: 2, Err: "segfault",
			StartedAt: h.now.Add(-10 * time.Minute), ExitedAt: h.now.Add(-9 * time.Minute)},
	}
	h.hist = []procInfo{
		{ID: 3, Name: "reaped", Kind: "child", State: "stopped", ExitCode: 0, Duration: "2m", ExitedAt: h.now.Add(-time.Hour)},
	}
	v := h.view(t, 1)
	stopped := findItem(v, "proc_5")
	if stopped == nil || !strings.Contains(stopped.Detail, "stopped") || !strings.Contains(stopped.Detail, "exit 0") ||
		!strings.Contains(stopped.Detail, "ended 30m ago") {
		t.Fatalf("stopped row = %+v", stopped)
	}
	failed := findItem(v, "proc_8")
	if failed == nil || !strings.Contains(failed.Detail, "failed") || !strings.Contains(failed.Detail, "exit 2") ||
		!strings.Contains(failed.Detail, "segfault") {
		t.Fatalf("failed row = %+v", failed)
	}
	hist := findItem(v, "hist_3")
	if hist == nil || !strings.Contains(hist.Detail, "exit 0") || !strings.Contains(hist.Detail, "ran 2m") ||
		!strings.Contains(hist.Detail, "ended 1h ago") {
		t.Fatalf("history row = %+v", hist)
	}
	if !strings.Contains(v.Status, "0 live") || !strings.Contains(v.Status, "1 exits") {
		t.Errorf("status = %q", v.Status)
	}
}

func TestSortCyclesFields(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{
		{ID: 5, Name: "zeta", Kind: "child", State: "running", StartedAt: h.now.Add(-time.Minute),
			Resources: resourceUsage{Supported: true, MemoryBytes: 1024, CPUUserMs: 10}},
		{ID: 2, Name: "alpha", Kind: "child", State: "running", StartedAt: h.now.Add(-time.Minute),
			Resources: resourceUsage{Supported: true, MemoryBytes: 4096, CPUUserMs: 900}},
		{ID: 9, Name: "mid", Kind: "inproc", State: "running", StartedAt: h.now.Add(-time.Minute)},
	}

	if got := procIDs(h.view(t, 1)); fmt.Sprint(got) != "[2 5 9]" {
		t.Fatalf("pid order = %v", got)
	}
	v := h.act(t, 1, "sort", "")
	if a := findAction(v, "sort"); a.Label != "Sort: "+sortByName {
		t.Fatalf("sort label = %q", a.Label)
	}
	if got := procIDs(v); fmt.Sprint(got) != "[2 9 5]" { // alpha, mid, zeta
		t.Fatalf("name order = %v", got)
	}
	v = h.act(t, 1, "sort", "")
	if a := findAction(v, "sort"); a.Label != "Sort: "+sortByCPU {
		t.Fatalf("sort label = %q", a.Label)
	}
	// CPU desc, unsupported (inproc) sinks last.
	if got := procIDs(v); fmt.Sprint(got) != "[2 5 9]" {
		t.Fatalf("cpu order = %v", got)
	}
	v = h.act(t, 1, "sort", "")
	if a := findAction(v, "sort"); a.Label != "Sort: "+sortByRAM {
		t.Fatalf("sort label = %q", a.Label)
	}
	if got := procIDs(v); fmt.Sprint(got) != "[2 5 9]" { // 4KB before 1KB before unsupported
		t.Fatalf("mem order = %v", got)
	}
	v = h.act(t, 1, "sort", "")
	if a := findAction(v, "sort"); a.Label != "Sort: "+sortByPID {
		t.Fatalf("sort label = %q after wrap", a.Label)
	}
}

func TestTruncationIndicatorIsHonest(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxProcRows+5; i++ {
		h.procs = append(h.procs, running(int32(100+i), fmt.Sprintf("job%02d", i), "child", h))
	}
	v := h.view(t, 1)
	var procCount int
	for _, it := range v.Items {
		if _, kind := parseItemPID(it.ID); kind == "proc" {
			procCount++
		}
	}
	if procCount != maxProcRows {
		t.Fatalf("rendered proc rows = %d, want cap %d", procCount, maxProcRows)
	}
	last := v.Items[len(v.Items)-1]
	if last.ID != itemMoreProcs || !strings.Contains(last.Label, "5 more") {
		t.Fatalf("truncation indicator = %+v", last)
	}
}

// --- selection / detail ---

func TestDetailsTogglePinsAndUnpins(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}

	if _, err := h.tryAct(1, "details", ""); err != nil {
		t.Fatalf("details: %v", err)
	}
	v := h.act(t, 1, "details", "proc_9")
	d := findItem(v, itemDetail)
	if d == nil || !strings.Contains(d.Label, "PID 9 — worker") {
		t.Fatalf("detail item = %+v", d)
	}
	if !strings.Contains(d.Detail, "running") || !strings.Contains(d.Detail, "child") ||
		!strings.Contains(d.Detail, "started 5m ago") {
		t.Errorf("detail = %q", d.Detail)
	}
	if !strings.Contains(v.Status, "watching PID 9") {
		t.Errorf("status = %q", v.Status)
	}

	// The pin survives subsequent polls — the detail re-fetches each view.
	h.procs[0].State = "stopping"
	v = h.view(t, 1)
	if d := findItem(v, itemDetail); d == nil || !strings.Contains(d.Detail, "stopping") {
		t.Fatalf("detail did not follow state change: %+v", d)
	}

	// Same row again unpins.
	v = h.act(t, 1, "details", "proc_9")
	if findItem(v, itemDetail) != nil {
		t.Error("detail should unpin on re-select")
	}
	if !strings.Contains(v.Status, "unpinned") {
		t.Errorf("status = %q", v.Status)
	}
}

func TestDetailsRejectsNonProcessRows(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}
	v := h.act(t, 1, "details", itemUptime)
	if !strings.Contains(v.Status, "not a process row") {
		t.Errorf("status = %q", v.Status)
	}
	if findItem(v, itemDetail) != nil {
		t.Error("no detail should pin for the uptime row")
	}
	if v.State != sdk.ViewReady || v.Error != "" {
		t.Error("a domain rejection should not become an error banner")
	}
}

func TestDetailsFollowsExitedProcess(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}
	h.act(t, 1, "details", "proc_9")

	// The process exits and is reaped before the next poll: proc/info
	// answers from the history buffer and the pinned row stays honest.
	h.procs = nil
	h.hist = []procInfo{{ID: 9, Name: "worker", Kind: "child", State: "stopped", ExitCode: 0,
		Duration: "5m", ExitedAt: h.now.Add(-time.Minute)}}
	v := h.view(t, 1)
	d := findItem(v, itemDetail)
	if d == nil || !strings.Contains(d.Detail, "stopped") || !strings.Contains(d.Detail, "exit 0") {
		t.Fatalf("detail after exit = %+v", d)
	}
}

func TestDetailsHonestWhenProcessVanishes(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}
	h.act(t, 1, "details", "proc_9")
	h.procs = nil // gone, and never made history
	v := h.view(t, 1)
	d := findItem(v, itemDetail)
	if d == nil || !strings.Contains(d.Detail, "no detail") || !strings.Contains(d.Detail, "no such process") {
		t.Fatalf("vanished detail = %+v", d)
	}
}

// --- kill ---

func TestKillTerminatesChild(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}
	v := h.act(t, 1, "kill", "proc_9")
	if len(h.stopCall) != 1 || h.stopCall[0] != 9 {
		t.Fatalf("proc/stop calls = %v", h.stopCall)
	}
	if !strings.Contains(v.Status, "terminated PID 9") {
		t.Errorf("status = %q", v.Status)
	}
	row := findItem(v, "proc_9")
	if row == nil || !strings.Contains(row.Detail, "stopped") {
		t.Fatalf("row after kill = %+v", row)
	}
}

func TestKillInprocFailsHonestly(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(12, "com.gostalgia.echo", "inproc", h)}
	v := h.act(t, 1, "kill", "proc_12")
	if len(h.stopCall) != 0 {
		t.Fatalf("proc/stop must not run for in-proc rows: %v", h.stopCall)
	}
	if !strings.Contains(v.Status, "in-proc") {
		t.Errorf("status = %q, want an honest in-proc rejection", v.Status)
	}
	if v.Error != "" || v.State != sdk.ViewReady {
		t.Error("a domain rejection should stay in the status line")
	}
}

func TestKillNeverTargetsSelf(t *testing.T) {
	h := newHarness(t)
	h.seedSelf()
	v := h.act(t, 1, "kill", procItemID(h.selfPID))
	if len(h.stopCall) != 0 {
		t.Fatalf("proc/stop must never run on the monitor itself: %v", h.stopCall)
	}
	if !strings.Contains(v.Status, "does not terminate itself") {
		t.Errorf("status = %q", v.Status)
	}
}

func TestKillExitedAndMissingRows(t *testing.T) {
	h := newHarness(t)
	h.hist = []procInfo{{ID: 4, Name: "old", Kind: "child", State: "stopped", ExitCode: 0}}
	h.procs = []procInfo{{ID: 8, Name: "done", Kind: "child", State: "failed", ExitCode: 1}}

	v := h.act(t, 1, "kill", "hist_4")
	if !strings.Contains(v.Status, "already exited") || len(h.stopCall) != 0 {
		t.Fatalf("history kill = %q / %v", v.Status, h.stopCall)
	}
	v = h.act(t, 1, "kill", "proc_8")
	if !strings.Contains(v.Status, "nothing to terminate") || len(h.stopCall) != 0 {
		t.Fatalf("failed-proc kill = %q / %v", v.Status, h.stopCall)
	}
	// A pid that vanished between render and action is rejected by the
	// SDK's item check; direct calls still get the honest domain answer.
	v, err := h.app.act(context.Background(), sdk.ActionRequest{Action: "kill", ItemID: "proc_77"})
	if err != nil {
		t.Fatalf("kill of missing pid: %v", err)
	}
	if !strings.Contains(v.Status, "is gone") || len(h.stopCall) != 0 {
		t.Fatalf("missing-proc kill = %q / %v", v.Status, h.stopCall)
	}
}

func TestKillDeniedSurfacesHonestly(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}
	h.deny["proc/stop"] = true
	v := h.act(t, 1, "kill", "proc_9")
	if !strings.Contains(v.Status, "permission denied") {
		t.Errorf("denial should surface verbatim in the status line: %q", v.Status)
	}
	if len(h.stopCall) != 1 {
		t.Fatalf("the attempt should still reach proc/stop: %v", h.stopCall)
	}
}

func TestReadOnlyWhenManifestOmitsProcStop(t *testing.T) {
	m := Manifest()
	m.Permissions = []string{"ipc", "proc.list"}
	h := newHarnessManifest(t, m)
	h.procs = []procInfo{running(9, "worker", "child", h)}

	v := h.view(t, 1)
	if findAction(v, "kill") != nil {
		t.Fatal("kill action must not render without the grant")
	}
	// A forged request is refused twice: the SDK rejects the undeclared
	// action, and the app itself checks the manifest.
	if _, err := h.tryAct(1, "kill", "proc_9"); err == nil {
		t.Error("kill action should be rejected without the grant")
	}
	if _, err := h.app.act(context.Background(), sdk.ActionRequest{Action: "kill", ItemID: "proc_9"}); err == nil ||
		!strings.Contains(err.Error(), "proc.stop") {
		t.Errorf("direct kill = %v, want an honest grant error", err)
	}
	// The read-only monitor itself is unaffected.
	if findItem(v, "proc_9") == nil || findAction(v, "details") == nil || findAction(v, "sort") == nil {
		t.Error("read-only view should keep its table and actions")
	}
}

// --- grant-denied reads ---

func TestDeniedProcListDegradesToNoticeAndBanner(t *testing.T) {
	h := newHarness(t)
	h.deny["proc/list"] = true
	v := h.view(t, 1)
	if v.State != sdk.ViewReady {
		t.Fatalf("denied list should still render: %q", v.State)
	}
	if !strings.Contains(v.Error, "proc/list") || !strings.Contains(v.Error, "permission denied") {
		t.Errorf("banner = %q", v.Error)
	}
	notice := findItem(v, itemProcErr)
	if notice == nil || !strings.Contains(notice.Detail, "permission denied") {
		t.Fatalf("procerr notice = %+v", notice)
	}
	if !strings.Contains(v.Status, "unavailable") {
		t.Errorf("status = %q", v.Status)
	}
}

func TestDeniedHistoryKeepsTable(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}
	h.deny["proc/history"] = true
	v := h.view(t, 1)
	if findItem(v, "proc_9") == nil {
		t.Fatal("the table should survive a denied history call")
	}
	if findItem(v, itemHistErr) == nil {
		t.Error("missing honest history notice")
	}
	if !strings.Contains(v.Error, "proc/history") {
		t.Errorf("banner = %q", v.Error)
	}
}

func TestDeniedSysStatusKeepsTable(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h)}
	h.deny["sys/status"] = true
	v := h.view(t, 1)
	up := findItem(v, itemUptime)
	if up == nil || !strings.Contains(up.Detail, "status unavailable") {
		t.Fatalf("uptime under denial = %+v", up)
	}
	if findItem(v, "proc_9") == nil {
		t.Error("the table should survive a denied status call")
	}
	if !strings.Contains(v.Error, "sys/status") {
		t.Errorf("banner = %q", v.Error)
	}
}

// --- actions and routes ---

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(1, "launch_missiles", ""); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

func TestSnapshotRouteMirrorsView(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h), running(12, "echo", "inproc", h)}
	h.hist = []procInfo{{ID: 3, Name: "old", Kind: "child", State: "stopped"}}
	h.act(t, 1, "details", "proc_9")

	var out struct {
		UptimeSeconds float64    `json:"uptime_seconds"`
		User          string     `json:"user"`
		Sort          string     `json:"sort"`
		SelectedPID   int32      `json:"selected_pid"`
		SelfPID       int32      `json:"self_pid"`
		Processes     []procInfo `json:"processes"`
		History       []procInfo `json:"history"`
	}
	h.route(t, "snapshot", nil, &out)
	if out.UptimeSeconds != 5400 || out.User != "guest" {
		t.Errorf("snapshot status = %+v", out)
	}
	if out.SelectedPID != 9 || out.SelfPID != h.selfPID {
		t.Errorf("snapshot selection = %+v", out)
	}
	if len(out.Processes) != 2 || len(out.History) != 1 {
		t.Errorf("snapshot data = %+v", out)
	}
	if out.Sort != sortByPID {
		t.Errorf("sort = %q", out.Sort)
	}
}

func TestSnapshotRouteReportsErrors(t *testing.T) {
	h := newHarness(t)
	h.deny["proc/list"] = true
	var out struct {
		Errors map[string]string `json:"errors"`
	}
	h.route(t, "snapshot", nil, &out)
	if !strings.Contains(out.Errors["proc/list"], "permission denied") {
		t.Errorf("route errors = %+v", out.Errors)
	}
}

// --- concurrency ---

func TestConcurrentHandlersAndActions(t *testing.T) {
	h := newHarness(t)
	h.procs = []procInfo{running(9, "worker", "child", h), running(12, "echo", "inproc", h)}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				if _, err := h.handlers["view"](context.Background(), json.RawMessage(`{"version":1}`)); err != nil {
					t.Errorf("view: %v", err)
				}
			case 1:
				if _, err := h.tryAct(1, "sort", ""); err != nil {
					t.Errorf("sort: %v", err)
				}
			case 2:
				h.tryAct(1, "details", "proc_9")
			case 3:
				if _, err := h.handlers["snapshot"](context.Background(), nil); err != nil {
					t.Errorf("snapshot route: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
}
