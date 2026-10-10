// Package pomodoro is Gostalgia's focus timer: work, short-break, and
// long-break phases with a live countdown, cycle counting, and a
// notify/post alert when each phase ends. The countdown is derived from a
// pinned ends_at anchor and rendered through the ordinary view snapshot.
// Mutations push the version-3 invalidate hint so an open view refreshes
// immediately instead of waiting for the shell's poll cadence; the
// countdown itself is still derived fresh on every fetch.
//
// Timer state and a bounded session log persist in the app-private
// partition (/apps/data/com.gostalgia.pomodoro/state.json). A timer never
// pretends to run while the app is down: a phase that was in flight at
// shutdown relands paused with the remainder it had at the last save, so
// the user chooses whether to resume, skip, or reset. Phase-end alerts
// degrade to the status line when the notify grant is absent or revoked.
package pomodoro

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Pomodoro.
const ID = "com.gostalgia.pomodoro"

// statePath is the single persisted document inside the app-private
// partition. fs/save stages and renames atomically, so a crash mid-write
// leaves the previous state intact.
const statePath = "/apps/data/com.gostalgia.pomodoro/state.json"

const stateVersion = 1

// tickInterval is how often the running timer checks its anchor. The
// countdown itself is computed per view request; the tick only fires the
// phase-end transition, so sub-second precision is wasted here.
const tickInterval = 500 * time.Millisecond

// lowerLabels name phases inside status sentences and notifications.
var lowerLabels = map[Phase]string{
	PhaseWork:  "work session",
	PhaseShort: "short break",
	PhaseLong:  "long break",
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

// diskState is the persisted document: config, the timer anchor, cycle
// bookkeeping, and the bounded session log.
type diskState struct {
	Version  int        `json:"version"`
	Config   Config     `json:"config"`
	Timer    Timer      `json:"timer"`
	CyclePos int        `json:"cycle_pos"`
	WorkDone int        `json:"work_done"`
	Log      []LogEntry `json:"log,omitempty"`
}

// Pomodoro is the running in-process instance. All state is guarded by mu.
type Pomodoro struct {
	app *sdk.Context
	now func() time.Time

	mu         sync.Mutex
	loaded     bool
	config     Config
	timer      Timer
	cyclePos   int // work phases ended since the last long break
	workDone   int // fully completed (not skipped) work phases, all time
	log        []LogEntry
	statusNote string // transient feedback line; cleared on each action
	lastError  string

	notifyDown bool // notify/post failed; logged once, stays quiet
}

// Factory constructs a fresh, uninitialized Pomodoro instance.
func Factory() (sdk.Instance, error) { return &Pomodoro{now: time.Now}, nil }

// setClock injects a deterministic clock for tests.
func setClock(p *Pomodoro, now func() time.Time) { p.now = now }

// Init registers routes and the presentation callbacks. State loads
// lazily on first use so Init stays prompt.
func (p *Pomodoro) Init(app *sdk.Context) error {
	p.app = app
	if p.now == nil {
		p.now = time.Now
	}
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"state", p.stateRoute},
		{"start", p.startRoute},
		{"pause", p.pauseRoute},
		{"skip", p.skipRoute},
		{"reset", p.resetRoute},
		{"configure", p.configureRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(p.view, p.act)
}

// Run checks the timer anchor on a fixed cadence until canceled. The
// countdown the user sees is always derived in the view; the tick only
// performs the phase-end transition and its bookkeeping.
func (p *Pomodoro) Run(ctx context.Context) error {
	p.app.Log.Info("pomodoro running")
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// Stop flushes the final state. A running phase is written with its
// ends_at and saved_at anchors so the next launch can land it paused
// with an honest remainder.
func (p *Pomodoro) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		if err := p.persistLocked(ctx); err != nil && p.app.Log != nil {
			p.app.Log.Warn("pomodoro: final save failed", "err", err)
		}
	}
	return nil
}

// tick performs at most one phase transition: when the anchor has passed,
// the finished phase is logged and the next phase starts running from
// now. One transition per tick means a long-dormant tick never cascades
// through the whole cycle firing stale alerts.
func (p *Pomodoro) tick(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		p.lastError = err.Error()
		return
	}
	now := p.now()
	if p.timer.Running && p.timer.Phase != PhaseIdle && !now.Before(p.timer.EndsAt) {
		p.completePhaseLocked(ctx, now, false, true)
	}
}

// invalidateLocked nudges open views to refetch now rather than wait for
// the poll cadence. A mutation reached through an IPC route or the ticker
// has no action reply to carry it; the version-3 hint is best-effort and
// its error is deliberately ignored.
func (p *Pomodoro) invalidateLocked(ctx context.Context) {
	if p.app != nil {
		_ = p.app.Invalidate(ctx)
	}
}

// ensureLoadedLocked reads and decodes state.json once. A missing file
// starts fresh; an unreadable one starts fresh with an honest note. A
// persisted non-idle phase always relands paused: the timer does not run
// while the app is off, and the status line says so.
func (p *Pomodoro) ensureLoadedLocked(ctx context.Context) error {
	if p.loaded {
		return nil
	}
	p.loaded = true
	p.config = defaultConfig()
	p.timer = Timer{Phase: PhaseIdle}

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := p.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		return nil // no save yet: fresh timer, nothing to report
	}
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		p.statusNote = "previous save unreadable; starting fresh"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		p.statusNote = "previous save unreadable; starting fresh"
		return nil
	}
	if err := st.Config.Validate(); err == nil {
		p.config = st.Config
	} else {
		p.statusNote = "saved lengths invalid; using defaults"
	}
	p.cyclePos = st.CyclePos
	if p.cyclePos < 0 || p.cyclePos > p.config.Cycles {
		p.cyclePos = 0
	}
	if st.WorkDone > 0 {
		p.workDone = st.WorkDone
	}
	p.log = st.Log
	p.sanitizeTimerLocked(st.Timer)
	// Re-anchor the file so it records the paused truth, not a running
	// flag that cannot outlive the process.
	if err := p.persistLocked(ctx); err != nil && p.app.Log != nil {
		p.app.Log.Warn("pomodoro: re-anchor save failed", "err", err)
	}
	return nil
}

// sanitizeTimerLocked repairs the loaded timer: unknown phases collapse
// to idle, and a live phase relands paused with the remainder it had at
// the last save — clamped to zero if it already ran out while off.
func (p *Pomodoro) sanitizeTimerLocked(t Timer) {
	switch t.Phase {
	case PhaseWork, PhaseShort, PhaseLong:
	default:
		p.timer = Timer{Phase: PhaseIdle}
		return
	}
	p.timer = t
	// A restored remainder can never exceed the phase's own length:
	// anything beyond it is corrupt input, not usable state.
	maxRem := p.config.Length(t.Phase)
	if t.Running {
		rem := t.EndsAt.Sub(t.SavedAt)
		if rem < 0 {
			rem = 0
		}
		if rem > maxRem {
			rem = maxRem
		}
		p.timer.Running = false
		p.timer.EndsAt = time.Time{}
		p.timer.Remaining = rem
		if rem == 0 {
			p.statusNote = "the phase reached 0:00 while the app was off — resume or skip to move on"
		} else {
			p.statusNote = fmt.Sprintf("timer paused across restart — %s left; it did not run while the app was off", clock(rem))
		}
		return
	}
	if p.timer.Remaining < 0 {
		p.timer.Remaining = 0
	}
	if p.timer.Remaining > maxRem {
		p.timer.Remaining = maxRem
	}
	p.statusNote = fmt.Sprintf("timer restored paused — %s left; it did not run while the app was off", clock(p.timer.Remaining))
}

// persistLocked writes the whole state document atomically, keeping only
// the newest maxLogRetain log entries so the file stays bounded.
func (p *Pomodoro) persistLocked(ctx context.Context) error {
	if len(p.log) > maxLogRetain {
		p.log = append([]LogEntry(nil), p.log[len(p.log)-maxLogRetain:]...)
	}
	p.timer.SavedAt = p.now()
	data, err := json.Marshal(diskState{
		Version:  stateVersion,
		Config:   p.config,
		Timer:    p.timer,
		CyclePos: p.cyclePos,
		WorkDone: p.workDone,
		Log:      p.log,
	})
	if err != nil {
		return err
	}
	return p.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
}

// notifyLocked posts one phase-end alert through notify/post. The notify
// capability may be absent or revoked; failures are logged once and the
// flag suppresses retry noise until a call succeeds again.
func (p *Pomodoro) notifyLocked(ctx context.Context, body string) bool {
	err := p.app.Call(ctx, "notify/post", map[string]string{
		"severity": "info",
		"title":    "Pomodoro",
		"body":     body,
	}, nil)
	if err != nil {
		if !p.notifyDown && p.app.Log != nil {
			p.app.Log.Info("pomodoro: notify/post unavailable", "err", err)
		}
		p.notifyDown = true
		return false
	}
	p.notifyDown = false
	return true
}

// completePhaseLocked ends the current phase and starts the next one
// running. The finished phase is appended to the session log — marked
// skipped when the user jumped ahead — work phases count toward the
// cycle either way but only unskipped ones count as completed focus
// sessions. alert posts a notify/post for timer-driven transitions;
// user-initiated skips show in the status line instead.
func (p *Pomodoro) completePhaseLocked(ctx context.Context, now time.Time, skipped, alert bool) {
	finished := p.timer.Phase
	if finished == PhaseIdle {
		return
	}
	p.log = append(p.log, LogEntry{
		Phase:     finished,
		StartedAt: p.timer.StartedAt,
		EndedAt:   now,
		Skipped:   skipped,
	})

	var next Phase
	var nextText string
	switch finished {
	case PhaseWork:
		p.cyclePos++
		if !skipped {
			p.workDone++
		}
		if p.cyclePos >= p.config.Cycles {
			next = PhaseLong
			nextText = fmt.Sprintf("long break (%dm) started — cycle complete", p.config.LongMin)
		} else {
			next = PhaseShort
			nextText = fmt.Sprintf("short break (%dm) started", p.config.ShortMin)
		}
	case PhaseShort:
		next = PhaseWork
		nextText = fmt.Sprintf("back to work (%dm)", p.config.WorkMin)
	case PhaseLong:
		p.cyclePos = 0
		next = PhaseWork
		nextText = fmt.Sprintf("new cycle — back to work (%dm)", p.config.WorkMin)
	}

	p.timer = Timer{
		Phase:     next,
		Running:   true,
		EndsAt:    now.Add(p.config.Length(next)),
		StartedAt: now,
	}

	verb := "done"
	if skipped {
		verb = "skipped"
	}
	msg := fmt.Sprintf("%s %s — %s", lowerLabels[finished], verb, nextText)
	if alert {
		// The post sets notifyDown on failure; the standing status
		// suffix carries "alerts unavailable" until a post succeeds.
		p.notifyLocked(ctx, msg)
	}
	p.statusNote = msg
	if err := p.persistLocked(ctx); err != nil {
		p.lastError = fmt.Sprintf("save failed: %v", err)
	}
	p.invalidateLocked(ctx)
}

// --- mutations: error-returning, shared by presentation and routes ---

// startLocked begins a work phase from idle, applying new lengths first
// when supplied, or resumes a paused phase by re-pinning ends_at at
// now + remaining. A paused phase at 0:00 resumes into an immediately
// expiring anchor, so its pending transition fires on the next tick.
func (p *Pomodoro) startLocked(ctx context.Context, cfg *Config) error {
	now := p.now()
	switch {
	case p.timer.Phase == PhaseIdle:
		if cfg != nil {
			if err := cfg.Validate(); err != nil {
				return err
			}
			p.config = *cfg
		}
		work := p.config.Length(PhaseWork)
		p.timer = Timer{
			Phase:     PhaseWork,
			Running:   true,
			EndsAt:    now.Add(work),
			StartedAt: now,
		}
		p.statusNote = fmt.Sprintf("work session started — %s of focus", clock(work))
	case !p.timer.Running:
		rem := p.timer.Remaining
		if rem < 0 {
			rem = 0
		}
		p.timer.Running = true
		p.timer.EndsAt = now.Add(rem)
		p.timer.Remaining = 0
		p.statusNote = fmt.Sprintf("resumed — %s left on %s", clock(rem), lowerLabels[p.timer.Phase])
	default:
		return fmt.Errorf("timer is already running")
	}
	err := p.persistLocked(ctx)
	if err == nil {
		p.invalidateLocked(ctx)
	}
	return err
}

// pauseLocked freezes the countdown by converting the live anchor into a
// stored remainder.
func (p *Pomodoro) pauseLocked(ctx context.Context) error {
	if !p.timer.Running {
		return fmt.Errorf("timer is not running")
	}
	rem := p.timer.EndsAt.Sub(p.now())
	if rem < 0 {
		rem = 0
	}
	p.timer.Running = false
	p.timer.EndsAt = time.Time{}
	p.timer.Remaining = rem
	p.statusNote = fmt.Sprintf("paused — %s left on %s", clock(rem), lowerLabels[p.timer.Phase])
	err := p.persistLocked(ctx)
	if err == nil {
		p.invalidateLocked(ctx)
	}
	return err
}

// resetLocked abandons the current phase back to ready and restarts the
// cycle count. The session log and all-time completion count are history
// and survive a reset.
func (p *Pomodoro) resetLocked(ctx context.Context) error {
	if p.timer.Phase == PhaseIdle {
		return fmt.Errorf("nothing to reset")
	}
	p.timer = Timer{Phase: PhaseIdle}
	p.cyclePos = 0
	p.statusNote = fmt.Sprintf("reset — %d sessions logged, %d focus sessions done", len(p.log), p.workDone)
	err := p.persistLocked(ctx)
	if err == nil {
		p.invalidateLocked(ctx)
	}
	return err
}

// skipLocked ends the current phase early; the log marks it skipped and
// the transition shows in the status line rather than a notification.
func (p *Pomodoro) skipLocked(ctx context.Context) error {
	if p.timer.Phase == PhaseIdle {
		return fmt.Errorf("no running phase to skip")
	}
	p.completePhaseLocked(ctx, p.now(), true, false)
	return nil
}

// configureLocked merges optional overrides into the stored lengths and
// validates the result. Changes apply to phases started after the call;
// a running phase keeps the anchor it began with.
func (p *Pomodoro) configureLocked(ctx context.Context, work, short, long, cycles *int) error {
	c := p.config
	if work != nil {
		c.WorkMin = *work
	}
	if short != nil {
		c.ShortMin = *short
	}
	if long != nil {
		c.LongMin = *long
	}
	if cycles != nil {
		c.Cycles = *cycles
	}
	if err := c.Validate(); err != nil {
		return err
	}
	p.config = c
	p.statusNote = "lengths updated — they apply from the next phase"
	err := p.persistLocked(ctx)
	if err == nil {
		p.invalidateLocked(ctx)
	}
	return err
}

// configFromFieldsLocked turns presentation field values into a Config.
// Blank fields keep their stored values; all-blank input yields nil so
// start can tell "no lengths given" from "lengths given".
func (p *Pomodoro) configFromFieldsLocked(values map[string]string) (*Config, error) {
	if len(values) == 0 {
		return nil, nil
	}
	c := p.config
	touched := false
	for _, f := range []struct {
		id  string
		dst *int
	}{
		{"work_min", &c.WorkMin},
		{"short_min", &c.ShortMin},
		{"long_min", &c.LongMin},
		{"cycles", &c.Cycles},
	} {
		raw, ok := values[f.id]
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s must be a whole number, got %q", f.id, raw)
		}
		*f.dst = n
		touched = true
	}
	if !touched {
		return nil, nil
	}
	return &c, nil
}

// --- presentation ---

func (p *Pomodoro) view(ctx context.Context) (sdk.View, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Pomodoro", State: sdk.ViewError, Error: err.Error()}, nil
	}
	return p.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (p *Pomodoro) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Pomodoro", State: sdk.ViewError, Error: err.Error()}, nil
	}
	p.lastError = ""
	p.statusNote = ""

	var err error
	switch req.Action {
	case "start":
		var cfg *Config
		cfg, err = p.configFromFieldsLocked(req.Values)
		if err == nil {
			err = p.startLocked(ctx, cfg)
		}
	case "pause":
		err = p.pauseLocked(ctx)
	case "skip":
		err = p.skipLocked(ctx)
	case "reset":
		err = p.resetLocked(ctx)
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		// Domain rejections land in the status line, not the error banner:
		// the timer keeps rendering and every valid action stays usable.
		p.statusNote = err.Error()
	}
	return p.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

// sessionLocked is the 1-based session number inside the current cycle.
func (p *Pomodoro) sessionLocked() int {
	s := p.cyclePos
	if p.timer.Phase == PhaseWork {
		s++
	}
	if s < 1 {
		s = 1
	}
	if s > p.config.Cycles {
		s = p.config.Cycles
	}
	return s
}

// viewLocked builds the snapshot at the negotiated presentation version.
// The countdown is computed fresh from the anchor on every request, so
// the shell's periodic polling sees real remaining time.
func (p *Pomodoro) viewLocked(version int) sdk.View {
	now := p.now()
	phase := p.timer.Phase
	rem := p.timer.remaining(now)

	title := "Pomodoro"
	if phase != PhaseIdle {
		title = fmt.Sprintf("Pomodoro — %s %s", phaseLabels[phase], clock(rem))
	}

	timerDetail := "no timer running"
	if phase != PhaseIdle {
		if p.timer.Running {
			timerDetail = clock(rem) + " remaining"
		} else {
			timerDetail = "paused at " + clock(rem)
		}
	}
	items := []sdk.Item{
		{ID: "timer", Label: phaseLabels[phase], Detail: timerDetail},
		{ID: "cycle", Label: "Cycle", Detail: fmt.Sprintf("session %d of %d · %d focus sessions logged",
			p.sessionLocked(), p.config.Cycles, p.workDone)},
	}
	// The newest sessions list first; the log stays bounded on disk and
	// only a short tail ever renders.
	shown := 0
	for i := len(p.log) - 1; i >= 0 && shown < 5; i-- {
		e := p.log[i]
		how := "completed"
		if e.Skipped {
			how = "skipped"
		}
		items = append(items, sdk.Item{
			ID:     fmt.Sprintf("log_%d", shown),
			Label:  phaseLabels[e.Phase],
			Detail: fmt.Sprintf("%s · ended %s ago", how, formatAge(now.Sub(e.EndedAt))),
		})
		shown++
	}

	startLabel := "Start"
	if phase != PhaseIdle {
		startLabel = "Resume"
	}

	v := sdk.View{
		Title: title,
		State: sdk.ViewReady,
		Items: items,
		Actions: []sdk.Action{
			{ID: "start", Label: startLabel, Disabled: p.timer.Running},
			{ID: "pause", Label: "Pause", Disabled: !p.timer.Running},
			{ID: "skip", Label: "Skip", Disabled: phase == PhaseIdle},
			{ID: "reset", Label: "Reset", Disabled: phase == PhaseIdle},
		},
		Status: p.statusLocked(now),
		Error:  p.lastError,
	}
	if phase == PhaseIdle {
		v.Fields = []sdk.Field{
			{ID: "work_min", Label: "Work minutes (5-120)", Value: strconv.Itoa(p.config.WorkMin)},
			{ID: "short_min", Label: "Short break minutes (1-30)", Value: strconv.Itoa(p.config.ShortMin)},
			{ID: "long_min", Label: "Long break minutes (1-30)", Value: strconv.Itoa(p.config.LongMin)},
			{ID: "cycles", Label: "Sessions per cycle (1-8)", Value: strconv.Itoa(p.config.Cycles)},
		}
	}
	if version >= 2 {
		frac := 0.0
		if total := p.config.Length(phase); total > 0 {
			frac = float64(total-rem) / float64(total)
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
		}
		phaseLabel := "Progress"
		if phase != PhaseIdle {
			phaseLabel = fmt.Sprintf("%s %s", phaseLabels[phase], clock(rem))
		}
		v.Meters = []sdk.Meter{
			{ID: "m_phase", Label: phaseLabel, Value: frac},
			{ID: "m_cycle", Label: "Cycle", Value: float64(p.cyclePos) / float64(p.config.Cycles)},
		}
		if phase != PhaseIdle {
			state := "paused"
			if p.timer.Running {
				state = "running"
			}
			v.Blocks = []sdk.Block{{
				ID:    "countdown",
				Label: fmt.Sprintf("%s · %s", phaseLabels[phase], state),
				Text:  bigClock(rem),
			}}
		}
	}
	return v
}

// statusLocked picks the line under the timer: transient notes win, then
// the honest state of the countdown, with a standing marker while the
// notify grant is unreachable.
func (p *Pomodoro) statusLocked(now time.Time) string {
	suffix := ""
	if p.notifyDown {
		suffix = " · alerts unavailable"
	}
	if p.statusNote != "" {
		return p.statusNote + suffix
	}
	switch {
	case p.timer.Phase == PhaseIdle:
		return fmt.Sprintf("ready · %dm work · %dm/%dm breaks · %d sessions per cycle%s",
			p.config.WorkMin, p.config.ShortMin, p.config.LongMin, p.config.Cycles, suffix)
	case p.timer.Running:
		return fmt.Sprintf("%s · %s left · session %d of %d%s",
			lowerLabels[p.timer.Phase], clock(p.timer.remaining(now)),
			p.sessionLocked(), p.config.Cycles, suffix)
	default:
		return fmt.Sprintf("paused · %s left on %s%s",
			clock(p.timer.remaining(now)), lowerLabels[p.timer.Phase], suffix)
	}
}

// formatAge renders a duration compactly: "34m", "6h", "2d4h".
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// --- programmatic IPC routes ---

func (p *Pomodoro) stateRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	now := p.now()
	return map[string]any{
		"phase":         p.timer.Phase,
		"running":       p.timer.Running,
		"remaining_sec": int(p.timer.remaining(now).Seconds()),
		"ends_at":       p.timer.EndsAt,
		"session":       p.sessionLocked(),
		"cycle_pos":     p.cyclePos,
		"work_done":     p.workDone,
		"config":        p.config,
		"log":           p.log,
		"notify_down":   p.notifyDown,
		"state_path":    statePath,
	}, nil
}

// lengthParams is the optional phase-length block shared by the start
// and configure routes; nil fields keep their stored values.
type lengthParams struct {
	WorkMin  *int `json:"work_min"`
	ShortMin *int `json:"short_min"`
	LongMin  *int `json:"long_min"`
	Cycles   *int `json:"cycles"`
}

func (p *lengthParams) any() bool {
	return p.WorkMin != nil || p.ShortMin != nil || p.LongMin != nil || p.Cycles != nil
}

func (p *Pomodoro) startRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params lengthParams
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if params.any() {
		if err := p.configureLocked(ctx, params.WorkMin, params.ShortMin, params.LongMin, params.Cycles); err != nil {
			return nil, err
		}
	}
	if err := p.startLocked(ctx, nil); err != nil {
		return nil, err
	}
	return map[string]any{"phase": p.timer.Phase, "running": p.timer.Running}, nil
}

func (p *Pomodoro) pauseRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := p.pauseLocked(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"paused": true, "remaining_sec": int(p.timer.Remaining.Seconds())}, nil
}

func (p *Pomodoro) skipRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := p.skipLocked(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"phase": p.timer.Phase, "running": p.timer.Running}, nil
}

func (p *Pomodoro) resetRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := p.resetLocked(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"phase": p.timer.Phase, "work_done": p.workDone}, nil
}

func (p *Pomodoro) configureRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params lengthParams
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := p.configureLocked(ctx, params.WorkMin, params.ShortMin, params.LongMin, params.Cycles); err != nil {
		return nil, err
	}
	return p.config, nil
}
