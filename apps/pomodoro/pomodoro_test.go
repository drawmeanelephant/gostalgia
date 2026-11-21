package pomodoro

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gostalgia/sdk"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// notification is one captured notify/post call.
type notification struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

// harness models the app-private VFS partition and the notify route,
// with an injectable clock. Time moves via h.now, never by sleeping;
// only TestRunDrivesTransitionAndExits waits on the real ticker.
type harness struct {
	now      time.Time
	files    map[string][]byte
	notifies []notification
	deny     bool  // notify/post denied: simulates a missing grant
	readErr  error // if set, the next fs/read fails once with this error
	seq      atomic.Int64

	app      *Pomodoro
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
	p := inst.(*Pomodoro)
	setClock(p, func() time.Time { return h.now })
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := p.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = p
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
	case "notify/post":
		if h.deny {
			return errors.New("permission denied: missing capability \"notify\"")
		}
		var n notification
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		h.notifies = append(h.notifies, n)
		return reply(map[string]any{"posted": true})
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

func findField(v sdk.View, id string) *sdk.Field {
	for i := range v.Fields {
		if v.Fields[i].ID == id {
			return &v.Fields[i]
		}
	}
	return nil
}

func findMeter(v sdk.View, id string) *sdk.Meter {
	for i := range v.Meters {
		if v.Meters[i].ID == id {
			return &v.Meters[i]
		}
	}
	return nil
}

func loadDisk(t *testing.T, h *harness) diskState {
	t.Helper()
	data, ok := h.files[statePath]
	if !ok {
		t.Fatal("state file was not persisted")
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("persisted state does not decode: %v", err)
	}
	return st
}

// --- pure domain tests ---

func TestConfigValidateBounds(t *testing.T) {
	good := defaultConfig()
	if err := good.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
	for _, c := range []Config{
		{WorkMin: 4, ShortMin: 5, LongMin: 15, Cycles: 4},
		{WorkMin: 121, ShortMin: 5, LongMin: 15, Cycles: 4},
		{WorkMin: 25, ShortMin: 0, LongMin: 15, Cycles: 4},
		{WorkMin: 25, ShortMin: 5, LongMin: 31, Cycles: 4},
		{WorkMin: 25, ShortMin: 5, LongMin: 15, Cycles: 0},
		{WorkMin: 25, ShortMin: 5, LongMin: 15, Cycles: 9},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("config %+v should be rejected", c)
		}
	}
	for _, c := range []Config{
		{WorkMin: 5, ShortMin: 1, LongMin: 1, Cycles: 1},
		{WorkMin: 120, ShortMin: 30, LongMin: 30, Cycles: 8},
	} {
		if err := c.Validate(); err != nil {
			t.Errorf("boundary config %+v should validate: %v", c, err)
		}
	}
}

func TestClockFormat(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                "0:00",
		-time.Minute:     "0:00",
		time.Millisecond: "0:01",
		59 * time.Second: "0:59",
		time.Minute:      "1:00",
		90 * time.Second: "1:30",
		25 * time.Minute: "25:00",
		time.Hour:        "1:00:00",
		2*time.Hour + 5*time.Minute + 7*time.Second: "2:05:07",
	} {
		if got := clock(d); got != want {
			t.Errorf("clock(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestTimerRemainingFromAnchor(t *testing.T) {
	running := Timer{Phase: PhaseWork, Running: true, EndsAt: t0.Add(25 * time.Minute)}
	if got := running.remaining(t0); got != 25*time.Minute {
		t.Errorf("running remaining = %v, want 25m", got)
	}
	if got := running.remaining(t0.Add(30 * time.Minute)); got != 0 {
		t.Errorf("overdue remaining = %v, want 0", got)
	}
	paused := Timer{Phase: PhaseWork, Remaining: 10 * time.Minute}
	if got := paused.remaining(t0.Add(time.Hour)); got != 10*time.Minute {
		t.Errorf("paused remaining drifted: %v, want frozen 10m", got)
	}
	if got := (Timer{Phase: PhaseIdle}).remaining(t0); got != 0 {
		t.Errorf("idle remaining = %v, want 0", got)
	}
}

// --- app-level tests through the real SDK presentation handlers ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "pomodoro" {
		t.Errorf("manifest = %s/%s", m.ID, m.Entrypoint)
	}
	want := map[string]bool{"ipc": true, "fs.read": true, "fs.write": true, "notify": true}
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

func TestIdleViewShape(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 2)
	if v.State != sdk.ViewReady {
		t.Fatalf("idle view state = %q", v.State)
	}
	timer := findItem(v, "timer")
	if timer == nil || timer.Label != "Ready" {
		t.Fatalf("timer item = %+v", timer)
	}
	for _, id := range []string{"work_min", "short_min", "long_min", "cycles"} {
		if findField(v, id) == nil {
			t.Errorf("idle view missing field %s", id)
		}
	}
	if a := findAction(v, "start"); a == nil || a.Disabled {
		t.Error("start should be enabled while idle")
	}
	for _, id := range []string{"pause", "skip", "reset"} {
		if a := findAction(v, id); a == nil || !a.Disabled {
			t.Errorf("%s should be disabled while idle", id)
		}
	}
	if len(v.Blocks) != 0 {
		t.Error("idle view carries no countdown block")
	}
	if !strings.Contains(v.Status, "ready") {
		t.Errorf("idle status = %q", v.Status)
	}
}

func TestStartPauseResumeCountdown(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 2, "start", "", map[string]string{
		"work_min": "25", "short_min": "5", "long_min": "15", "cycles": "4",
	})
	if got := findItem(v, "timer").Detail; got != "25:00 remaining" {
		t.Fatalf("countdown at start = %q, want 25:00 remaining", got)
	}
	if v.Title != "Pomodoro — Work 25:00" {
		t.Errorf("title = %q", v.Title)
	}
	if len(v.Fields) != 0 {
		t.Error("fields must hide once the timer is running")
	}
	if len(v.Blocks) != 1 || !strings.Contains(v.Blocks[0].Text, "###") {
		t.Fatalf("countdown block missing: %+v", v.Blocks)
	}
	if a := findAction(v, "start"); a == nil || !a.Disabled {
		t.Error("start should be disabled while running")
	}
	if a := findAction(v, "pause"); a == nil || a.Disabled {
		t.Error("pause should be enabled while running")
	}

	h.advance(10 * time.Minute)
	v = h.view(t, 2)
	if got := findItem(v, "timer").Detail; got != "15:00 remaining" {
		t.Fatalf("countdown after 10m = %q, want 15:00 remaining", got)
	}
	if m := findMeter(v, "m_phase"); m == nil || m.Value < 0.39 || m.Value > 0.41 {
		t.Errorf("phase meter at half = %+v", m)
	}

	v = h.act(t, 2, "pause", "", nil)
	if got := findItem(v, "timer").Detail; got != "paused at 15:00" {
		t.Fatalf("paused detail = %q, want paused at 15:00", got)
	}
	h.advance(10 * time.Minute) // paused time does not tick down
	h.app.tick(context.Background())
	v = h.view(t, 2)
	if got := findItem(v, "timer").Detail; got != "paused at 15:00" {
		t.Fatalf("paused drifted: %q", got)
	}
	if h.app.timer.Phase != PhaseWork {
		t.Error("tick must not transition a paused timer")
	}
	if a := findAction(v, "start"); a == nil || a.Disabled || a.Label != "Resume" {
		t.Error("start should offer Resume while paused")
	}

	v = h.act(t, 2, "start", "", nil)
	if got := findItem(v, "timer").Detail; got != "15:00 remaining" {
		t.Fatalf("resume detail = %q, want 15:00 remaining", got)
	}
	h.advance(15 * time.Minute)
	h.app.tick(context.Background())
	if h.app.timer.Phase != PhaseShort {
		t.Fatalf("phase after resumed work = %s, want short", h.app.timer.Phase)
	}
}

func TestWorkToShortBreakNotifies(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", map[string]string{"work_min": "25"})
	h.advance(25 * time.Minute)
	h.app.tick(context.Background())

	if h.app.timer.Phase != PhaseShort || !h.app.timer.Running {
		t.Fatalf("after work: phase=%s running=%v, want running short", h.app.timer.Phase, h.app.timer.Running)
	}
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %+v, want one phase-end alert", h.notifies)
	}
	n := h.notifies[0]
	if n.Severity != "info" || n.Title != "Pomodoro" || !strings.Contains(n.Body, "short break") {
		t.Errorf("notification = %+v", n)
	}
	if h.app.cyclePos != 1 || h.app.workDone != 1 {
		t.Errorf("cyclePos=%d workDone=%d, want 1/1", h.app.cyclePos, h.app.workDone)
	}
	if len(h.app.log) != 1 || h.app.log[0].Phase != PhaseWork || h.app.log[0].Skipped {
		t.Fatalf("log = %+v", h.app.log)
	}
	v := h.view(t, 2)
	if got := findItem(v, "timer"); got.Label != "Short break" || got.Detail != "5:00 remaining" {
		t.Errorf("timer item = %+v", got)
	}
	if !strings.Contains(v.Status, "work session done") {
		t.Errorf("status = %q", v.Status)
	}
	if findItem(v, "log_0") == nil {
		t.Error("finished work should appear in the session items")
	}
}

func TestFullCycleReachesLongBreak(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", map[string]string{
		"work_min": "25", "short_min": "5", "long_min": "15", "cycles": "2",
	})
	h.advance(25 * time.Minute)
	h.app.tick(context.Background()) // work 1 -> short
	if h.app.timer.Phase != PhaseShort {
		t.Fatalf("phase = %s, want short", h.app.timer.Phase)
	}
	h.advance(5 * time.Minute)
	h.app.tick(context.Background()) // short -> work 2
	if h.app.timer.Phase != PhaseWork || h.app.cyclePos != 1 {
		t.Fatalf("phase=%s cyclePos=%d, want work/1", h.app.timer.Phase, h.app.cyclePos)
	}
	h.advance(25 * time.Minute)
	h.app.tick(context.Background()) // work 2 -> long (cycle complete)
	if h.app.timer.Phase != PhaseLong {
		t.Fatalf("phase = %s, want long after 2 sessions", h.app.timer.Phase)
	}
	if h.app.workDone != 2 {
		t.Errorf("workDone = %d, want 2", h.app.workDone)
	}
	h.advance(15 * time.Minute)
	h.app.tick(context.Background()) // long -> work, new cycle
	if h.app.timer.Phase != PhaseWork || h.app.cyclePos != 0 {
		t.Fatalf("phase=%s cyclePos=%d, want work/0 for the new cycle", h.app.timer.Phase, h.app.cyclePos)
	}
	wantPhases := []Phase{PhaseWork, PhaseShort, PhaseWork, PhaseLong}
	if len(h.app.log) != len(wantPhases) {
		t.Fatalf("log = %+v", h.app.log)
	}
	for i, want := range wantPhases {
		if h.app.log[i].Phase != want {
			t.Errorf("log[%d].Phase = %s, want %s", i, h.app.log[i].Phase, want)
		}
	}
	if len(h.notifies) != 4 {
		t.Errorf("notifies = %d, want one per transition", len(h.notifies))
	}
}

func TestSkipMarksLogAndMovesOn(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", map[string]string{"work_min": "25", "cycles": "4"})
	v := h.act(t, 2, "skip", "", nil)
	if got := findItem(v, "timer"); got.Label != "Short break" {
		t.Fatalf("after skip: timer = %+v", got)
	}
	if len(h.notifies) != 0 {
		t.Error("a user-initiated skip should not post a notification")
	}
	if !strings.Contains(v.Status, "skipped") {
		t.Errorf("status after skip = %q", v.Status)
	}
	if len(h.app.log) != 1 || !h.app.log[0].Skipped {
		t.Fatalf("log = %+v, want one skipped work entry", h.app.log)
	}
	if h.app.workDone != 0 {
		t.Error("a skipped work session is not a completed focus session")
	}
	if h.app.cyclePos != 1 {
		t.Error("skip still advances the cycle position")
	}
	v = h.act(t, 2, "skip", "", nil) // skip the break too
	if got := findItem(v, "timer"); got.Label != "Work" {
		t.Fatalf("after skipping break: timer = %+v", got)
	}
}

func TestResetReturnsToReady(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", nil)
	h.advance(25 * time.Minute)
	h.app.tick(context.Background()) // completes work -> short, workDone=1
	v := h.act(t, 2, "reset", "", nil)
	if got := findItem(v, "timer"); got.Label != "Ready" {
		t.Fatalf("after reset: timer = %+v", got)
	}
	if h.app.cyclePos != 0 || h.app.timer.Phase != PhaseIdle {
		t.Error("reset should clear the phase and cycle position")
	}
	if h.app.workDone != 1 || len(h.app.log) != 1 {
		t.Error("reset must keep history: workDone and the log survive")
	}
	if len(v.Fields) != 4 {
		t.Error("fields return once idle")
	}
	if _, err := h.tryAct(2, "reset", "", nil); err == nil {
		// reset is Disabled in the idle view, so the contract rejects it.
		t.Error("reset while idle should be a contract rejection")
	}
}

func TestConfigValidationOnStart(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 2, "start", "", map[string]string{"work_min": "3"})
	if h.app.timer.Phase != PhaseIdle {
		t.Fatal("out-of-bounds work length must not start the timer")
	}
	if !strings.Contains(v.Status, "5-120") {
		t.Errorf("status = %q, want the bound in the message", v.Status)
	}
	v = h.act(t, 2, "start", "", map[string]string{"work_min": "soon"})
	if !strings.Contains(v.Status, "whole number") {
		t.Errorf("status = %q, want a parse message", v.Status)
	}
	if h.app.timer.Phase != PhaseIdle {
		t.Error("garbage input must not start the timer")
	}
}

func TestConfigureRouteAppliesToNextPhase(t *testing.T) {
	h := newHarness(t)
	h.route(t, "start", map[string]any{"work_min": 10, "cycles": 2}, nil)

	// A mid-run change does not move the running phase's anchor.
	h.route(t, "configure", map[string]any{"short_min": 7}, nil)
	if h.app.config.ShortMin != 7 {
		t.Fatal("configure did not apply")
	}
	if !h.app.timer.EndsAt.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("running anchor moved: ends_at = %v", h.app.timer.EndsAt)
	}
	h.advance(10 * time.Minute)
	h.app.tick(context.Background())
	if got := h.app.timer.EndsAt.Sub(h.now); got != 7*time.Minute {
		t.Errorf("next phase length = %v, want 7m from the new config", got)
	}

	// Out-of-bounds changes are honest route errors.
	hd := h.handlers["configure"]
	raw, _ := json.Marshal(map[string]any{"long_min": 45})
	if _, err := hd(context.Background(), raw); err == nil {
		t.Error("configure with long_min=45 should fail")
	}
}

func TestNotifyDegradesToStatusLine(t *testing.T) {
	h := newHarness(t)
	h.deny = true
	h.act(t, 2, "start", "", map[string]string{"work_min": "25"})
	h.advance(25 * time.Minute)
	h.app.tick(context.Background())

	if !h.app.notifyDown {
		t.Error("denied notify/post should set notifyDown")
	}
	if h.app.timer.Phase != PhaseShort {
		t.Fatal("transition still happens without the grant")
	}
	v := h.view(t, 2)
	if !strings.Contains(v.Status, "alerts unavailable") {
		t.Errorf("status should carry the degradation honestly: %q", v.Status)
	}
	if v.State != sdk.ViewReady {
		t.Errorf("view state = %q after notify denial", v.State)
	}

	// The marker persists in the standing status line until the grant works.
	h.act(t, 2, "pause", "", nil)
	v = h.view(t, 2)
	if !strings.Contains(v.Status, "alerts unavailable") {
		t.Errorf("standing status should keep the marker: %q", v.Status)
	}

	h.deny = false
	h.app.tick(context.Background()) // paused: no transition
	h.act(t, 2, "start", "", nil)    // resume
	h.advance(7 * time.Minute)
	h.app.tick(context.Background()) // break ends -> work, posts again
	if len(h.notifies) == 0 {
		t.Error("restored grant should resume posting")
	}
	if h.app.notifyDown {
		t.Error("notifyDown should clear on success")
	}
	v = h.view(t, 2)
	if strings.Contains(v.Status, "alerts unavailable") {
		t.Errorf("marker should clear once notify works: %q", v.Status)
	}
}

func TestRunDrivesTransitionAndExits(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", map[string]string{"work_min": "5"})
	h.advance(5*time.Minute + time.Second) // past the anchor

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.app.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		h.app.mu.Lock()
		phase := h.app.timer.Phase
		h.app.mu.Unlock()
		if phase == PhaseShort {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run did not fire the transition within 3s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel — goroutine leak")
	}
}

func TestRelaunchLandsPausedWithHonestRemainder(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", map[string]string{"work_min": "25"})
	h.advance(10 * time.Minute)
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The file pins the running anchor: ends_at plus the save timestamp.
	st := loadDisk(t, h)
	if !st.Timer.Running || !st.Timer.EndsAt.Equal(t0.Add(25*time.Minute)) {
		t.Fatalf("persisted timer = %+v, want running work ending t0+25m", st.Timer)
	}
	if !st.Timer.SavedAt.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("saved_at = %v, want t0+10m", st.Timer.SavedAt)
	}

	// Five more minutes pass with the app off; a fresh instance reads the
	// same files. The timer did not run while down: it relands paused with
	// the 15m it had at the last save, not the 10m wall-clock truth.
	h2 := newHarness(t)
	h2.files = h.files
	h2.now = t0.Add(15 * time.Minute)
	v := h2.view(t, 2)
	timer := findItem(v, "timer")
	if timer.Label != "Work" || timer.Detail != "paused at 15:00" {
		t.Fatalf("relaunched timer = %+v, want paused at 15:00", timer)
	}
	if h2.app.timer.Running {
		t.Error("a relaunched timer must not resume running on its own")
	}
	if !strings.Contains(v.Status, "did not run while the app was off") {
		t.Errorf("status = %q, want the honest relaunch note", v.Status)
	}
	if len(h2.notifies) != 0 {
		t.Error("relaunch posts no notifications")
	}

	// Resuming picks up the honest remainder and completes on the tick.
	h2.act(t, 2, "start", "", nil)
	h2.advance(15 * time.Minute)
	h2.app.tick(context.Background())
	if h2.app.timer.Phase != PhaseShort {
		t.Fatalf("phase = %s, want short after resumed work ran out", h2.app.timer.Phase)
	}
	if len(h2.notifies) != 1 {
		t.Errorf("notifies = %+v", h2.notifies)
	}
}

func TestRelaunchAfterPhaseRanOut(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", map[string]string{"work_min": "25"})
	h.advance(30 * time.Minute) // past the end, no tick ran
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	h2 := newHarness(t)
	h2.files = h.files
	h2.now = t0.Add(35 * time.Minute)
	v := h2.view(t, 2)
	if got := findItem(v, "timer").Detail; got != "paused at 0:00" {
		t.Fatalf("ran-out timer = %q, want paused at 0:00", got)
	}
	if !strings.Contains(v.Status, "reached 0:00") {
		t.Errorf("status = %q", v.Status)
	}
	if h2.app.workDone != 0 {
		t.Error("a phase that lapsed while off is not credited as done")
	}
	// Resuming a 0:00 phase lets the pending transition fire honestly.
	h2.act(t, 2, "start", "", nil)
	h2.app.tick(context.Background())
	if h2.app.timer.Phase != PhaseShort || h2.app.workDone != 1 {
		t.Fatalf("phase=%s workDone=%d after resume+tick", h2.app.timer.Phase, h2.app.workDone)
	}
}

func TestPausedTimerSurvivesRelaunch(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", nil)
	h.advance(5 * time.Minute)
	h.act(t, 2, "pause", "", nil)
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	h2 := newHarness(t)
	h2.files = h.files
	h2.now = t0.Add(3 * time.Hour)
	v := h2.view(t, 2)
	if got := findItem(v, "timer").Detail; got != "paused at 20:00" {
		t.Fatalf("restored pause = %q, want paused at 20:00", got)
	}
	if !strings.Contains(v.Status, "restored paused") {
		t.Errorf("status = %q", v.Status)
	}
}

func TestConfigAndLogSurviveRelaunch(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", map[string]string{"work_min": "10", "cycles": "2"})
	h.advance(10 * time.Minute)
	h.app.tick(context.Background())
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	h2 := newHarness(t)
	h2.files = h.files
	h2.now = t0.Add(time.Hour)
	h2.view(t, 2)
	if h2.app.config.WorkMin != 10 || h2.app.config.Cycles != 2 {
		t.Errorf("config = %+v, want persisted 10m/2", h2.app.config)
	}
	if h2.app.workDone != 1 || len(h2.app.log) != 1 {
		t.Error("history did not survive relaunch")
	}
}

func TestCorruptSaveStartsFresh(t *testing.T) {
	h := newHarness(t)
	h.files[statePath] = []byte("{not json")
	v := h.view(t, 2)
	if !strings.Contains(v.Status, "starting fresh") {
		t.Errorf("corrupt save status = %q", v.Status)
	}
	if h.app.timer.Phase != PhaseIdle {
		t.Error("corrupt save should land idle")
	}
}

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(2, "launch_missiles", "", nil); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

func TestRouteHandlers(t *testing.T) {
	h := newHarness(t)
	var startOut struct {
		Phase   Phase `json:"phase"`
		Running bool  `json:"running"`
	}
	h.route(t, "start", map[string]any{"work_min": 20}, &startOut)
	if !startOut.Running || startOut.Phase != PhaseWork {
		t.Fatalf("start route = %+v", startOut)
	}
	var st struct {
		Phase        Phase `json:"phase"`
		Running      bool  `json:"running"`
		RemainingSec int   `json:"remaining_sec"`
		Session      int   `json:"session"`
	}
	h.route(t, "state", nil, &st)
	if st.Phase != PhaseWork || !st.Running || st.RemainingSec != 20*60 || st.Session != 1 {
		t.Errorf("state = %+v", st)
	}
	var pauseOut struct {
		Paused       bool `json:"paused"`
		RemainingSec int  `json:"remaining_sec"`
	}
	h.advance(5 * time.Minute)
	h.route(t, "pause", nil, &pauseOut)
	if !pauseOut.Paused || pauseOut.RemainingSec != 15*60 {
		t.Errorf("pause = %+v", pauseOut)
	}
	var skipOut struct {
		Phase Phase `json:"phase"`
	}
	h.route(t, "skip", nil, &skipOut)
	if skipOut.Phase != PhaseShort {
		t.Errorf("skip = %+v", skipOut)
	}
	var resetOut struct {
		Phase    Phase `json:"phase"`
		WorkDone int   `json:"work_done"`
	}
	h.route(t, "reset", nil, &resetOut)
	if resetOut.Phase != PhaseIdle {
		t.Errorf("reset = %+v", resetOut)
	}

	// Route-level errors are honest IPC errors.
	hd := h.handlers["pause"]
	if _, err := hd(context.Background(), nil); err == nil {
		t.Error("pause while idle should be an IPC error")
	}
}

func TestV1FallbackKeepsCountdown(t *testing.T) {
	h := newHarness(t)
	h.act(t, 1, "start", "", map[string]string{"work_min": "25"})
	h.advance(time.Minute)
	v := h.view(t, 1)
	if len(v.Blocks) != 0 || len(v.Meters) != 0 || v.Grid != nil {
		t.Fatal("v1 view must not carry blocks, meters, or grid")
	}
	if got := findItem(v, "timer").Detail; got != "24:00 remaining" {
		t.Errorf("v1 countdown = %q", got)
	}
	if findField(v, "work_min") != nil {
		t.Error("fields hidden while running at v1 too")
	}
}

// A transient fs/read failure (a canceled call racing the first view, a
// host I/O error, the read-size bound) is not "no save yet": the app must
// say so honestly, retry the load, and never let the next change overwrite
// the saved timer.
func TestTransientReadErrorNeverOverwritesTimer(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "start", "", nil) // running work session, persisted

	h2 := newHarness(t)
	h2.files = h.files
	h2.readErr = context.Canceled // first fs/read fails once, transiently

	v := h2.view(t, 2)
	if v.State != sdk.ViewError || v.Error == "" {
		t.Fatalf("transient read failure view = %+v, want an honest error", v)
	}

	// The retry loads the real file: the seeded running session relands
	// paused, so skipping it advances to a break — a fresh idle timer
	// would have no skip to offer.
	v = h2.act(t, 2, "skip", "", nil)
	var st diskState
	if err := json.Unmarshal(h2.files[statePath], &st); err != nil {
		t.Fatalf("post-change save is not valid json: %v", err)
	}
	if st.Timer.Phase != PhaseShort {
		t.Fatalf("persisted timer after transient failure = %+v, want the skipped work session's break", st.Timer)
	}
	if len(st.Log) != 1 || !st.Log[0].Skipped {
		t.Fatalf("log after transient failure = %+v, want the seeded session's skip entry", st.Log)
	}
}
