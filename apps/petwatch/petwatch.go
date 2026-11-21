// Package petwatch is Gostalgia's Tamagotchi task manager: a desktop pet
// whose hunger, happiness, and energy decay by elapsed wall-clock time,
// rendered through the version-2 presentation contract (block art plus
// meter gauges) with a one-line fallback for version-1 requests.
//
// Completing tasks is the only food. Pet state and the task list persist
// together in the app-private partition
// (/apps/data/com.gostalgia.petwatch/state.json) so an idle stretch while
// the app is off ages the pet by real elapsed time, not app uptime. A
// starving pet posts notify/post alerts while the grant is held; if the
// grant is missing or revoked the app logs once and keeps working.
package petwatch

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Petwatch.
const ID = "com.gostalgia.petwatch"

// statePath is the single persisted document inside the app-private
// partition. fs/save stages and renames atomically, so a crash mid-write
// leaves the previous state intact.
const statePath = "/apps/data/com.gostalgia.petwatch/state.json"

const stateVersion = 1

// tickInterval is the live decay/save cadence while the app runs.
const tickInterval = 30 * time.Second

// alertCooldown bounds level-triggered alerts; recovery clears the key so
// the next dip alerts immediately.
const alertCooldown = 8 * time.Hour

// flashDuration is how long the eating/playing frame overrides the
// persistent mood after a care action.
const flashDuration = 90 * time.Second

// hungryAlertAt is the fullness level that triggers a warning.
const hungryAlertAt = 25.0

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

// diskState is the persisted document: pet, tasks, task counter, and
// alert bookkeeping. Pet.UpdatedAt is the decay anchor the next launch
// ages from.
type diskState struct {
	Version int                  `json:"version"`
	Pet     Pet                  `json:"pet"`
	Tasks   []Task               `json:"tasks"`
	Seq     int                  `json:"seq"`
	Alerts  map[string]time.Time `json:"alerts,omitempty"`
}

// Petwatch is the running in-process instance. All state is guarded by mu.
type Petwatch struct {
	app *sdk.Context
	now func() time.Time

	mu         sync.Mutex
	loaded     bool
	pet        Pet
	tasks      []Task
	seq        int
	alerts     map[string]time.Time
	statusNote string // transient feedback line; cleared on each action
	lastError  string

	flash      Mood      // transient mood override (eating, playing)
	flashUntil time.Time // instant the override expires

	notifyDown bool // notify/post failed; logged once, stays quiet
}

// Factory constructs a fresh, uninitialized Petwatch instance.
func Factory() (sdk.Instance, error) { return &Petwatch{now: time.Now}, nil }

// setClock injects a deterministic clock for tests.
func setClock(p *Petwatch, now func() time.Time) { p.now = now }

// Init registers routes and the presentation callbacks. State loads
// lazily on first use so Init stays prompt.
func (p *Petwatch) Init(app *sdk.Context) error {
	p.app = app
	if p.now == nil {
		p.now = time.Now
	}
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"state", p.stateRoute},
		{"add", p.addRoute},
		{"complete", p.completeRoute},
		{"play", p.playRoute},
		{"sleep", p.sleepRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(p.view, p.act)
}

// Run ticks the live decay/save loop until canceled.
func (p *Petwatch) Run(ctx context.Context) error {
	p.app.Log.Info("petwatch running")
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

// Stop flushes the final state so the next launch ages from a fresh
// timestamp. A failed flush is logged, never fatal.
func (p *Petwatch) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		if err := p.persistLocked(ctx); err != nil && p.app.Log != nil {
			p.app.Log.Warn("petwatch: final save failed", "err", err)
		}
	}
	return nil
}

// tick applies one decay step plus alert evaluation and persists.
func (p *Petwatch) tick(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		p.lastError = err.Error()
		return
	}
	p.advanceLocked(ctx, p.now())
	if err := p.persistLocked(ctx); err != nil {
		p.lastError = fmt.Sprintf("save failed: %v", err)
	}
}

// ensureLoadedLocked reads and decodes state.json once, aging the pet by
// the wall-clock gap since the persisted anchor. A missing or unreadable
// save starts a fresh egg with an honest note in the status line. A denied
// read is reported, since nothing will persist without the grant. Any other
// read failure is NOT "no save yet": the app reports an honest error and
// stays unloaded so the next call retries — a fresh egg must never
// overwrite a save it could not read.
func (p *Petwatch) ensureLoadedLocked(ctx context.Context) error {
	if p.loaded {
		return nil
	}
	p.alerts = map[string]time.Time{}
	p.pet = newPet(p.now())

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := p.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// no save yet: fresh egg, nothing to report
		case errors.Is(err, fs.ErrPermission):
			p.lastError = fmt.Sprintf("storage access denied (%v) — your pet will not be saved", err)
		default:
			return fmt.Errorf("the saved pet could not be loaded: %w", err)
		}
		p.loaded = true
		return nil
	}
	p.loaded = true
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		p.statusNote = "previous save unreadable; starting a new egg"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		p.statusNote = "previous save unreadable; starting a new egg"
		return nil
	}
	p.pet = st.Pet
	p.tasks = st.Tasks
	p.seq = st.Seq
	if st.Alerts != nil {
		p.alerts = st.Alerts
	}
	p.sanitizeLocked()
	// Age the pet across the offline gap, alert on what it woke to, and
	// re-anchor the file so (stats, updated_at) stays a consistent pair.
	p.advanceLocked(ctx, p.now())
	if err := p.persistLocked(ctx); err != nil && p.app.Log != nil {
		p.app.Log.Warn("petwatch: re-anchor save failed", "err", err)
	}
	return nil
}

// sanitizeLocked repairs loaded state: stats clamped, timestamps pinned
// inside [BornAt, now], name defaulted, seq pushed past every task id.
func (p *Petwatch) sanitizeLocked() {
	now := p.now()
	p.pet.Hunger = clamp(p.pet.Hunger)
	p.pet.Happiness = clamp(p.pet.Happiness)
	p.pet.Energy = clamp(p.pet.Energy)
	if p.pet.Name == "" {
		p.pet.Name = "Mochi"
	}
	if p.pet.BornAt.IsZero() || p.pet.BornAt.After(now) {
		p.pet.BornAt = now
	}
	if p.pet.UpdatedAt.IsZero() || p.pet.UpdatedAt.After(now) {
		p.pet.UpdatedAt = now
	}
	for _, t := range p.tasks {
		var n int
		if _, err := fmt.Sscanf(t.ID, "task_%d", &n); err == nil && n > p.seq {
			p.seq = n
		}
	}
}

// persistLocked writes the whole state document atomically, pruning the
// oldest completed tasks past maxDoneRetain to bound the file.
func (p *Petwatch) persistLocked(ctx context.Context) error {
	done := 0
	for _, t := range p.tasks {
		if t.Done {
			done++
		}
	}
	if done > maxDoneRetain {
		remaining := done - maxDoneRetain
		kept := p.tasks[:0]
		for _, t := range p.tasks {
			if t.Done && remaining > 0 {
				remaining--
				continue
			}
			kept = append(kept, t)
		}
		p.tasks = kept
	}
	data, err := json.Marshal(diskState{
		Version: stateVersion,
		Pet:     p.pet,
		Tasks:   p.tasks,
		Seq:     p.seq,
		Alerts:  p.alerts,
	})
	if err != nil {
		return err
	}
	return p.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
}

// advanceLocked applies decay to now, then evaluates alert conditions.
// Caller holds mu.
func (p *Petwatch) advanceLocked(ctx context.Context, now time.Time) {
	p.pet.advance(now)
	p.checkAlertsLocked(ctx, now)
}

// checkAlertsLocked posts notify/post alerts for level conditions with
// per-kind cooldowns. "hatched" fires once ever; the others clear their
// key when the condition recovers so the next dip re-alerts.
func (p *Petwatch) checkAlertsLocked(ctx context.Context, now time.Time) {
	post := func(kind, severity, body string) {
		if t, ok := p.alerts[kind]; ok {
			if kind == "hatched" || now.Sub(t) < alertCooldown {
				return
			}
		}
		if p.notify(ctx, severity, body) {
			p.alerts[kind] = now
		}
	}

	if stage := p.pet.stage(now); stage != StageEgg {
		post("hatched", "info", fmt.Sprintf("%s hatched from its egg!", p.pet.Name))
		if p.pet.Hunger <= hungryAlertAt {
			post("hunger", "warning", fmt.Sprintf("%s is hungry - complete a task to feed it.", p.pet.Name))
		} else if p.pet.Hunger > hungryAlertAt+15 {
			delete(p.alerts, "hunger")
		}
		if p.pet.Sick {
			post("sick", "error", fmt.Sprintf("%s is sick from neglect - it needs real food (finish a task).", p.pet.Name))
		} else {
			delete(p.alerts, "sick")
		}
	}
	if p.pet.Sleeping && p.pet.Energy <= sleepCollapseAt {
		post("collapsed", "warning", fmt.Sprintf("%s collapsed from exhaustion. Let it rest.", p.pet.Name))
	} else if !p.pet.Sleeping {
		delete(p.alerts, "collapsed")
	}
}

// notify posts through the notify/post route. The notify capability may
// be absent or revoked; failures are logged once and the flag suppresses
// retry noise until a call succeeds again.
func (p *Petwatch) notify(ctx context.Context, severity, body string) bool {
	err := p.app.Call(ctx, "notify/post", map[string]string{
		"severity": severity,
		"title":    "Petwatch",
		"body":     body,
	}, nil)
	if err != nil {
		if !p.notifyDown && p.app.Log != nil {
			p.app.Log.Info("petwatch: notify/post unavailable", "err", err)
		}
		p.notifyDown = true
		return false
	}
	p.notifyDown = false
	return true
}

// --- mutations: error-returning, shared by presentation and routes ---

func (p *Petwatch) addTaskLocked(ctx context.Context, title string) error {
	title = sanitizeTaskTitle(title)
	if title == "" {
		return fmt.Errorf("give the task a name first")
	}
	pending := 0
	for _, t := range p.tasks {
		if !t.Done {
			pending++
		}
	}
	if pending >= maxTasksTotal {
		return fmt.Errorf("task list is full (%d pending)", maxTasksTotal)
	}
	p.seq++
	now := p.now()
	p.tasks = append(p.tasks, Task{
		ID:        fmt.Sprintf("task_%d", p.seq),
		Title:     title,
		CreatedAt: now,
	})
	p.pet.Happiness = clamp(p.pet.Happiness + addTaskHappy)
	p.statusNote = fmt.Sprintf("task %q added - finishing it feeds %s", title, p.pet.Name)
	p.checkAlertsLocked(ctx, p.now())
	return p.persistLocked(ctx)
}

func (p *Petwatch) completeTaskLocked(ctx context.Context, itemID string) error {
	if itemID == "" {
		return fmt.Errorf("select a task to complete")
	}
	for i := range p.tasks {
		if p.tasks[i].ID == itemID && !p.tasks[i].Done {
			p.tasks[i].Done = true
			p.tasks[i].DoneAt = p.now()
			p.pet.feed()
			p.flash = MoodEating
			p.flashUntil = p.now().Add(flashDuration)
			p.statusNote = fmt.Sprintf("%s devoured %q!", p.pet.Name, p.tasks[i].Title)
			p.checkAlertsLocked(ctx, p.now())
			return p.persistLocked(ctx)
		}
	}
	return fmt.Errorf("no pending task %q", itemID)
}

func (p *Petwatch) playLocked(ctx context.Context) error {
	if err := p.pet.play(p.now()); err != nil {
		return err
	}
	p.flash = MoodPlaying
	p.flashUntil = p.now().Add(flashDuration)
	p.statusNote = fmt.Sprintf("%s loved that!", p.pet.Name)
	p.checkAlertsLocked(ctx, p.now())
	return p.persistLocked(ctx)
}

func (p *Petwatch) setSleepLocked(ctx context.Context, want *bool) error {
	target := !p.pet.Sleeping
	if want != nil {
		target = *want
	}
	if target == p.pet.Sleeping {
		return p.persistLocked(ctx) // idempotent set
	}
	if err := p.pet.toggleSleep(p.now()); err != nil {
		return err
	}
	if p.pet.Sleeping {
		p.statusNote = fmt.Sprintf("%s is napping - energy will recover", p.pet.Name)
	} else {
		p.statusNote = fmt.Sprintf("%s woke up", p.pet.Name)
	}
	p.checkAlertsLocked(ctx, p.now())
	return p.persistLocked(ctx)
}

// --- presentation ---

func (p *Petwatch) view(ctx context.Context) (sdk.View, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Petwatch", State: sdk.ViewError, Error: err.Error()}, nil
	}
	// Views advance the clock in memory only; ticks persist. A crash
	// between them still decays honestly from the persisted anchor.
	p.pet.advance(p.now())
	return p.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (p *Petwatch) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Petwatch", State: sdk.ViewError, Error: err.Error()}, nil
	}
	p.lastError = ""
	p.statusNote = ""
	p.advanceLocked(ctx, p.now())

	var err error
	switch req.Action {
	case "add_task":
		err = p.addTaskLocked(ctx, req.Values["new_task"])
	case "complete":
		err = p.completeTaskLocked(ctx, req.ItemID)
	case "play":
		err = p.playLocked(ctx)
	case "toggle_sleep":
		err = p.setSleepLocked(ctx, nil)
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		// Domain rejections land in the status line, not the error banner:
		// the pet and the task list are still fully usable.
		p.statusNote = err.Error()
	}
	return p.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

// currentMoodLocked resolves the flash override over the persistent mood.
// Sleeping pets never flash; sick pets still perk up for food.
func (p *Petwatch) currentMoodLocked(now time.Time) Mood {
	if p.flash != "" && now.Before(p.flashUntil) && !p.pet.Sleeping {
		return p.flash
	}
	return p.pet.mood(now)
}

// viewLocked builds the snapshot at the negotiated presentation version.
func (p *Petwatch) viewLocked(version int) sdk.View {
	now := p.now()
	stage := p.pet.stage(now)
	mood := p.currentMoodLocked(now)

	pending, done := 0, 0
	for _, t := range p.tasks {
		if t.Done {
			done++
		} else {
			pending++
		}
	}

	about := fmt.Sprintf("%s · %s · alive %s · fed %dx · %d pending",
		stage, mood, formatAge(now.Sub(p.pet.BornAt)), p.pet.Feeds, pending)
	items := []sdk.Item{{ID: "about", Label: p.pet.Name, Detail: about}}
	for _, t := range p.tasks {
		if t.Done {
			continue
		}
		items = append(items, sdk.Item{
			ID:     t.ID,
			Label:  t.Title,
			Detail: fmt.Sprintf("task · added %s ago", formatAge(now.Sub(t.CreatedAt))),
		})
	}

	sleepLabel := "Nap"
	if p.pet.Sleeping {
		sleepLabel = "Wake"
	}

	v := sdk.View{
		Title:  fmt.Sprintf("Petwatch - %s the %s", p.pet.Name, stage),
		State:  sdk.ViewReady,
		Items:  items,
		Fields: []sdk.Field{{ID: "new_task", Label: "New task"}},
		Actions: []sdk.Action{
			{ID: "add_task", Label: "Add task"},
			{ID: "complete", Label: "Complete selected", Disabled: pending == 0},
			{ID: "play", Label: "Play", Disabled: stage == StageEgg || p.pet.Sleeping},
			{ID: "toggle_sleep", Label: sleepLabel, Disabled: stage == StageEgg},
		},
		Status: p.statusLocked(now, pending, done, mood),
		Error:  p.lastError,
	}
	if version >= 2 {
		art := artFor(stage, mood)
		if stage == StageEgg && now.Sub(p.pet.BornAt) > hatchAfter*2/3 {
			art = artEggCracked
		}
		v.Blocks = []sdk.Block{{
			ID:    "pet_art",
			Label: fmt.Sprintf("%s - %s, %s", p.pet.Name, stage, mood),
			Text:  art,
		}}
		v.Meters = []sdk.Meter{
			{ID: "m_hunger", Label: "Fed", Value: p.pet.Hunger / statMax},
			{ID: "m_happy", Label: "Mood", Value: p.pet.Happiness / statMax},
			{ID: "m_energy", Label: "Energy", Value: p.pet.Energy / statMax},
		}
		return v
	}
	// Version-1 requests carry no blocks or meters: the pet renders as one
	// honest line of text plus its stats in the item detail.
	items[0].Detail = fmt.Sprintf("%s %s · %s · fed %.0f%% mood %.0f%% energy %.0f%%",
		faceLine(mood), stage, mood, p.pet.Hunger, p.pet.Happiness, p.pet.Energy)
	v.Items = items
	return v
}

// statusLocked picks the line under the art: transient notes win, then
// the most urgent condition, then a calm summary.
func (p *Petwatch) statusLocked(now time.Time, pending, done int, mood Mood) string {
	if p.statusNote != "" {
		return p.statusNote
	}
	switch {
	case p.pet.Sick:
		return fmt.Sprintf("%s is sick - it needs real food (complete a task)", p.pet.Name)
	case p.pet.Hunger <= hungryAlertAt:
		return fmt.Sprintf("%s is starving - complete a task to feed it", p.pet.Name)
	case p.pet.Sleeping:
		return fmt.Sprintf("%s is asleep - energy recovering", p.pet.Name)
	case mood == MoodEating:
		return fmt.Sprintf("%s is eating. Good pet.", p.pet.Name)
	default:
		return fmt.Sprintf("%s the %s is %s · %d pending · %d done",
			p.pet.Name, p.pet.stage(now), mood, pending, done)
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

func (p *Petwatch) stateRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	p.pet.advance(p.now())
	now := p.now()
	pending, done := 0, 0
	for _, t := range p.tasks {
		if t.Done {
			done++
		} else {
			pending++
		}
	}
	return map[string]any{
		"pet":         p.pet,
		"stage":       p.pet.stage(now),
		"mood":        p.currentMoodLocked(now),
		"tasks":       p.tasks,
		"pending":     pending,
		"done":        done,
		"alerts":      p.alerts,
		"state_path":  statePath,
		"notify_down": p.notifyDown,
	}, nil
}

func (p *Petwatch) addRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Title string `json:"title"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	p.advanceLocked(ctx, p.now())
	if err := p.addTaskLocked(ctx, params.Title); err != nil {
		return nil, err
	}
	last := p.tasks[len(p.tasks)-1]
	return map[string]any{"id": last.ID, "title": last.Title, "added": true}, nil
}

func (p *Petwatch) completeRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	p.advanceLocked(ctx, p.now())
	if err := p.completeTaskLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "done": true, "feeds": p.pet.Feeds}, nil
}

func (p *Petwatch) playRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	p.advanceLocked(ctx, p.now())
	if err := p.playLocked(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"played": true, "happiness": p.pet.Happiness}, nil
}

func (p *Petwatch) sleepRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Sleep *bool `json:"sleep"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	p.advanceLocked(ctx, p.now())
	if err := p.setSleepLocked(ctx, params.Sleep); err != nil {
		return nil, err
	}
	return map[string]any{"sleeping": p.pet.Sleeping}, nil
}
