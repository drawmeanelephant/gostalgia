// Package sysmon is Gostalgia's process-and-system monitor: uptime, a
// live process table with state/CPU/RAM/exit info, and recent process
// history. It is the utilitarian sibling of the shell's Task Manager,
// built on the same runtime diagnostics — sys/status, proc/list,
// proc/info, and proc/history are re-fetched inside every view snapshot,
// so the shell's ~500ms poll is the whole refresh story. There are no
// app-side timers or goroutines.
//
// Termination ships as a kill action only while the manifest grants
// proc.stop. The monitor is deliberately narrower than the raw route:
// only child processes are terminable here (in-proc apps belong to the
// app manager, not a monitor), a row that already exited fails honestly,
// and Sysmon never terminates its own process.
package sysmon

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Sysmon.
const ID = "com.gostalgia.sysmon"

// Row bounds. A monitor stays useful while the table stays scannable, and
// the whole snapshot must fit the shared 100-item budget regardless —
// uptime, detail, indicators, and history all share it with the live rows.
const (
	maxProcRows    = 64
	maxHistoryRows = 8
)

// Sort fields the sort action cycles through. pid and name sort
// ascending; cpu and mem sort descending so the heaviest rows lead.
const (
	sortByPID  = "pid"
	sortByName = "name"
	sortByCPU  = "cpu"
	sortByRAM  = "ram"
)

// Reserved item IDs that are notices or pinned rows, never processes.
const (
	itemUptime    = "uptime"
	itemDetail    = "detail"
	itemEmpty     = "empty"
	itemProcErr   = "procerr"
	itemHistErr   = "histerr"
	itemMoreProcs = "more_procs"
	itemMoreHist  = "more_history"
)

// liveStates are the states in which a process can still be terminated.
var liveStates = map[string]bool{
	"starting":   true,
	"running":    true,
	"stopping":   true,
	"restarting": true,
}

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

// resourceUsage mirrors platform.ResourceUsage; the app decodes the wire
// shape rather than importing runtime internals.
type resourceUsage struct {
	Supported   bool   `json:"supported"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	CPUUserMs   int64  `json:"cpu_user_ms,omitempty"`
	CPUSysMs    int64  `json:"cpu_sys_ms,omitempty"`
}

// procInfo decodes both the proc/list and proc/info payloads and the
// proc/history entry shape — history entries carry the same fields plus
// a precomputed duration string.
type procInfo struct {
	ID           int32         `json:"id"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	State        string        `json:"state"`
	User         string        `json:"user,omitempty"`
	SessionID    string        `json:"session,omitempty"`
	Caps         []string      `json:"caps,omitempty"`
	StartedAt    time.Time     `json:"started_at,omitempty"`
	ExitedAt     time.Time     `json:"exited_at,omitempty"`
	Duration     string        `json:"duration,omitempty"`
	Err          string        `json:"error,omitempty"`
	ExitCode     int           `json:"exit_code,omitempty"`
	RestartCount int           `json:"restart_count,omitempty"`
	CrashLoop    bool          `json:"crash_loop,omitempty"`
	Isolation    string        `json:"isolation,omitempty"`
	Resources    resourceUsage `json:"resources"`
}

// sysStatus decodes the sys/status fields the monitor renders.
type sysStatus struct {
	Version       string  `json:"version"`
	User          string  `json:"user"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

// snapshot is one poll's worth of diagnostics data plus the errors each
// call produced. Sections degrade independently: a denied proc/list does
// not blank the uptime row, and missing history does not hide the table.
type snapshot struct {
	status    sysStatus
	hasStatus bool
	statusErr error
	procs     []procInfo
	listErr   error
	hist      []procInfo
	histErr   error
	detail    *procInfo
	detailErr error
}

// Sysmon is the running in-process instance. All state is guarded by mu;
// sort order and the pinned selection are view state, never persisted.
type Sysmon struct {
	app *sdk.Context
	now func() time.Time

	mu          sync.Mutex
	sortField   string
	selectedPID int32 // process pinned in the detail row; 0 = none

	selfPID   int32
	selfKnown bool // whoami answered; a failed probe retries on the next poll

	statusNote string // transient feedback line; cleared on each action
}

// Factory constructs a fresh, uninitialized Sysmon instance.
func Factory() (sdk.Instance, error) {
	return &Sysmon{now: time.Now, sortField: sortByPID}, nil
}

// setClock injects a deterministic clock for tests.
func setClock(s *Sysmon, now func() time.Time) { s.now = now }

// Init registers the programmatic route and the presentation callbacks.
// Nothing is fetched here: the first view or action call pulls state.
func (s *Sysmon) Init(app *sdk.Context) error {
	s.app = app
	if s.now == nil {
		s.now = time.Now
	}
	if s.sortField == "" {
		s.sortField = sortByPID
	}
	if err := app.Handle("snapshot", s.snapshotRoute); err != nil {
		return err
	}
	return app.Present(s.view, s.act)
}

// Run blocks until the instance is canceled. The monitor has no tick:
// every view call re-fetches the diagnostics it renders.
func (s *Sysmon) Run(ctx context.Context) error {
	s.app.Log.Info("sysmon running")
	<-ctx.Done()
	return nil
}

// Stop releases nothing; Sysmon holds no resources between calls.
func (s *Sysmon) Stop(ctx context.Context) error { return nil }

// hasCap reports whether the manifest grant includes cap. Runtime
// self-check, not a cache: a seeded manifest that omits proc.stop must
// produce the read-only monitor.
func (s *Sysmon) hasCap(cap string) bool {
	for _, c := range s.app.Manifest.Permissions {
		if c == cap {
			return true
		}
	}
	return false
}

// resolveSelfLocked learns this instance's process ID once via
// session/whoami so kill can refuse itself. A failed probe is not
// latched — the next poll retries.
func (s *Sysmon) resolveSelfLocked(ctx context.Context) {
	if s.selfKnown {
		return
	}
	var out struct {
		ProcessID int32 `json:"process_id"`
	}
	if err := s.app.Call(ctx, "session/whoami", nil, &out); err == nil && out.ProcessID != 0 {
		s.selfPID = out.ProcessID
		s.selfKnown = true
	}
}

// fetchLocked pulls every diagnostic the view renders. Callers hold mu.
func (s *Sysmon) fetchLocked(ctx context.Context) snapshot {
	var snap snapshot
	if err := s.app.Call(ctx, "sys/status", nil, &snap.status); err != nil {
		snap.statusErr = err
	} else {
		snap.hasStatus = true
	}
	var procs []procInfo
	if err := s.app.Call(ctx, "proc/list", nil, &procs); err != nil {
		snap.listErr = err
	} else {
		snap.procs = procs
		sortProcs(snap.procs, s.sortField)
	}
	var hist []procInfo
	if err := s.app.Call(ctx, "proc/history", nil, &hist); err != nil {
		snap.histErr = err
	} else {
		snap.hist = hist
	}
	if s.selectedPID != 0 {
		var info procInfo
		if err := s.app.Call(ctx, "proc/info", map[string]any{"id": s.selectedPID}, &info); err != nil {
			snap.detailErr = err
		} else {
			snap.detail = &info
		}
	}
	return snap
}

// --- presentation ---

func (s *Sysmon) view(ctx context.Context) (sdk.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveSelfLocked(ctx)
	return s.renderLocked(s.fetchLocked(ctx)), nil
}

func (s *Sysmon) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveSelfLocked(ctx)
	s.statusNote = ""

	var err error
	switch req.Action {
	case "sort":
		s.cycleSortLocked()
	case "details":
		err = s.toggleDetailsLocked(req.ItemID)
	case "kill":
		// The action is only rendered with the grant, so a request that
		// still arrives is a protocol violation, not a domain rejection.
		if !s.hasCap(sdk.CapProcStop) {
			return sdk.View{}, fmt.Errorf("action %q is unavailable: this manifest grants no proc.stop", req.Action)
		}
		err = s.killLocked(ctx, req.ItemID)
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		// Domain rejections land in the status line, not the error banner:
		// the table keeps rendering and every valid action stays usable.
		s.statusNote = err.Error()
	}
	return s.renderLocked(s.fetchLocked(ctx)), nil
}

// cycleSortLocked rotates pid -> name -> cpu -> ram -> pid. The order is
// view state, like todo's filter, and deliberately not persisted.
func (s *Sysmon) cycleSortLocked() {
	switch s.sortField {
	case sortByPID:
		s.sortField = sortByName
	case sortByName:
		s.sortField = sortByCPU
	case sortByCPU:
		s.sortField = sortByRAM
	default:
		s.sortField = sortByPID
	}
	s.statusNote = "sorted by " + s.sortField
}

// toggleDetailsLocked pins the selected process's detail row, or unpins
// it when the same row is chosen again. Live rows and history rows both
// resolve — proc/info falls back to the bounded history buffer, so an
// exited process still produces an honest detail.
func (s *Sysmon) toggleDetailsLocked(itemID string) error {
	if itemID == "" {
		return fmt.Errorf("select a process row first")
	}
	pid, kind := parseItemPID(itemID)
	if pid == 0 {
		return fmt.Errorf("%q is not a process row", itemID)
	}
	if s.selectedPID == pid {
		s.selectedPID = 0
		s.statusNote = fmt.Sprintf("PID %d unpinned", pid)
		return nil
	}
	s.selectedPID = pid
	if kind == "hist" {
		s.statusNote = fmt.Sprintf("watching PID %d (exited)", pid)
	} else {
		s.statusNote = fmt.Sprintf("watching PID %d", pid)
	}
	return nil
}

// killLocked terminates the selected child process. The grant is checked
// from the manifest, the target is re-read from proc/list so a stale
// selection cannot blind-fire, and in-proc rows — including Sysmon
// itself — are refused honestly before the route is touched.
func (s *Sysmon) killLocked(ctx context.Context, itemID string) error {
	if itemID == "" {
		return fmt.Errorf("select a process to terminate")
	}
	pid, kind := parseItemPID(itemID)
	if pid == 0 {
		return fmt.Errorf("%q is not a process row", itemID)
	}
	if kind == "hist" {
		return fmt.Errorf("PID %d already exited — nothing to terminate", pid)
	}
	if pid == s.selfPID {
		return fmt.Errorf("sysmon does not terminate itself")
	}

	// Verify live state before touching proc/stop: the row that was
	// rendered a poll ago may have exited or been reaped since.
	var procs []procInfo
	if err := s.app.Call(ctx, "proc/list", nil, &procs); err != nil {
		return fmt.Errorf("cannot verify PID %d: %w", pid, err)
	}
	var row *procInfo
	for i := range procs {
		if procs[i].ID == pid {
			row = &procs[i]
			break
		}
	}
	if row == nil {
		return fmt.Errorf("PID %d is gone — nothing to terminate", pid)
	}
	if row.Name == ID {
		return fmt.Errorf("sysmon does not terminate itself")
	}
	if row.Kind != "child" {
		return fmt.Errorf("PID %d (%s) is in-proc — only child processes can be terminated here", pid, row.Name)
	}
	if !liveStates[row.State] {
		return fmt.Errorf("PID %d (%s) is %s — nothing to terminate", pid, row.Name, stateText(*row))
	}
	var out struct {
		Stopped bool `json:"stopped"`
	}
	if err := s.app.Call(ctx, "proc/stop", map[string]any{"id": pid}, &out); err != nil {
		return fmt.Errorf("terminate PID %d: %w", pid, err)
	}
	s.statusNote = fmt.Sprintf("terminated PID %d (%s)", pid, row.Name)
	return nil
}

// renderLocked assembles the snapshot into the view. Section failures are
// honest notice items; every failed call also lands on the error banner.
func (s *Sysmon) renderLocked(snap snapshot) sdk.View {
	now := s.now()
	items := make([]sdk.Item, 0, maxProcRows+maxHistoryRows+4)
	items = append(items, s.uptimeItem(snap))
	if s.selectedPID != 0 {
		items = append(items, detailItem(s.selectedPID, now, snap))
	}

	procRows := 0
	switch {
	case snap.listErr != nil:
		items = append(items, sdk.Item{
			ID:     itemProcErr,
			Label:  "Process list unavailable",
			Detail: snap.listErr.Error(),
		})
	default:
		shown := snap.procs
		if len(shown) > maxProcRows {
			shown = shown[:maxProcRows]
		}
		for _, p := range shown {
			items = append(items, s.procItem(now, p))
			procRows++
		}
		if rest := len(snap.procs) - len(shown); rest > 0 {
			items = append(items, sdk.Item{
				ID:     itemMoreProcs,
				Label:  fmt.Sprintf("… and %d more", rest),
				Detail: fmt.Sprintf("the table is capped at %d rows — sorted by %s", maxProcRows, s.sortField),
			})
		}
		if len(snap.procs) == 0 {
			items = append(items, sdk.Item{
				ID:     itemEmpty,
				Label:  "No live processes",
				Detail: "apps and child jobs appear here as they launch",
			})
		}
	}

	histRows := 0
	switch {
	case snap.histErr != nil:
		items = append(items, sdk.Item{
			ID:     itemHistErr,
			Label:  "History unavailable",
			Detail: snap.histErr.Error(),
		})
	default:
		shown := snap.hist
		if len(shown) > maxHistoryRows {
			shown = shown[:maxHistoryRows]
		}
		for _, p := range shown {
			items = append(items, histItem(now, p))
			histRows++
		}
		if rest := len(snap.hist) - len(shown); rest > 0 {
			items = append(items, sdk.Item{
				ID:     itemMoreHist,
				Label:  fmt.Sprintf("… and %d more exits", rest),
				Detail: "history is bounded; gctl history shows the full buffer",
			})
		}
	}

	return sdk.View{
		Title:   "Sysmon",
		State:   sdk.ViewReady,
		Items:   items,
		Actions: s.actions(procRows+histRows > 0),
		Status:  s.statusLocked(snap),
		Error:   bannerErr(snap),
	}
}

// uptimeItem is the pinned first row: environment uptime, user, and
// version from sys/status, or the honest reason it is missing.
func (s *Sysmon) uptimeItem(snap snapshot) sdk.Item {
	if !snap.hasStatus {
		return sdk.Item{ID: itemUptime, Label: "Uptime", Detail: "status unavailable: " + snap.statusErr.Error()}
	}
	st := snap.status
	parts := []string{"up " + formatUptime(st.UptimeSeconds)}
	if st.User != "" {
		parts = append(parts, "user "+st.User)
	}
	if st.Version != "" {
		parts = append(parts, "gostalgia "+st.Version)
	}
	return sdk.Item{ID: itemUptime, Label: "Uptime", Detail: strings.Join(parts, " · ")}
}

// procItem renders one live-table row: `pid  name` plus state, kind,
// CPU/memory, and uptime or exit info. Sysmon's own row is marked so the
// monitor cannot quietly pretend it is not in the table.
func (s *Sysmon) procItem(now time.Time, p procInfo) sdk.Item {
	label := fmt.Sprintf("%d  %s", p.ID, clip(p.Name, 80))
	self := p.ID == s.selfPID || p.Name == ID
	if self {
		label += "  (this monitor)"
	}
	parts := []string{stateText(p), p.Kind}
	if liveStates[p.State] {
		parts = append(parts, "cpu "+cpuText(p.Resources), "mem "+memText(p.Resources))
		if !p.StartedAt.IsZero() {
			parts = append(parts, "up "+ago(now.Sub(p.StartedAt)))
		}
	} else {
		parts = append(parts, "exit "+strconv.Itoa(p.ExitCode))
		if !p.ExitedAt.IsZero() {
			parts = append(parts, "ended "+ago(now.Sub(p.ExitedAt)))
		}
	}
	if p.RestartCount > 0 {
		parts = append(parts, fmt.Sprintf("restarts %d", p.RestartCount))
	}
	if p.Err != "" {
		parts = append(parts, "err: "+clip(p.Err, 120))
	}
	return sdk.Item{ID: procItemID(p.ID), Label: label, Detail: strings.Join(parts, " · ")}
}

// detailItem is the pinned row under uptime describing the selected
// process, refreshed through proc/info on every poll — including the
// history fallback after exit. A lookup failure reads as its own honest
// row rather than dropping the section.
func detailItem(pid int32, now time.Time, snap snapshot) sdk.Item {
	if snap.detailErr != nil {
		return sdk.Item{
			ID:     itemDetail,
			Label:  fmt.Sprintf("PID %d", pid),
			Detail: "no detail: " + snap.detailErr.Error(),
		}
	}
	p := snap.detail
	label := fmt.Sprintf("PID %d — %s", p.ID, clip(p.Name, 60))
	parts := []string{stateText(*p), p.Kind}
	if p.Isolation != "" {
		parts = append(parts, "isolation "+p.Isolation)
	}
	if p.User != "" {
		parts = append(parts, "user "+p.User)
	}
	if p.SessionID != "" {
		parts = append(parts, "session "+p.SessionID)
	}
	if len(p.Caps) > 0 {
		parts = append(parts, "caps "+strings.Join(p.Caps, ","))
	}
	parts = append(parts, "cpu "+cpuText(p.Resources), "mem "+memText(p.Resources))
	if !p.StartedAt.IsZero() {
		parts = append(parts, "started "+ago(now.Sub(p.StartedAt)))
	}
	if !p.ExitedAt.IsZero() {
		parts = append(parts, "exited "+ago(now.Sub(p.ExitedAt)))
	}
	if p.Duration != "" {
		parts = append(parts, "ran "+p.Duration)
	}
	if !liveStates[p.State] {
		parts = append(parts, "exit "+strconv.Itoa(p.ExitCode))
	}
	if p.RestartCount > 0 {
		parts = append(parts, fmt.Sprintf("restarts %d", p.RestartCount))
	}
	if p.Err != "" {
		parts = append(parts, "err: "+clip(p.Err, 120))
	}
	return sdk.Item{ID: itemDetail, Label: label, Detail: strings.Join(parts, " · ")}
}

// histItem renders one bounded-history row, newest first.
func histItem(now time.Time, p procInfo) sdk.Item {
	parts := []string{stateText(p), "exit " + strconv.Itoa(p.ExitCode)}
	if p.Duration != "" {
		parts = append(parts, "ran "+p.Duration)
	}
	if !p.ExitedAt.IsZero() {
		parts = append(parts, "ended "+ago(now.Sub(p.ExitedAt)))
	}
	if p.Err != "" {
		parts = append(parts, "err: "+clip(p.Err, 120))
	}
	return sdk.Item{
		ID:     histItemID(p.ID),
		Label:  fmt.Sprintf("%d  %s", p.ID, clip(p.Name, 80)),
		Detail: strings.Join(parts, " · "),
	}
}

// actions lists the labeled buttons. kill ships only while the manifest
// grants proc.stop. It stays enabled whenever a row is selectable —
// per-target honesty (self, in-proc, exited, gone) is the status line's
// job, since the action cannot know which item will be highlighted.
func (s *Sysmon) actions(anySelectable bool) []sdk.Action {
	actions := []sdk.Action{
		{ID: "details", Label: "Details", Disabled: !anySelectable},
		{ID: "sort", Label: "Sort: " + s.sortField},
	}
	if s.hasCap(sdk.CapProcStop) {
		actions = append(actions, sdk.Action{ID: "kill", Label: "Terminate", Disabled: !anySelectable})
	}
	return actions
}

// statusLocked picks the line under the list: transient notes win, then
// the honest summary of what the poll just fetched.
func (s *Sysmon) statusLocked(snap snapshot) string {
	if s.statusNote != "" {
		return s.statusNote
	}
	if snap.listErr != nil {
		return "process table unavailable"
	}
	live := 0
	for _, p := range snap.procs {
		if liveStates[p.State] {
			live++
		}
	}
	status := fmt.Sprintf("%d live · %d in table · %d exits · sorted by %s",
		live, len(snap.procs), len(snap.hist), s.sortField)
	if s.selectedPID != 0 {
		status += fmt.Sprintf(" · watching PID %d", s.selectedPID)
	}
	return status
}

// bannerErr joins every failed call into the error banner; per-section
// notice items carry the same text where the data would have been.
func bannerErr(snap snapshot) string {
	var errs []string
	if snap.statusErr != nil {
		errs = append(errs, "sys/status: "+snap.statusErr.Error())
	}
	if snap.listErr != nil {
		errs = append(errs, "proc/list: "+snap.listErr.Error())
	}
	if snap.histErr != nil {
		errs = append(errs, "proc/history: "+snap.histErr.Error())
	}
	return strings.Join(errs, " · ")
}

// --- programmatic IPC routes ---

// snapshotRoute returns the same assembly the view renders: uptime,
// the sorted live table, recent exits, and per-call errors. Scripts get
// one call instead of three.
func (s *Sysmon) snapshotRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveSelfLocked(ctx)
	snap := s.fetchLocked(ctx)
	out := map[string]any{
		"sort":         s.sortField,
		"selected_pid": s.selectedPID,
		"self_pid":     s.selfPID,
		"processes":    snap.procs,
		"history":      snap.hist,
	}
	if snap.hasStatus {
		out["uptime_seconds"] = snap.status.UptimeSeconds
		out["user"] = snap.status.User
		out["version"] = snap.status.Version
	}
	errs := map[string]string{}
	if snap.statusErr != nil {
		errs["sys/status"] = snap.statusErr.Error()
	}
	if snap.listErr != nil {
		errs["proc/list"] = snap.listErr.Error()
	}
	if snap.histErr != nil {
		errs["proc/history"] = snap.histErr.Error()
	}
	if snap.detailErr != nil {
		errs["proc/info"] = snap.detailErr.Error()
	}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	return out, nil
}

// --- helpers ---

func procItemID(pid int32) string { return "proc_" + strconv.Itoa(int(pid)) }
func histItemID(pid int32) string { return "hist_" + strconv.Itoa(int(pid)) }

// parseItemPID splits a proc_/hist_ item ID into its PID and section,
// reporting 0 for the reserved notice rows.
func parseItemPID(itemID string) (int32, string) {
	for _, prefix := range []struct {
		tag  string
		kind string
	}{{"proc_", "proc"}, {"hist_", "hist"}} {
		if rest, ok := strings.CutPrefix(itemID, prefix.tag); ok {
			n, err := strconv.ParseInt(rest, 10, 32)
			if err != nil || n <= 0 {
				return 0, ""
			}
			return int32(n), prefix.kind
		}
	}
	return 0, ""
}

// sortProcs orders the live table by the current field; ties and rows
// without metrics fall back to pid order, and unsupported metrics sink.
func sortProcs(procs []procInfo, field string) {
	sort.SliceStable(procs, func(i, j int) bool {
		a, b := procs[i], procs[j]
		switch field {
		case sortByName:
			la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
			if la != lb {
				return la < lb
			}
		case sortByCPU:
			if a.Resources.Supported != b.Resources.Supported {
				return a.Resources.Supported
			}
			ca, cb := a.Resources.CPUUserMs+a.Resources.CPUSysMs, b.Resources.CPUUserMs+b.Resources.CPUSysMs
			if ca != cb {
				return ca > cb
			}
		case sortByRAM:
			if a.Resources.Supported != b.Resources.Supported {
				return a.Resources.Supported
			}
			if a.Resources.MemoryBytes != b.Resources.MemoryBytes {
				return a.Resources.MemoryBytes > b.Resources.MemoryBytes
			}
		}
		return a.ID < b.ID
	})
}

// stateText renders the row's state the way Task Manager does: a
// crash-looping process reads "crash loop", a restarting one "backoff".
func stateText(p procInfo) string {
	if p.CrashLoop {
		return "crash loop"
	}
	if p.State == "restarting" {
		return "backoff"
	}
	return p.State
}

// cpuText renders cumulative CPU the way Task Manager does: honest "-" when
// the platform cannot sample, otherwise the summed user+sys milliseconds.
func cpuText(res resourceUsage) string {
	if !res.Supported {
		return "-"
	}
	totalMs := res.CPUUserMs + res.CPUSysMs
	if totalMs >= 1000 {
		return fmt.Sprintf("%.1fs", float64(totalMs)/1000.0)
	}
	return fmt.Sprintf("%dms", totalMs)
}

// memText renders resident memory humanized; unsupported reads "-".
func memText(res resourceUsage) string {
	if !res.Supported {
		return "-"
	}
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch b := res.MemoryBytes; {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%d KB", b/kb)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// formatUptime renders the environment uptime compactly.
func formatUptime(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
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

// clip bounds untrusted text into a detail or label budget.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max])
}
