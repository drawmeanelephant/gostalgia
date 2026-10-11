package shell

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/ipc"
	"gostalgia/sdk"
)

// appView is shell-owned state. No app supplies a tea.Model, keymap, style, or
// terminal handle. Epoch/sequence guards reject replies after dismissal,
// cancellation, or a later operation. version is the negotiated presentation
// protocol; cell is the selected grid cell when a version-2 grid exists.
// regionTop/regionPinned/regionWindow drive the version-3 scroll region; the
// window is written by the renderer so key handling clamps against the same
// line count the user sees. refreshing/refreshQueued coalesce version-3
// invalidation pushes into at most one in-flight refetch plus one follow-up.
type appView struct {
	id, instance string
	pid          int32
	epoch, seq   uint64
	version      int
	data         sdk.View
	values       map[string]string
	focus, item  int
	cell         int
	itemFocus    bool
	busy         bool
	request      string
	cancel       context.CancelFunc
	banner       string

	regionTop    int
	regionPinned bool
	regionWindow int

	refreshing    bool
	refreshQueued bool
}

type viewMsg struct {
	epoch, seq uint64
	version    int
	data       sdk.View
	err        error
	action     bool
	refresh    bool
}

// viewSubscribedMsg carries the event subscription the shell opens once per
// connection for the version-3 push channel; a nil sub means the caller does
// not support subscriptions and polling remains the only refresh path.
type viewSubscribedMsg struct {
	sub *ipc.ClientSubscription
	err error
}

// viewEventMsg is one bus notification from the subscription. ok=false means
// the subscription stream ended.
type viewEventMsg struct {
	event ipc.Notification
	ok    bool
}

type viewTickMsg struct{ epoch uint64 }
type viewCheckMsg struct {
	epoch, seq uint64
	apps       []appStatus
	data       *sdk.View
	version    int
	err        error
	viewErr    error
}
type viewCancelMsg struct{ err error }

// fetchView requests a snapshot at the newest version the shell supports and
// steps down one version at a time while the app reports an unsupported
// version, so an app speaking only an older contract keeps rendering and a
// future newer shell does not strand mid-version apps at the oldest contract.
// The fallback matches error text because the protocol has no structured
// unsupported-version error; keep the message check this narrow. The stamped
// snapshot version is the negotiated version callers reuse on action and
// cancel requests. A reply stamped newer than requested is rejected: an app
// speaking raw IPC must not serve elements the shell never asked for.
func fetchView(ctx context.Context, client Caller, id string, version int) (sdk.View, int, error) {
	var data sdk.View
	for {
		err := client.Call(ctx, "app/"+id+"/view", sdk.ViewRequest{Version: version}, &data)
		if err != nil {
			if version > sdk.MinPresentationVersion && strings.Contains(err.Error(), "unsupported version") {
				version--
				continue
			}
			return data, data.Version, err
		}
		if data.Version > version {
			return data, data.Version, fmt.Errorf("presentation: view stamped version %d exceeds requested %d", data.Version, version)
		}
		return data, data.Version, data.Validate()
	}
}

func (m *Model) openView(a appStatus) tea.Cmd {
	m.viewEpoch++
	v := &appView{
		id: a.Manifest.ID, pid: a.PID, epoch: m.viewEpoch, version: sdk.PresentationVersion,
		data:   sdk.View{Title: a.Manifest.Name, State: sdk.ViewLoading},
		values: make(map[string]string), regionPinned: true,
	}
	m.presentation, m.shelf, m.scroll = v, false, 0
	epoch, ctx, client, id := v.epoch, m.ctx, m.client, v.id
	return tea.Batch(func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		data, version, err := fetchView(ctx, client, id, sdk.PresentationVersion)
		return viewMsg{epoch: epoch, version: version, data: data, err: err}
	}, viewTick(epoch), m.ensureViewSub())
}

func viewTick(epoch uint64) tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return viewTickMsg{epoch} })
}

// viewEventTopic is the bus topic of the version-3 invalidation signal; the
// record source carries "app/<app-id>" of the app that pushed.
const viewEventTopic = "app.view"

// viewSubscriber is the optional subscription half of the environment client:
// *ipc.Client implements it, in-process fakes do not. When absent the
// version-3 push path silently degrades to the ordinary poll.
type viewSubscriber interface {
	Subscribe(context.Context, ipc.SubscribeParams) (*ipc.ClientSubscription, error)
}

// ensureViewSub opens the connection's single event subscription on the
// "app.view" topic the first time an app view opens. Subscribing is IPC off
// the event loop; the answer arrives as viewSubscribedMsg.
func (m *Model) ensureViewSub() tea.Cmd {
	if m.viewSub != nil {
		return nil
	}
	client, ok := m.client.(viewSubscriber)
	if !ok {
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		sub, err := client.Subscribe(ctx, ipc.SubscribeParams{Topic: viewEventTopic, Buffer: 16})
		return viewSubscribedMsg{sub: sub, err: err}
	}
}

// waitViewEvent blocks in a command goroutine for the next subscription
// notification; the update loop re-arms it after every delivery.
func (m *Model) waitViewEvent(sub *ipc.ClientSubscription) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case n, ok := <-sub.Events:
			return viewEventMsg{event: n, ok: ok}
		case <-ctx.Done():
			return nil
		}
	}
}

// refreshView refetches the current snapshot at the negotiated version in
// answer to a version-3 invalidation push. Unlike the poll it skips app/list:
// liveness keeps coming from the tick.
func (m *Model) refreshView() tea.Cmd {
	v := m.presentation
	v.refreshing = true
	epoch, seq, id, version := v.epoch, v.seq, v.id, v.version
	ctx, client := m.ctx, m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var data sdk.View
		err := client.Call(ctx, "app/"+id+"/view", sdk.ViewRequest{Version: version}, &data)
		if err == nil {
			err = data.Validate()
		}
		if err == nil && data.Version > version {
			err = fmt.Errorf("presentation: view stamped version %d exceeds requested %d", data.Version, version)
		}
		return viewMsg{epoch: epoch, seq: seq, version: version, data: data, err: err, refresh: true}
	}
}

// dismissView retracts focus and data immediately. Cancel uses a separate IPC
// call because canceling a socket client's wait alone cannot stop server work.
func (m *Model) dismissView() tea.Cmd {
	v := m.presentation
	m.presentation = nil
	m.viewEpoch++
	if v == nil || v.cancel == nil {
		return nil
	}
	return m.cancelViewAction(v)
}

func (m *Model) cancelViewAction(v *appView) tea.Cmd {
	v.cancel()
	v.cancel = nil
	v.busy = false
	v.seq++
	v.banner = "Canceled. Work already committed cannot be undone."
	params := sdk.CancelRequest{Version: v.version, Instance: v.instance, RequestID: v.request}
	ctx, client, id := context.WithoutCancel(m.ctx), m.client, v.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		err := client.Call(ctx, "app/"+id+"/cancel", params, nil)
		return viewCancelMsg{err}
	}
}

func (m *Model) checkView() tea.Cmd {
	v := m.presentation
	epoch, seq, id, pid, busy, version := v.epoch, v.seq, v.id, v.pid, v.busy, v.version
	negotiated := v.instance != ""
	ctx, client := m.ctx, m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		msg := viewCheckMsg{epoch: epoch, seq: seq}
		msg.err = client.Call(ctx, "app/list", nil, &msg.apps)
		if msg.err == nil && !busy {
			for _, a := range msg.apps {
				if a.Manifest.ID == id && a.Running && a.PID == pid {
					if !negotiated {
						// The open fetch failed before negotiation finished, so
						// renegotiate instead of polling at a version the app
						// may not speak.
						var data sdk.View
						data, msg.version, msg.viewErr = fetchView(ctx, client, id, sdk.PresentationVersion)
						if msg.viewErr == nil {
							msg.data = &data
						}
					} else {
						var data sdk.View
						msg.viewErr = client.Call(ctx, "app/"+id+"/view", sdk.ViewRequest{Version: version}, &data)
						if msg.viewErr == nil {
							msg.viewErr = data.Validate()
						}
						if msg.viewErr == nil && data.Version > version {
							msg.viewErr = fmt.Errorf("presentation: view stamped version %d exceeds requested %d", data.Version, version)
						}
						if msg.viewErr == nil {
							msg.data, msg.version = &data, data.Version
						}
					}
					break
				}
			}
		}
		return msg
	}
}

func (m *Model) applyView(data sdk.View) bool {
	v := m.presentation
	if v.instance != "" && v.instance != data.Instance {
		return false
	}
	var itemID, fieldID, actionID, cellID string
	if v.item < len(v.data.Items) {
		itemID = v.data.Items[v.item].ID
	}
	if v.focus < len(v.data.Fields) {
		fieldID = v.data.Fields[v.focus].ID
	} else if index := v.focus - len(v.data.Fields); index < len(v.data.Actions) {
		actionID = v.data.Actions[index].ID
	}
	if v.data.Grid != nil && v.cell < len(v.data.Grid.Cells) {
		cellID = v.data.Grid.Cells[v.cell].ID
	}
	var regionID string
	if v.region() != nil {
		regionID = v.region().ID
	}
	wasGrid := v.gridFocused()
	wasRegion := v.regionFocused()
	v.instance, v.data = data.Instance, data
	v.focus, v.item, v.cell = 0, 0, 0
	fields := make(map[string]string)
	for i, f := range data.Fields {
		value, exists := v.values[f.ID]
		if !exists {
			value = f.Value
		}
		fields[f.ID] = value
		if f.ID == fieldID {
			v.focus = i
		}
	}
	for i, a := range data.Actions {
		if a.ID == actionID {
			v.focus = len(data.Fields) + i
		}
	}
	for i, item := range data.Items {
		if item.ID == itemID {
			v.item = i
		}
	}
	if data.Grid != nil {
		for i, c := range data.Grid.Cells {
			if c.ID == cellID {
				v.cell = i
				break
			}
		}
	}
	if wasGrid && v.grid() != nil {
		v.focus = len(data.Fields) + len(data.Actions)
	}
	if wasRegion && v.region() != nil {
		v.focus = v.regionIndex()
	}
	// A different region resets scroll to the tail; the same one keeps the
	// user's position (clamped at render) or stays pinned.
	if v.region() == nil || v.region().ID != regionID {
		v.regionTop, v.regionPinned = 0, true
	}
	v.values = fields
	return true
}

func (m *Model) updatePresentation(msg tea.Msg) tea.Cmd {
	v := m.presentation
	switch msg := msg.(type) {
	case viewSubscribedMsg:
		if msg.err == nil && msg.sub != nil {
			m.viewSub = msg.sub
			return m.waitViewEvent(msg.sub)
		}
		m.viewSub = nil
		return nil
	case viewEventMsg:
		if !msg.ok {
			// The stream ended (unsubscribe or disconnect); the next
			// openView retries the subscription.
			m.viewSub = nil
			return nil
		}
		var wait tea.Cmd
		if m.viewSub != nil {
			wait = m.waitViewEvent(m.viewSub)
		}
		if v == nil || v.instance == "" || msg.event.Event.Type != viewEventTopic ||
			msg.event.Event.Source != "app/"+v.id {
			return wait
		}
		// A matching push coalesces into at most one in-flight refetch:
		// while an action or earlier refresh runs, the action's own reply
		// already freshens the view and one queued refresh follows.
		if v.busy || v.refreshing {
			v.refreshQueued = true
			return wait
		}
		v.refreshQueued = false
		return tea.Batch(wait, m.refreshView())
	case viewMsg:
		if v == nil {
			return nil
		}
		if msg.refresh {
			v.refreshing = false
		}
		if msg.epoch != v.epoch || msg.seq != v.seq {
			return nil
		}
		if msg.action {
			v.busy = false
			if v.cancel != nil {
				v.cancel()
				v.cancel = nil
			}
		}
		if msg.err != nil {
			v.banner = safe(msg.err.Error())
			if v.instance == "" {
				v.data.State = sdk.ViewError
			}
		} else {
			v.version = msg.version
			if v.version == 0 {
				v.version = msg.data.Version
			}
			if !m.applyView(msg.data) {
				m.append(entry{"App instance changed; view closed.", "error"})
				return m.dismissView()
			}
			v.banner = ""
		}
		if msg.action && v.id == "com.gostalgia.settings" {
			return m.fetchConfigCmd()
		}
		if v.refreshQueued && !v.busy {
			v.refreshQueued = false
			return m.refreshView()
		}
	case viewTickMsg:
		if v != nil && msg.epoch == v.epoch {
			return m.checkView()
		}
	case viewCheckMsg:
		if v == nil || msg.epoch != v.epoch {
			return nil
		}
		alive := false
		for _, a := range msg.apps {
			alive = alive || a.Manifest.ID == v.id && a.Running && a.PID == v.pid
		}
		if msg.err != nil || !alive {
			text := "App exited or stopped; view closed."
			if msg.err != nil {
				text = "App view unavailable: " + msg.err.Error()
			}
			m.append(entry{text, "error"})
			return m.dismissView()
		}
		m.apps = msg.apps
		if msg.seq == v.seq && msg.viewErr != nil {
			v.banner = safe(msg.viewErr.Error())
		}
		if msg.seq == v.seq && msg.data != nil {
			if msg.version != 0 {
				v.version = msg.version
			}
			if !m.applyView(*msg.data) {
				m.append(entry{"App instance changed; view closed.", "error"})
				return m.dismissView()
			}
		}
		return viewTick(v.epoch)
	case viewCancelMsg:
		if msg.err != nil {
			m.append(entry{"Cancel request: " + msg.err.Error(), "error"})
		}
	}
	return nil
}

// gridFocused reports whether the tab ring rests on the grid stop. The grid
// sits after fields and actions; the version-3 scroll region is the only stop
// past it.
func (v *appView) grid() *sdk.Grid {
	if g := v.data.Grid; g != nil && len(g.Cells) > 0 {
		return g
	}
	return nil
}

func (v *appView) gridFocused() bool {
	return v.grid() != nil && v.focus == len(v.data.Fields)+len(v.data.Actions)
}

// region returns the version-3 scroll region when the snapshot carries one.
func (v *appView) region() *sdk.Region {
	return v.data.Region
}

// regionIndex is the region's focus-ring position: last, after the grid stop.
func (v *appView) regionIndex() int {
	idx := len(v.data.Fields) + len(v.data.Actions)
	if v.grid() != nil {
		idx++
	}
	return idx
}

func (v *appView) regionFocused() bool {
	return v.region() != nil && v.focus == v.regionIndex()
}

// textLines splits text the way the renderer and the validator count it: a
// trailing newline terminates the last line. Control sequences are dropped.
func textLines(text string) []string {
	return strings.Split(strings.TrimSuffix(safe(text), "\n"), "\n")
}

// scrollRegion moves the region window by delta lines; reaching the tail
// re-pins the view to live output.
func (v *appView) scrollRegion(delta int) {
	r := v.region()
	if r == nil || v.regionWindow <= 0 {
		return
	}
	maxTop := max(0, len(textLines(r.Text))-v.regionWindow)
	v.regionTop = min(maxTop, max(0, v.regionTop+delta))
	v.regionPinned = v.regionTop >= maxTop
}

// topActionDisabled reports whether a same-named top-level action is
// disabled; cell and list actions share one namespace, so the flag vetoes a
// cell's declaration too (the SDK enforces the same rule).
func (v *appView) topActionDisabled(id string) bool {
	for _, a := range v.data.Actions {
		if a.ID == id {
			return a.Disabled
		}
	}
	return false
}

// moveCell walks the selection clamped to row/column edges; a short final row
// lands on its last cell.
func (v *appView) moveCell(dcol, drow int) {
	g := v.grid()
	if g == nil {
		return
	}
	cols := g.Columns
	rows := (len(g.Cells) + cols - 1) / cols
	col := min(cols-1, max(0, v.cell%cols+dcol))
	row := min(rows-1, max(0, v.cell/cols+drow))
	v.cell = min(len(g.Cells)-1, row*cols+col)
}

// viewRun dispatches a semantic action for the focused action or cell. Callers
// set Action, CellID, and ItemID; the request picks up the negotiated version,
// instance, a bounded request ID, and current field values.
func (m *Model) viewRun(p sdk.ActionRequest) tea.Cmd {
	v := m.presentation
	p.Version, p.Instance = v.version, v.instance
	p.RequestID = "r" + strings.ToLower(rand.Text())
	p.Values = make(map[string]string, len(v.values))
	for id, value := range v.values {
		p.Values[id] = value
	}
	v.seq++
	v.request, v.busy, v.banner = p.RequestID, true, ""
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	v.cancel = cancel
	epoch, seq, id, client := v.epoch, v.seq, v.id, m.client
	return func() tea.Msg {
		defer cancel()
		var data sdk.View
		err := client.Call(ctx, "app/"+id+"/action", p, &data)
		if err == nil {
			err = data.Validate()
			if err == nil && data.Version > p.Version {
				err = fmt.Errorf("presentation: view stamped version %d exceeds requested %d", data.Version, p.Version)
			}
		} else {
			// A timeout/disconnect of the local wait is not server-side
			// cancellation. Best-effort cancel using a fresh deadline.
			cancelCtx, stop := context.WithTimeout(context.WithoutCancel(m.ctx), 2*time.Second)
			_ = client.Call(cancelCtx, "app/"+id+"/cancel", sdk.CancelRequest{
				Version: p.Version, Instance: p.Instance, RequestID: p.RequestID,
			}, nil)
			stop()
		}
		return viewMsg{epoch: epoch, seq: seq, version: p.Version, data: data, err: err, action: true}
	}
}

func (m *Model) viewKey(key tea.KeyPressMsg) tea.Cmd {
	v := m.presentation
	if key.String() == "esc" {
		if v.busy {
			return m.cancelViewAction(v)
		}
		return m.dismissView()
	}
	if v.busy || v.instance == "" {
		return nil
	}
	fields, actions := v.data.Fields, v.data.Actions
	total := len(fields) + len(actions)
	if v.grid() != nil {
		total++
	}
	if v.region() != nil {
		total++
	}
	fieldFocused := v.focus < len(fields)
	switch key.String() {
	case "tab", "shift+tab":
		v.itemFocus, m.scroll = false, 0
		if total > 0 {
			delta := 1
			if key.String() == "shift+tab" {
				delta = total - 1
			}
			v.focus = (v.focus + delta) % total
		}
	case "up":
		switch {
		case v.gridFocused():
			v.moveCell(0, -1)
		case v.regionFocused():
			v.scrollRegion(-1)
		default:
			v.itemFocus = true
			v.item = max(0, v.item-1)
		}
		m.scroll = 0
	case "down":
		switch {
		case v.gridFocused():
			v.moveCell(0, 1)
		case v.regionFocused():
			v.scrollRegion(1)
		default:
			v.itemFocus = true
			v.item = min(max(0, len(v.data.Items)-1), v.item+1)
		}
		m.scroll = 0
	case "pgup":
		if v.regionFocused() {
			v.scrollRegion(-v.regionWindow)
			m.scroll = 0
		}
	case "pgdown":
		if v.regionFocused() {
			v.scrollRegion(v.regionWindow)
			m.scroll = 0
		}
	case "left":
		if v.gridFocused() {
			v.moveCell(-1, 0)
			m.scroll = 0
		}
	case "right":
		if v.gridFocused() {
			v.moveCell(1, 0)
			m.scroll = 0
		}
	case "enter", "shift+enter", "ctrl+enter":
		if v.data.State == sdk.ViewLoading {
			return nil
		}
		if fieldFocused && fields[v.focus].Multiline && !key.Mod.Contains(tea.ModCtrl) {
			// In a version-3 multiline field Enter edits like a newline —
			// and Shift-Enter with it, matching the Notes-style chord;
			// terminals without key disambiguation report both as plain
			// Enter, so the chord is identical everywhere. Ctrl-Enter
			// skips newline insertion and dispatches below, giving
			// multiline forms a submit chord that only a disambiguating
			// terminal can name.
			v.itemFocus, m.scroll = false, 0
			v.values[fields[v.focus].ID] += "\n"
			return nil
		}
		if key.IsRepeat {
			// A held activation key commits once, not once per
			// auto-repeat: kitty event types flag the repeats and the
			// closing KeyReleaseMsg is swallowed in Update. Held-key
			// interactions in the bundled apps — Adventure's command
			// field, Musictoy's step grid, Petwatch's task entry — can no
			// longer storm the action route. Terminals without event
			// types never flag repeats, so a held Enter there repeats
			// exactly as before.
			return nil
		}
		if v.regionFocused() {
			return nil
		}
		if v.gridFocused() {
			c := v.grid().Cells[v.cell]
			if c.Disabled || c.Action == "" || v.topActionDisabled(c.Action) {
				return nil
			}
			return m.viewRun(sdk.ActionRequest{Action: c.Action, CellID: c.ID})
		}
		index := v.focus - len(fields)
		if index < 0 {
			index = 0
		}
		if index >= len(actions) || actions[index].Disabled {
			return nil
		}
		p := sdk.ActionRequest{Action: actions[index].ID}
		if len(v.data.Items) > 0 {
			p.ItemID = v.data.Items[v.item].ID
		}
		// A disabled cell's id is meaningless input the SDK rejects; omit it
		// so list actions keep working while one is selected.
		if g := v.grid(); g != nil && !g.Cells[v.cell].Disabled {
			p.CellID = g.Cells[v.cell].ID
		}
		return m.viewRun(p)
	default:
		if !fieldFocused {
			return nil
		}
		// Field editing stays custom rather than adopting bubbles/textarea
		// (issue #185). Upstream owns an ↑↓/Tab/Enter keymap that collides
		// with the view's focus ring — items keep ↑↓ while a field is
		// focused, and Enter in a multiline field must stay a newline —
		// and it edits per rune, not per grapheme cluster. Field values
		// also stay shell-owned drafts keyed by field ID, surviving view
		// refreshes, which upstream's internal buffer can't express.
		f := fields[v.focus]
		id := f.ID
		v.itemFocus, m.scroll = false, 0
		value := []rune(v.values[id])
		limit := 4096
		if f.Multiline {
			limit = 32768 // matches the SDK maxAreaValue budget
		}
		k := key.Key()
		// Text-bearing keys insert before control-chord handling so Windows
		// AltGr chords (Ctrl+Alt with text) reach the field.
		switch text := safe(k.Text); {
		case text != "":
			if !f.Multiline {
				text = strings.ReplaceAll(text, "\n", " ")
			}
			if len(string(value))+len(text) <= limit {
				value = append(value, []rune(text)...)
			}
		case k.Code == tea.KeyBackspace || (k.Mod.Contains(tea.ModCtrl) && k.Code == 'h'):
			if len(value) > 0 {
				value = value[:len(value)-1]
			}
		case k.Mod.Contains(tea.ModCtrl) && k.Code == 'u':
			value = nil
		}
		v.values[id] = string(value)
	}
	return nil
}

func viewText(s string) string { return strings.ReplaceAll(safe(s), "\n", " ") }

func (m *Model) viewLines(height int) []string {
	v := m.presentation
	hints := "Tab focus · Enter action · ↑↓ item · Esc cancel/back"
	switch {
	case v.grid() != nil && v.region() != nil:
		hints = "Tab focus · Enter action · ↑↓ item/region · ←→↑↓ grid · Esc cancel/back"
	case v.grid() != nil:
		hints = "Tab focus · Enter action · ↑↓ item · ←→↑↓ grid · Esc cancel/back"
	case v.region() != nil:
		hints = "Tab focus · Enter action · ↑↓ item/region · Esc cancel/back"
	}
	for _, f := range v.data.Fields {
		if f.Multiline {
			// A multiline field turns Enter into a newline, so the action
			// hint names the Ctrl-Enter submit chord instead.
			hints = strings.Replace(hints, "Enter action", "Enter/Shift-Enter newline · Ctrl-Enter action", 1)
			break
		}
	}
	lines := []string{m.kit.Heading(viewText(v.data.Title)), m.kit.Muted(hints)}
	if v.busy || v.data.State == sdk.ViewLoading {
		lines = append(lines, m.kit.Badge("Loading…", theme.Busy, max(0, m.width-4)))
	}
	if v.banner != "" {
		lines = append(lines, m.kit.StatusText(viewText(v.banner), theme.Error))
	}
	if v.data.Error != "" {
		lines = append(lines, m.kit.StatusText(viewText(v.data.Error), theme.Error))
	}
	var rows []string
	for _, block := range v.data.Blocks {
		if block.Label != "" {
			rows = append(rows, m.kit.Muted(viewText(block.Label)))
		}
		for _, line := range strings.Split(safe(block.Text), "\n") {
			rows = append(rows, m.kit.Text(line))
		}
	}
	for _, meter := range v.data.Meters {
		rows = append(rows, m.kit.Progress(ui.Progress{
			Label: meter.Label, Value: meter.Value,
		}, max(1, m.width-8)))
	}
	itemBase := len(rows)
	for i, item := range v.data.Items {
		text := viewText(item.Label)
		if item.Detail != "" {
			text = viewText(item.Label + ": " + item.Detail)
		}
		if i == v.item {
			text = m.kit.FocusText(text)
		} else {
			text = m.kit.Text(text)
		}
		rows = append(rows, text)
	}
	formBase := len(rows)
	for i, field := range v.data.Fields {
		label := viewText(field.Label) + ": "
		if field.Multiline {
			// Version-3 multiline field: label row then each value line
			// verbatim. Focus marks the label; shell still owns the draft.
			if i == v.focus {
				rows = append(rows, m.kit.FocusText(label+" "))
			} else {
				rows = append(rows, m.kit.Text(label))
			}
			for _, line := range textLines(v.values[field.ID]) {
				rows = append(rows, m.kit.Text("  "+line))
			}
			continue
		}
		value := viewText(v.values[field.ID])
		room := max(1, m.width-8-lipgloss.Width(label))
		text := label + ui.Tail(value, room)
		if i == v.focus {
			text = m.kit.FocusText(text + " ")
		} else {
			text = m.kit.Text(text)
		}
		rows = append(rows, text)
	}
	for i, action := range v.data.Actions {
		text := "[" + viewText(action.Label) + "]"
		if action.Disabled || v.busy || v.data.State == sdk.ViewLoading {
			text += " (disabled)"
			text = m.kit.StatusText(text, theme.Disabled)
		} else if len(v.data.Fields)+i == v.focus {
			text = m.kit.FocusText(text)
		} else {
			text = m.kit.Text(text)
		}
		rows = append(rows, text)
	}
	gridBase, gridRow := -1, 0
	if g := v.grid(); g != nil {
		if g.Label != "" {
			rows = append(rows, m.kit.Muted(viewText(g.Label)))
		}
		gridBase, gridRow = len(rows), v.cell/g.Columns
		focused := v.gridFocused()
		for row := 0; row*g.Columns < len(g.Cells); row++ {
			var cells []string
			for col := 0; col < g.Columns; col++ {
				i := row*g.Columns + col
				if i >= len(g.Cells) {
					break
				}
				c := g.Cells[i]
				text := "[" + viewText(c.Label) + "]"
				switch {
				case c.Disabled:
					text += " (disabled)"
					if i == v.cell {
						// Keep the cursor visible on a disabled cell: theme
						// marker plus the dimmed label.
						text = m.kit.StatusText(m.kit.Theme().Focus.Marker+" "+text, theme.Disabled)
					} else {
						text = m.kit.StatusText(text, theme.Disabled)
					}
				case focused && i == v.cell:
					text = m.kit.FocusText(text)
				case i == v.cell:
					// Selection has no fill in plain/monochrome; the theme
					// marker keeps the selected cell visible, matching item
					// selection.
					text = m.kit.Text(m.kit.Theme().Focus.Marker + " " + text)
				default:
					text = m.kit.Text(text)
				}
				cells = append(cells, text)
			}
			rows = append(rows, strings.Join(cells, " "))
		}
	}
	visible := max(1, height-len(lines)-1)
	regionBase := -1
	if r := v.region(); r != nil {
		all := textLines(r.Text)
		v.regionWindow = min(20, max(4, visible/2))
		// The region delegates its clamp to bubbles/viewport (issue #185);
		// regionPinned means "stay at the tail", so GotoBottom is the pin.
		// The regionTop field stays canonical: scrollRegion moves it between
		// renders and the tests observe it directly.
		vp := viewport.New(viewport.WithHeight(v.regionWindow))
		vp.SetContentLines(all)
		if v.regionPinned {
			vp.GotoBottom()
		} else {
			vp.SetYOffset(v.regionTop)
		}
		v.regionTop = vp.YOffset()
		label := viewText(r.Label)
		if len(all) > v.regionWindow {
			label += " " + m.kit.Muted(fmt.Sprintf("– %d/%d", v.regionTop+1, len(all)))
		}
		if v.regionFocused() {
			rows = append(rows, m.kit.FocusText(label))
		} else {
			rows = append(rows, m.kit.Muted(label))
		}
		regionBase = len(rows)
		for _, line := range all[v.regionTop:min(len(all), v.regionTop+v.regionWindow)] {
			rows = append(rows, m.kit.Text(line))
		}
	}
	focus := formBase + v.focus
	switch {
	case v.itemFocus:
		focus = itemBase + v.item
	case regionBase >= 0 && v.regionFocused():
		focus = len(rows) - 1
	case gridBase >= 0 && v.gridFocused():
		focus = gridBase + gridRow
	}
	start := max(0, min(focus-visible+1, max(0, len(rows)-visible))-m.scroll)
	lines = append(lines, rows[start:min(len(rows), start+visible)]...)
	lines = append(lines, m.kit.Muted(viewText(v.data.Status)))
	return lines
}

func (m *Model) selectedView() tea.Cmd {
	if m.busy {
		return nil
	}
	if m.currentMode() == modeLauncher {
		filtered := m.filteredApps()
		if len(filtered) > 0 && m.selected < len(filtered) && filtered[m.selected].Running {
			return m.openView(filtered[m.selected])
		}
	}
	if m.currentMode() == modeHome {
		items := m.homeItems()
		if len(items) > 0 && m.homeSelected < len(items) && items[m.homeSelected].kind == homeItemApp {
			return m.openView(items[m.homeSelected].app)
		}
	}
	if len(m.apps) > 0 && m.selected < len(m.apps) && m.apps[m.selected].Running {
		return m.openView(m.apps[m.selected])
	}
	for _, a := range m.apps {
		if a.Running {
			return m.openView(a)
		}
	}
	m.append(entry{"Launch an app before opening its view.", "error"})
	return nil
}
