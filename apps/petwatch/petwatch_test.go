package petwatch

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
// with an injectable clock. No test sleeps: time moves via h.now.
type harness struct {
	now      time.Time
	files    map[string][]byte
	notifies []notification
	deny     bool  // notify/post denied: simulates a missing grant
	readErr  error // if set, the next fs/read fails once with this error
	seq      atomic.Int64

	app      *Petwatch
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
	p := inst.(*Petwatch)
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

func findMeter(v sdk.View, id string) *sdk.Meter {
	for i := range v.Meters {
		if v.Meters[i].ID == id {
			return &v.Meters[i]
		}
	}
	return nil
}

// --- pure domain tests ---

func TestDecayMathFromTimestamps(t *testing.T) {
	p := newPet(t0) // hunger 80, happy 80, energy 90
	p.advance(t0.Add(10 * time.Hour))
	if p.Hunger != 30 {
		t.Errorf("hunger after 10h = %v, want 30", p.Hunger)
	}
	if p.Happiness != 40 {
		t.Errorf("happiness after 10h = %v, want 40", p.Happiness)
	}
	if p.Energy != 65 {
		t.Errorf("energy after 10h = %v, want 65", p.Energy)
	}
	if p.UpdatedAt != t0.Add(10*time.Hour) {
		t.Errorf("updated_at = %v, want +10h", p.UpdatedAt)
	}
}

func TestDecayClampsAtFloor(t *testing.T) {
	p := newPet(t0)
	p.advance(t0.Add(1000 * time.Hour))
	for _, s := range []struct {
		name string
		got  float64
	}{{"hunger", p.Hunger}, {"happiness", p.Happiness}} {
		if s.got != 0 {
			t.Errorf("%s after 1000h = %v, want 0", s.name, s.got)
		}
	}
	// Energy collapses into sleep; the pet is out cold, not negative.
	if p.Energy < 0 || !p.Sleeping {
		t.Errorf("energy = %v sleeping=%v, want collapsed (>=0, sleeping)", p.Energy, p.Sleeping)
	}
	if !p.Sick {
		t.Error("pet with zero needs should be sick")
	}
}

func TestSleepRegenAndWake(t *testing.T) {
	p := newPet(t0)
	p.BornAt = t0.Add(-2 * time.Hour) // chick, not egg
	p.Sleeping = true
	p.Energy = 50
	p.advance(t0.Add(time.Hour))
	if p.Energy != 75 {
		t.Errorf("energy after 1h sleep = %v, want 75", p.Energy)
	}
	if p.Hunger != 80-2.5 {
		t.Errorf("hunger during sleep = %v, want half-rate decay %v", p.Hunger, 80-2.5)
	}
	if !p.Sleeping {
		t.Error("woke too early: energy 75 is below rested threshold")
	}
	p.advance(t0.Add(2 * time.Hour)) // +50 -> capped at 100, past rested
	if p.Sleeping {
		t.Error("should auto-wake once energy reaches rested threshold")
	}
	if p.Energy != statMax {
		t.Errorf("rested energy = %v, want %v", p.Energy, statMax)
	}
}

func TestClockSkewNeverRewinds(t *testing.T) {
	p := newPet(t0)
	p.advance(t0.Add(-time.Hour))
	if p.Hunger != 80 {
		t.Errorf("backwards advance changed hunger to %v", p.Hunger)
	}
}

func TestLifecycleStages(t *testing.T) {
	p := newPet(t0)
	if p.stage(t0.Add(30*time.Minute)) != StageEgg {
		t.Error("30m should still be egg")
	}
	if p.stage(t0.Add(2*time.Hour)) != StageChick {
		t.Error("2h should be chick")
	}
	if p.stage(t0.Add(30*time.Hour)) != StageAdult {
		t.Error("30h should be adult")
	}
}

func TestSickEnterExit(t *testing.T) {
	p := newPet(t0)
	p.BornAt = t0.Add(-2 * time.Hour)
	p.Hunger = 18
	p.advance(t0.Add(30 * time.Minute)) // 18 - 2.5 = 15.5, above sickEnterAt
	if p.Sick {
		t.Error("hunger 15.5 should not be sick yet")
	}
	p.advance(t0.Add(time.Hour)) // 15.5 - 5 = 10.5 <= 15: sick
	if !p.Sick {
		t.Error("hunger 10.5 should be sick")
	}
	// Recovery needs both needs past sickExitAt; feeding cures 10.5+30.
	p.Happiness = 80
	p.feed()
	if p.Sick {
		t.Error("recovered pet should leave sick state")
	}
}

func TestEggCannotSleepOrPlay(t *testing.T) {
	p := newPet(t0)
	if err := p.toggleSleep(t0); err == nil {
		t.Error("egg toggleSleep should fail")
	}
	if err := p.play(t0); err == nil {
		t.Error("egg play should fail")
	}
}

func TestPlayCostsEnergy(t *testing.T) {
	p := newPet(t0)
	p.BornAt = t0.Add(-30 * time.Hour) // adult
	p.Energy = 40
	h := p.Happiness
	if err := p.play(t0); err != nil {
		t.Fatalf("play: %v", err)
	}
	if p.Happiness != h+playHappiness || p.Energy != 40-playEnergyCost {
		t.Errorf("after play: happy=%v energy=%v", p.Happiness, p.Energy)
	}
	p.Energy = playEnergyCost
	if err := p.play(t0); err == nil {
		t.Error("play at exhaustion threshold should fail")
	}
	p.Sleeping = true
	if err := p.play(t0); err == nil {
		t.Error("play while asleep should fail")
	}
}

func TestExhaustedPetCannotBeWoken(t *testing.T) {
	p := newPet(t0)
	p.BornAt = t0.Add(-2 * time.Hour)
	p.Sleeping = true
	p.Energy = 5
	if err := p.toggleSleep(t0); err == nil {
		t.Error("collapsed pet should refuse to wake")
	}
	p.Energy = 50
	if err := p.toggleSleep(t0); err != nil {
		t.Fatalf("wake with energy: %v", err)
	}
	if p.Sleeping {
		t.Error("pet should be awake")
	}
}

// --- app-level tests through the real SDK presentation handlers ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "petwatch" {
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

func TestV2ViewHasBlocksAndMeters(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 2)
	if len(v.Blocks) != 1 || v.Blocks[0].ID != "pet_art" {
		t.Fatalf("blocks = %+v, want one pet_art block", v.Blocks)
	}
	if !strings.Contains(v.Blocks[0].Text, "___") {
		t.Errorf("egg art missing: %q", v.Blocks[0].Text)
	}
	for _, id := range []string{"m_hunger", "m_happy", "m_energy"} {
		m := findMeter(v, id)
		if m == nil || m.Value < 0 || m.Value > 1 {
			t.Fatalf("meter %s missing or out of range: %+v", id, m)
		}
	}
	if m := findMeter(v, "m_hunger"); m.Value != 0.8 {
		t.Errorf("fresh pet hunger meter = %v, want 0.8", m.Value)
	}
}

func TestV1FallbackIsOneLine(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 1)
	if len(v.Blocks) != 0 || len(v.Meters) != 0 || v.Grid != nil {
		t.Fatal("v1 view must not carry blocks, meters, or grid")
	}
	about := findItem(v, "about")
	if about == nil || !strings.Contains(about.Detail, "fed 80%") {
		t.Fatalf("v1 pet line = %+v", about)
	}
	if !strings.Contains(about.Detail, "egg") {
		t.Errorf("v1 pet line should name the stage: %q", about.Detail)
	}
	// Same session serves v2 too.
	v2 := h.view(t, 2)
	if len(v2.Blocks) != 1 {
		t.Error("v2 after v1 should still render art")
	}
}

func TestAddAndCompleteTaskFeedsPet(t *testing.T) {
	h := newHarness(t)
	h.view(t, 2)             // birth anchored at t0
	h.advance(2 * time.Hour) // hatch it first: chick
	before := h.view(t, 2)
	hungerBefore := *findMeter(before, "m_hunger")

	v := h.act(t, 2, "add_task", "", map[string]string{"new_task": "write tests"})
	if findItem(v, "task_1") == nil {
		t.Fatalf("task_1 not in items: %+v", v.Items)
	}
	if a := findAction(v, "complete"); a == nil || a.Disabled {
		t.Fatal("complete action should be enabled with a pending task")
	}

	v = h.act(t, 2, "complete", "task_1", nil)
	after := *findMeter(v, "m_hunger")
	if after.Value <= hungerBefore.Value-0.05 { // decayed ~2h*5/h then +30
		t.Errorf("hunger meter did not rise after feeding: %v -> %v", hungerBefore.Value, after.Value)
	}
	if h.app.pet.Feeds != 1 {
		t.Errorf("feeds = %d, want 1", h.app.pet.Feeds)
	}
	if !strings.Contains(v.Status, "devoured") {
		t.Errorf("status after feeding = %q", v.Status)
	}
	if !strings.Contains(v.Blocks[0].Text, "nom") {
		t.Errorf("eating frame should show after feeding: %q", v.Blocks[0].Text)
	}
	// Completing a task that left the item list is a contract-level error:
	// the action request names an item the current snapshot no longer has.
	if _, err := h.tryAct(2, "complete", "task_1", nil); err == nil {
		t.Error("re-completing a done task should be rejected")
	}
	// The domain-level route reports it too.
	hd := h.handlers["complete"]
	raw, _ := json.Marshal(map[string]string{"id": "task_1"})
	if _, err := hd(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "no pending task") {
		t.Errorf("route re-complete should report the domain error, got %v", err)
	}
}

func TestEmptyAndDuplicateTaskRejection(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 2, "add_task", "", map[string]string{"new_task": "   "})
	if !strings.Contains(v.Status, "name") {
		t.Errorf("empty task status = %q", v.Status)
	}
	if len(v.Items) != 1 {
		t.Errorf("items after rejected add = %d, want just the about item", len(v.Items))
	}
}

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(2, "launch_missiles", "", nil); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

func TestPersistenceAcrossRestartAgesByWallClock(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "add_task", "", map[string]string{"new_task": "feed me later"})
	h.route(t, "complete", map[string]string{"id": "task_1"}, nil)

	// Simulate shutdown: Stop persists (stats, updated_at) at t0.
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Forty hours pass with the app off. New instance, same fs, clock +40h.
	h2 := newHarness(t)
	h2.files = h.files
	h2.now = t0.Add(40 * time.Hour)

	v := h2.view(t, 2)
	// 80+30fed=100 clamped -> 100 - 5*40 = -100 -> 0; energy 90+5=95-100 -> 0.
	if m := findMeter(v, "m_hunger"); m.Value != 0 {
		t.Errorf("hunger after 40h offline = %v, want 0", m.Value)
	}
	if !h2.app.pet.Sick {
		t.Error("pet starved for 40h while off should wake up sick")
	}
	if !h2.app.pet.Sleeping {
		t.Error("pet at zero energy should be collapsed asleep")
	}
	if len(h2.app.tasks) != 1 || !h2.app.tasks[0].Done {
		t.Error("completed task should persist across restart")
	}
	// It woke up sick: the notification pipeline heard about it.
	var sawSick, sawHunger bool
	for _, n := range h2.notifies {
		sawSick = sawSick || n.Severity == "error" && strings.Contains(n.Body, "sick")
		sawHunger = sawHunger || n.Severity == "warning" && strings.Contains(n.Body, "hungry")
	}
	if !sawSick || !sawHunger {
		t.Errorf("starving pet should alert on load; got %+v", h2.notifies)
	}
}

func TestAlertCooldownAndRecoveryReset(t *testing.T) {
	h := newHarness(t)
	h.view(t, 2)             // birth anchored at t0
	h.advance(2 * time.Hour) // chick
	h.view(t, 2)             // re-anchor updated_at at t0+2h
	h.app.pet.Hunger = 20    // below hungryAlertAt
	h.app.tick(context.Background())
	if n := len(h.notifies); n != 2 { // hatched + hunger
		t.Fatalf("notifies after starvation = %d, want 2 (hatched+hunger)", n)
	}
	h.advance(30 * time.Second)
	h.app.tick(context.Background())
	if n := len(h.notifies); n != 2 {
		t.Fatalf("repeat tick reposted; notifies = %d", n)
	}
	// Feeding recovers and clears the hunger key; the next dip re-alerts.
	h.act(t, 2, "add_task", "", map[string]string{"new_task": "snack"})
	h.route(t, "complete", map[string]string{"id": "task_1"}, nil)
	h.app.pet.Hunger = 10
	h.app.tick(context.Background())
	if n := len(h.notifies); n != 3 {
		t.Fatalf("re-dip after recovery should re-alert; notifies = %d", n)
	}
}

func TestNotifyDegradesQuietly(t *testing.T) {
	h := newHarness(t)
	h.view(t, 2) // birth anchored at t0
	h.deny = true
	h.advance(30 * time.Hour) // adult, starving
	h.app.tick(context.Background())
	h.app.tick(context.Background()) // second tick: still quiet
	if !h.app.notifyDown {
		t.Error("denied notify/post should set notifyDown")
	}
	v := h.view(t, 2) // app keeps rendering; no crash
	if v.State != sdk.ViewReady {
		t.Errorf("view state = %q after notify denial", v.State)
	}
	// Grant restored: the next crossing posts again.
	h.deny = false
	h.app.pet.Hunger = 5
	h.app.pet.Sick = true
	delete(h.app.alerts, "sick")
	h.app.tick(context.Background())
	if len(h.notifies) == 0 {
		t.Error("restored grant should resume posting")
	}
	if h.app.notifyDown {
		t.Error("notifyDown should clear on success")
	}
}

func TestHatchNotificationOnce(t *testing.T) {
	h := newHarness(t)
	h.view(t, 2)                // birth anchored at t0
	h.advance(90 * time.Minute) // past hatchAfter
	h.app.tick(context.Background())
	h.advance(time.Hour)
	h.app.tick(context.Background())
	hatch := 0
	for _, n := range h.notifies {
		if n.Severity == "info" && strings.Contains(n.Body, "hatched") {
			hatch++
		}
	}
	if hatch != 1 {
		t.Fatalf("hatch notifications = %d, want exactly 1", hatch)
	}
}

func TestSleepCycleViaActions(t *testing.T) {
	h := newHarness(t)
	h.view(t, 2)             // birth anchored at t0
	h.advance(2 * time.Hour) // chick
	h.view(t, 2)             // re-anchor updated_at at t0+2h
	h.app.pet.Energy = 50    // mid-range: a nap won't hit the auto-wake point
	v := h.act(t, 2, "toggle_sleep", "", nil)
	if a := findAction(v, "toggle_sleep"); a.Label != "Wake" {
		t.Errorf("sleep toggle label = %q, want Wake", a.Label)
	}
	if !strings.Contains(v.Blocks[0].Text, "z") {
		t.Errorf("asleep art should show z's: %q", v.Blocks[0].Text)
	}
	if a := findAction(v, "play"); !a.Disabled {
		t.Error("play should be disabled while asleep")
	}
	h.advance(time.Hour) // regen to 75, still below the 90 auto-wake point
	v = h.act(t, 2, "toggle_sleep", "", nil)
	if a := findAction(v, "toggle_sleep"); a.Label != "Nap" {
		t.Errorf("after wake label = %q, want Nap", a.Label)
	}
	if m := findMeter(v, "m_energy"); m.Value != 0.75 {
		t.Errorf("energy after 1h nap = %v, want 0.75", m.Value)
	}
}

func TestCompleteRouteAndStateRoute(t *testing.T) {
	h := newHarness(t)
	var addOut struct {
		ID string `json:"id"`
	}
	h.route(t, "add", map[string]string{"title": "route task"}, &addOut)
	if addOut.ID != "task_1" {
		t.Fatalf("add id = %q", addOut.ID)
	}
	var st struct {
		Stage   string `json:"stage"`
		Pending int    `json:"pending"`
	}
	h.route(t, "state", nil, &st)
	if st.Stage != "egg" || st.Pending != 1 {
		t.Errorf("state = %+v", st)
	}
	var done struct {
		Done  bool `json:"done"`
		Feeds int  `json:"feeds"`
	}
	h.route(t, "complete", map[string]string{"id": "task_1"}, &done)
	if !done.Done || done.Feeds != 1 {
		t.Errorf("complete = %+v", done)
	}
	// Route-level errors are honest IPC errors.
	hd := h.handlers["complete"]
	raw, _ := json.Marshal(map[string]string{"id": "task_999"})
	if _, err := hd(context.Background(), raw); err == nil {
		t.Error("complete of unknown task should be an IPC error")
	}
}

func TestCorruptSaveStartsFreshEgg(t *testing.T) {
	h := newHarness(t)
	h.files[statePath] = []byte("{not json")
	v := h.view(t, 2)
	if !strings.Contains(v.Status, "new egg") {
		t.Errorf("corrupt save status = %q", v.Status)
	}
	if h.app.pet.stage(h.now) != StageEgg {
		t.Error("corrupt save should start a fresh egg")
	}
}

func TestActionViewAlsoServesV1(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 1, "add_task", "", map[string]string{"new_task": "v1 task"})
	if len(v.Blocks) != 0 || len(v.Meters) != 0 {
		t.Fatal("v1 action response must not carry v2 elements")
	}
	if findItem(v, "task_1") == nil {
		t.Error("v1 action view should still list the task")
	}
}

// A transient fs/read failure (a canceled call racing the first view, a
// host I/O error, the read-size bound) is not "no save yet": the app must
// say so honestly, retry the load, and never let the next change overwrite
// the saved pet.
func TestTransientReadErrorNeverOverwritesPet(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, "add_task", "", map[string]string{"new_task": "keep me task"})

	h2 := newHarness(t)
	h2.files = h.files
	h2.readErr = context.Canceled // first fs/read fails once, transiently

	v := h2.view(t, 2)
	if v.State != sdk.ViewError || v.Error == "" {
		t.Fatalf("transient read failure view = %+v, want an honest error", v)
	}

	// The retry loads the real file; an add then extends it, not
	// replaces it.
	v = h2.act(t, 2, "add_task", "", map[string]string{"new_task": "second task"})
	var st diskState
	if err := json.Unmarshal(h2.files[statePath], &st); err != nil {
		t.Fatalf("post-change save is not valid json: %v", err)
	}
	if len(st.Tasks) != 2 {
		t.Fatalf("%d task(s) on disk after a transient read error and one add, want 2", len(st.Tasks))
	}
}
