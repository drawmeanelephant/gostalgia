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
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/experience/windows"
	"gostalgia/internal/ipc"
	"gostalgia/sdk"
)

// appView is one window's shell-owned presentation state. No app supplies a
// tea.Model, keymap, style, or terminal handle. Epoch/sequence guards reject
// replies after the window closes, a cancellation, or a later operation.
// version is the negotiated presentation protocol; cell is the selected grid
// cell when a version-2 grid exists. regionTop/regionPinned/regionWindow drive
// the version-3 scroll region; the window is written by the renderer so key handling clamps against the same
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
	// scroll is how many rows the user paged the flow up from its focus.
	scroll int

	regionTop    int
	regionPinned bool
	regionWindow int
	// baseRegion is the view's ordinary output surface; surface names a
	// different region an action just revealed (such as a Help panel). While
	// set, the secondary pane shows that surface from the top instead of the
	// control window; ordinary navigation clears it. revealed remembers which
	// regions actions have surfaced so a later poll cannot promote a panel to
	// the ordinary surface while the user has navigated away from it.
	baseRegion string
	surface    string
	revealed   map[string]bool

	// inspect gives a hinted focal block the whole body; inspectTop is its
	// first visible line (-1 re-centers on the focal cell). blockTop and
	// blockWindow are the resolved window the last frame drew, so arrow
	// scrolling clamps against what the user actually sees.
	inspect     bool
	inspectTop  int
	blockTop    int
	blockWindow int

	refreshing    bool
	refreshQueued bool
	draftSeq      uint64
}

type viewMsg struct {
	id           windows.ID
	epoch, seq   uint64
	draftSeq     uint64
	version      int
	data         sdk.View
	err          error
	action       bool
	actionReq    sdk.ActionRequest
	retriedGrant bool
	refresh      bool
}

type grantPathMsg struct {
	id         windows.ID
	epoch, seq uint64
	retry      sdk.ActionRequest
	err        error
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

type viewTickMsg struct {
	id    windows.ID
	epoch uint64
}
type viewCheckMsg struct {
	id         windows.ID
	epoch, seq uint64
	draftSeq   uint64
	apps       []appStatus
	data       *sdk.View
	version    int
	err        error
	viewErr    error
}
type viewCancelMsg struct{ err error }

// primaryMinRows is the smallest content budget that can show a hinted focal
// block beside usable secondary controls. Below it — 30x10 leaves one content
// row — the shell keeps controls on screen and exposes the block through the
// explicit inspection route instead.
const primaryMinRows = 10

// secondaryControlRows is the part of the split body reserved for the focused
// control itself; the rest is focus-windowed content.
const secondaryControlRows = 2

// presentationPanes is an app body's vertical budget. primary is the bounded
// focal-block viewport, meters are pinned vitals under it, and secondary is
// the independently focus-windowed flow. Compact bodies put everything in
// secondary and show the focal block only while inspecting.
type presentationPanes struct {
	inspect   bool
	primary   int
	meters    int
	secondary int
}

// fetchView requests a snapshot at the newest version the shell supports and
// steps down one version at a time while the app reports an unsupported
// version, so an app speaking only an older contract keeps rendering and a
// future newer shell does not strand mid-version apps at the oldest contract.
// The fallback matches the structured error code first, then the error text
// for apps still speaking the pre-code protocol; keep the message check this
// narrow. The stamped snapshot version is the negotiated version callers
// reuse on action and cancel requests. A reply stamped newer than requested
// is rejected: an app speaking raw IPC must not serve elements the shell
// never asked for.
func fetchView(ctx context.Context, client Caller, id string, version int) (sdk.View, int, error) {
	var data sdk.View
	for {
		err := client.Call(ctx, "app/"+id+"/view", sdk.ViewRequest{Version: version}, &data)
		if err != nil {
			unsupported := ipc.ErrorCode(err) == ipc.CodeUnsupportedVersion ||
				strings.Contains(err.Error(), "unsupported version")
			if version > sdk.MinPresentationVersion && unsupported {
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

func viewTick(id windows.ID, epoch uint64) tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return viewTickMsg{id, epoch} })
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

// liveValues returns a fresh copy of v's current drafts for fields marked Live
// on a version-3+ presentation, or nil when no live fields exist.
func (v *appView) liveValues() map[string]string {
	if v.version < 3 {
		return nil
	}
	var values map[string]string
	for _, f := range v.data.Fields {
		if !f.Live {
			continue
		}
		if values == nil {
			values = make(map[string]string)
		}
		values[f.ID] = v.values[f.ID]
	}
	return values
}

// queueLiveRefresh schedules a coalesced non-blocking view refresh after a
// live field edit so snapshot metadata tracks the visible draft without
// blocking typing or flashing a loading badge.
func (m *Model) queueLiveRefresh(v *appView) tea.Cmd {
	v.draftSeq++
	if v.busy || v.refreshing {
		v.refreshQueued = true
		return nil
	}
	v.refreshQueued = false
	return m.refreshView(v)
}

// refreshView refetches v's snapshot at the negotiated version in answer to
// a version-3 invalidation push or live field edit. Unlike the poll it skips
// app/list: liveness keeps coming from the tick.
func (m *Model) refreshView(v *appView) tea.Cmd {
	v.refreshing = true
	epoch, seq, draftSeq, id, version := v.epoch, v.seq, v.draftSeq, v.id, v.version
	values := v.liveValues()
	ctx, client := m.ctx, m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var data sdk.View
		err := client.Call(ctx, "app/"+id+"/view", sdk.ViewRequest{Version: version, Values: values}, &data)
		if err == nil {
			err = data.Validate()
		}
		if err == nil && data.Version > version {
			err = fmt.Errorf("presentation: view stamped version %d exceeds requested %d", data.Version, version)
		}
		return viewMsg{id: windows.ID(id), epoch: epoch, seq: seq, draftSeq: draftSeq, version: version, data: data, err: err, refresh: true}
	}
}

// cancelViewAction cancels v's in-flight action. Cancel uses a separate IPC
// call because canceling a socket client's wait alone cannot stop server work.
func (m *Model) cancelViewAction(v *appView) tea.Cmd {
	v.cancel()
	v.cancel = nil
	v.busy = false
	v.seq++
	v.banner = "Canceled. Work already committed cannot be undone."
	if v.request == "" {
		return nil
	}
	params := sdk.CancelRequest{Version: v.version, Instance: v.instance, RequestID: v.request}
	ctx, client, id := context.WithoutCancel(m.ctx), m.client, v.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		err := client.Call(ctx, "app/"+id+"/cancel", params, nil)
		return viewCancelMsg{err}
	}
}

func (m *Model) checkView(v *appView) tea.Cmd {
	epoch, seq, draftSeq, id, pid, busy, version := v.epoch, v.seq, v.draftSeq, v.id, v.pid, v.busy, v.version
	values := v.liveValues()
	negotiated := v.instance != ""
	ctx, client := m.ctx, m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		msg := viewCheckMsg{id: windows.ID(id), epoch: epoch, seq: seq, draftSeq: draftSeq}
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
						msg.viewErr = client.Call(ctx, "app/"+id+"/view", sdk.ViewRequest{Version: version, Values: values}, &data)
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

// apply adopts a snapshot, preserving semantic selection by identity.
// fromAction marks a reply to a user action: an action that reveals a
// different output surface than the view's ordinary one surfaces it from the
// top, so the panel the user asked for is actually readable.
func (v *appView) apply(data sdk.View, fromAction bool) bool {
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
	// The first region an ordinary snapshot shows is its output surface; a
	// panel revealed later by an action is distinguishable from it. A poll
	// must not promote a revealed panel to the ordinary surface, or toggling
	// it shut would look like opening it.
	if v.region() != nil && !fromAction && v.surface == "" &&
		v.baseRegion == "" && !v.revealed[v.region().ID] {
		v.baseRegion = v.region().ID
	}
	regionChanged := v.region() != nil && v.region().ID != regionID
	// A different region resets scroll to the tail; the same one keeps the
	// user's position (clamped at render) or stays pinned.
	if v.region() == nil || regionChanged {
		v.regionTop, v.regionPinned = 0, true
	}
	if fromAction {
		switch {
		case v.region() == nil:
			v.surface = ""
		case regionChanged && v.baseRegion != "" && v.region().ID != v.baseRegion && v.focus >= len(data.Fields):
			// A newly revealed panel is the action's result: show it from its
			// first line rather than the tail, without moving focus away from
			// the action that can toggle it again.
			if v.revealed == nil {
				v.revealed = make(map[string]bool)
			}
			v.surface = v.region().ID
			v.revealed[v.region().ID] = true
			v.regionTop, v.regionPinned = 0, false
		case regionChanged:
			v.surface = ""
		}
	}
	v.values = fields
	return true
}

// updatePresentation routes a window's replies, ticks, and pushes by window
// ID. Each window has its own epoch and request sequence, so a late reply for
// one window can never change another, or a window opened again since.
func (m *Model) updatePresentation(msg tea.Msg) tea.Cmd {
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
			// window to open retries the subscription.
			m.viewSub = nil
			return nil
		}
		var wait tea.Cmd
		if m.viewSub != nil {
			wait = m.waitViewEvent(m.viewSub)
		}
		appID, ok := strings.CutPrefix(msg.event.Event.Source, "app/")
		v := m.views[windows.ID(appID)]
		if !ok || v == nil || v.instance == "" || msg.event.Event.Type != viewEventTopic {
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
		return tea.Batch(wait, m.refreshView(v))
	case viewMsg:
		v := m.views[msg.id]
		if v == nil {
			return nil
		}
		if msg.refresh && msg.epoch == v.epoch {
			v.refreshing = false
		}
		if msg.epoch != v.epoch || msg.seq != v.seq {
			if msg.epoch == v.epoch && v.refreshQueued && !v.busy && !v.refreshing {
				v.refreshQueued = false
				return m.refreshView(v)
			}
			return nil
		}
		if msg.action {
			v.busy = false
			if v.cancel != nil {
				v.cancel()
				v.cancel = nil
			}
		}
		var cmds []tea.Cmd
		if msg.err != nil {
			v.banner = safe(msg.err.Error())
			if v.instance == "" {
				v.data.State = sdk.ViewError
			}
		} else if !msg.refresh || msg.draftSeq == v.draftSeq || !v.refreshQueued {
			v.version = msg.version
			if v.version == 0 {
				v.version = msg.data.Version
			}
			if !v.apply(msg.data, msg.action) {
				m.append(entry{"App instance changed; window closed.", "error"})
				return m.closeWindow(msg.id)
			}
			m.windows.Retitle(msg.id, viewText(v.data.Title))
			m.pruneGridHover()
			v.banner = ""
			if msg.action && !msg.retriedGrant && msg.data.GrantRequest != nil {
				if cmd := m.openGrantConfirm(v, msg.actionReq, *msg.data.GrantRequest); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		if msg.action && v.id == settingsAppID {
			cmds = append(cmds, m.fetchConfigCmd())
		} else if v.refreshQueued && !v.busy && !v.refreshing {
			v.refreshQueued = false
			cmds = append(cmds, m.refreshView(v))
		}
		if len(cmds) > 0 {
			return tea.Batch(cmds...)
		}
	case grantPathMsg:
		v := m.views[msg.id]
		if v == nil || msg.epoch != v.epoch || msg.seq != v.seq {
			return nil
		}
		v.busy = false
		if v.cancel != nil {
			v.cancel()
			v.cancel = nil
		}
		if msg.err != nil {
			v.banner = safe(msg.err.Error())
			return nil
		}
		return m.dispatchViewAction(v, msg.retry, true)
	case viewTickMsg:
		if v := m.views[msg.id]; v != nil && msg.epoch == v.epoch {
			return m.checkView(v)
		}
	case viewCheckMsg:
		v := m.views[msg.id]
		if v == nil || msg.epoch != v.epoch {
			return nil
		}
		alive := false
		for _, a := range msg.apps {
			alive = alive || a.Manifest.ID == v.id && a.Running && a.PID == v.pid
		}
		if msg.err != nil || !alive {
			text := "App exited or stopped; window closed."
			if msg.err != nil {
				text = "App view unavailable: " + msg.err.Error()
			}
			m.append(entry{text, "error"})
			return m.closeWindow(msg.id)
		}
		m.apps = msg.apps
		// A fresh app/list answer outranks an older process-derived state;
		// the next accepted poll reconciles it again.
		m.procApps = nil
		if msg.seq == v.seq && msg.draftSeq == v.draftSeq && msg.viewErr != nil {
			v.banner = safe(msg.viewErr.Error())
		}
		if msg.seq == v.seq && msg.draftSeq == v.draftSeq && msg.data != nil {
			if msg.version != 0 {
				v.version = msg.version
			}
			if !v.apply(*msg.data, false) {
				m.append(entry{"App instance changed; window closed.", "error"})
				return m.closeWindow(msg.id)
			}
			m.windows.Retitle(msg.id, viewText(v.data.Title))
			m.pruneGridHover()
		}
		return viewTick(msg.id, v.epoch)
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

// focalBlock returns the first block carrying optional focus metadata. Only a
// hinted block becomes a bounded primary viewport; unhinted blocks keep their
// existing place and semantics, so an app that predates the datum — or any
// ordinary art block — renders exactly as before.
func (v *appView) focalBlock() *sdk.Block {
	if i := v.focalBlockIndex(); i >= 0 {
		return &v.data.Blocks[i]
	}
	return nil
}

func (v *appView) focalBlockIndex() int {
	for i := range v.data.Blocks {
		if v.data.Blocks[i].Focus != nil {
			return i
		}
	}
	return -1
}

// scrollBlock moves an inspected block window. The window size and top come
// from the last frame, so scrolling never describes rows the user cannot see.
func (v *appView) scrollBlock(delta int) {
	block := v.focalBlock()
	if block == nil || v.blockWindow <= 0 {
		return
	}
	top := v.blockTop
	if v.inspectTop >= 0 {
		top = v.inspectTop
	}
	maxTop := max(0, len(textLines(block.Text))-v.blockWindow)
	v.inspectTop = min(maxTop, max(0, top+delta))
}

// viewPanes splits v's content height between a hinted focal block, pinned
// meters, and the focus-windowed secondary flow. A narrow body keeps controls
// whole and leaves the block to the inspection route.
func (m *Model) viewPanes(v *appView, width, height int) presentationPanes {
	visible := max(1, height-len(m.viewHeader(v, width, height))-1)
	block := v.focalBlock()
	if block == nil {
		return presentationPanes{secondary: visible}
	}
	if v.inspect {
		return presentationPanes{inspect: true, secondary: visible}
	}
	if visible < primaryMinRows {
		return presentationPanes{secondary: visible}
	}
	blockRows := len(textLines(block.Text))
	if block.Label != "" {
		blockRows++
	}
	meters := len(v.data.Meters)
	primary := min(blockRows, visible-meters-secondaryControlRows)
	if primary < 3 {
		return presentationPanes{secondary: visible}
	}
	return presentationPanes{primary: primary, meters: meters, secondary: visible - primary - meters}
}

// blockWindowTop resolves the first visible line of a block window. A
// negative top centers the focal line and clamps at the block's edges; a
// supplied top keeps the user's inspection offset inside the same bounds.
func blockWindowTop(lineCount, focusY, rows, top int) int {
	if rows <= 0 {
		return 0
	}
	maxTop := max(0, lineCount-rows)
	if top >= 0 {
		return min(maxTop, max(0, top))
	}
	return min(maxTop, max(0, focusY-rows/2))
}

// focusColumn converts a block focus's code-point column to display cells.
func focusColumn(line string, x int) int {
	if x <= 0 {
		return 0
	}
	runes := []rune(line)
	if x > len(runes) {
		x = len(runes)
	}
	return ui.GraphemeWidth(string(runes[:x]))
}

// clipBlockLine keeps width display cells of one sanitized block line,
// starting at left display cells, without splitting a grapheme cluster. A
// cluster straddling the left edge is dropped whole, as ui.Tail does.
func clipBlockLine(line string, left, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	col, used := 0, 0
	for _, cluster := range ui.GraphemeClusters(line) {
		w := ui.GraphemeWidth(cluster)
		if col+w <= left {
			col += w
			continue
		}
		if col < left {
			col += w
			continue
		}
		if used+w > width {
			break
		}
		b.WriteString(cluster)
		used += w
		col += w
		if used >= width {
			break
		}
	}
	return b.String()
}

// primaryBlockRows draws a hinted block in at most rows lines, keeping the
// focal cell visible both vertically and horizontally. The snapshot keeps the
// whole block; only this viewport crops it, width cells wide. inspect uses
// the user's scroll offset, while the ordinary split view always centers on
// the focal cell.
func (m *Model) primaryBlockRows(v *appView, block *sdk.Block, width, rows int, inspect bool) []string {
	if block == nil || rows <= 0 {
		v.blockTop, v.blockWindow = 0, max(0, rows)
		return nil
	}
	all := textLines(block.Text)
	label := viewText(block.Label)
	lineRows := rows
	if label != "" {
		lineRows = max(0, rows-1)
	}
	focusX, focusY := 0, 0
	if block.Focus != nil {
		focusX, focusY = block.Focus.X, block.Focus.Y
	}
	top := blockWindowTop(len(all), focusY, lineRows, -1)
	if inspect {
		top = blockWindowTop(len(all), focusY, lineRows, v.inspectTop)
	}
	v.blockTop, v.blockWindow = top, lineRows

	out := make([]string, 0, rows)
	if label != "" {
		out = append(out, m.kit.Muted(label))
	}
	width = max(1, width)
	focusLine := ""
	if focusY >= 0 && focusY < len(all) {
		focusLine = all[focusY]
	}
	left := 0
	if lineWidth := ui.GraphemeWidth(focusLine); lineWidth > width {
		left = min(lineWidth-width, max(0, focusColumn(focusLine, focusX)-width/2))
	}
	for i := top; i < min(len(all), top+lineRows); i++ {
		out = append(out, m.kit.Text(clipBlockLine(all[i], left, width)))
	}
	return out
}

// meterRow draws one gauge. The whole meter fits the content width and, per
// Delve's fixed contract, the whole meter never exceeds forty cells, so its
// bar can never sprawl past forty either.
func (m *Model) meterRow(meter sdk.Meter, width int) string {
	return m.kit.Progress(ui.Progress{Label: meter.Label, Value: meter.Value},
		max(1, min(40, width)))
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

// viewRun dispatches a semantic action for v's focused action or cell.
// Callers set Action, CellID, and ItemID; the request picks up the negotiated
// version, instance, a bounded request ID, and current field values.
func (m *Model) viewRun(v *appView, p sdk.ActionRequest) tea.Cmd {
	p.Values = make(map[string]string, len(v.values))
	for id, value := range v.values {
		p.Values[id] = value
	}
	return m.dispatchViewAction(v, p, false)
}

func (m *Model) dispatchViewAction(v *appView, p sdk.ActionRequest, retriedGrant bool) tea.Cmd {
	p.Version, p.Instance = v.version, v.instance
	p.RequestID = "r" + strings.ToLower(rand.Text())
	v.seq++
	v.request, v.busy, v.banner, v.refreshQueued = p.RequestID, true, "", false
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
		return viewMsg{
			id: windows.ID(id), epoch: epoch, seq: seq, version: p.Version,
			data: data, err: err, action: true, actionReq: p, retriedGrant: retriedGrant,
		}
	}
}

// grantPathAndRetry issues a single-path, non-recursive, session-bound VFS
// grant approved by the operator and then retries the original app action.
func (m *Model) grantPathAndRetry(c confirmation) tea.Cmd {
	v := m.views[c.grantWindow]
	if v == nil || v.epoch != c.grantEpoch {
		return nil
	}
	v.seq++
	v.request, v.busy, v.banner = "", true, ""
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	v.cancel = cancel
	epoch, seq, client := v.epoch, v.seq, m.client
	return func() tea.Msg {
		defer cancel()
		if c.grantAccess == "read-write" {
			var statOut struct {
				IsDir bool `json:"is_dir"`
			}
			if statErr := client.Call(ctx, "fs/stat", map[string]string{"path": c.grantPath}, &statOut); statErr == nil && statOut.IsDir {
				return grantPathMsg{
					id:    c.grantWindow,
					epoch: epoch,
					seq:   seq,
					err:   fmt.Errorf("cannot grant file write access to directory %s: include a file name (e.g. %s/note.txt)", c.grantPath, strings.TrimSuffix(c.grantPath, "/")),
				}
			}
		}
		if err := client.Call(ctx, "fs/grant", map[string]any{
			"app_id":        c.appID,
			"path":          c.grantPath,
			"access":        c.grantAccess,
			"recursive":     false,
			"session_bound": true,
		}, nil); err != nil {
			return grantPathMsg{
				id:    c.grantWindow,
				epoch: epoch,
				seq:   seq,
				err:   fmt.Errorf("could not grant %s access to %s: %w", c.grantAccess, c.grantPath, err),
			}
		}
		return grantPathMsg{id: c.grantWindow, epoch: epoch, seq: seq, retry: c.grantReq}
	}
}

// viewKey handles a key for v's presentation. The window keys, Esc among
// them, are routed before it.
func (m *Model) viewKey(v *appView, key tea.KeyPressMsg) tea.Cmd {
	if v.busy || v.instance == "" {
		return nil
	}
	// A revealed surface stays for reading until the next ordinary
	// navigation; Enter still reaches the focused action so the same key can
	// toggle a panel closed again.
	if v.surface != "" && key.String() != "enter" {
		v.surface = ""
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
	// A hinted block has a shell-owned inspection route. It is independent of
	// field editing: while a field owns the keyboard, "m" stays ordinary
	// text. Inspection itself keeps focus, pack selection, and drafts intact.
	if v.focalBlock() != nil && !fieldFocused {
		if v.inspect {
			switch key.String() {
			case "m":
				v.inspect = false
				v.inspectTop = -1
				v.scroll = 0
			case "up":
				v.scrollBlock(-1)
			case "down":
				v.scrollBlock(1)
			case "pgup":
				v.scrollBlock(-max(1, v.blockWindow))
			case "pgdown":
				v.scrollBlock(max(1, v.blockWindow))
			}
			return nil
		}
		if key.String() == "m" {
			v.inspect = true
			v.inspectTop = -1
			v.itemFocus = false
			v.scroll = 0
			return nil
		}
	}
	switch key.String() {
	case "tab", "shift+tab":
		v.itemFocus, v.scroll = false, 0
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
		v.scroll = 0
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
		v.scroll = 0
	case "pgup":
		if v.regionFocused() {
			v.scrollRegion(-v.regionWindow)
			v.scroll = 0
		}
	case "pgdown":
		if v.regionFocused() {
			v.scrollRegion(v.regionWindow)
			v.scroll = 0
		}
	case "left":
		if v.gridFocused() {
			v.moveCell(-1, 0)
			v.scroll = 0
		}
	case "right":
		if v.gridFocused() {
			v.moveCell(1, 0)
			v.scroll = 0
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
			v.itemFocus, v.scroll = false, 0
			f := fields[v.focus]
			if len(v.values[f.ID])+1 <= 32768 {
				v.values[f.ID] += "\n"
				if f.Live {
					return m.queueLiveRefresh(v)
				}
			}
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
			return m.runViewCell(v)
		}
		return m.runViewAction(v, max(0, v.focus-len(fields)))
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
		v.itemFocus, v.scroll = false, 0
		prev := v.values[id]
		value := []rune(prev)
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
		next := string(value)
		v.values[id] = next
		if f.Live && next != prev {
			return m.queueLiveRefresh(v)
		}
	}
	return nil
}

// runViewCell runs v's selected grid cell's action, as Enter does on it.
func (m *Model) runViewCell(v *appView) tea.Cmd {
	c := v.grid().Cells[v.cell]
	if c.Disabled || c.Action == "" || v.topActionDisabled(c.Action) {
		return nil
	}
	return m.viewRun(v, sdk.ActionRequest{Action: c.Action, CellID: c.ID})
}

// runViewAction runs v's action index, as Enter does on it, for the selected
// item and grid cell.
func (m *Model) runViewAction(v *appView, index int) tea.Cmd {
	actions := v.data.Actions
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
	return m.viewRun(v, p)
}

// viewTargetKind is what a click target in an app view is.
type viewTargetKind int

const (
	viewTargetAction viewTargetKind = iota + 1
	viewTargetCell
)

// viewTarget is an action or grid cell where viewLayout shows it: its line
// of the body and the cells across it it takes, with its index and ID in
// the view's snapshot.
type viewTarget struct {
	kind    viewTargetKind
	index   int
	id      string
	line, x int
	w       int
}

// activateViewTarget focuses t in v and runs it, as Tab to it and Enter
// do, if v's window is still open and t still in it. The gates Enter
// checks apply here too: nothing runs while an action is in flight or the
// view is loading, or for a disabled action or cell.
func (m *Model) activateViewTarget(v *appView, t viewTarget) tea.Cmd {
	if m.views[windows.ID(v.id)] != v || v.busy || v.instance == "" || v.data.State == sdk.ViewLoading {
		return nil
	}
	fields, actions := v.data.Fields, v.data.Actions
	switch t.kind {
	case viewTargetAction:
		if t.index >= len(actions) || actions[t.index].ID != t.id {
			return nil
		}
		v.focus, v.itemFocus, v.scroll = len(fields)+t.index, false, 0
		return m.runViewAction(v, t.index)
	case viewTargetCell:
		g := v.grid()
		if g == nil || t.index >= len(g.Cells) || g.Cells[t.index].ID != t.id {
			return nil
		}
		v.focus, v.cell, v.itemFocus, v.scroll = len(fields)+len(actions), t.index, false, 0
		return m.runViewCell(v)
	}
	return nil
}

func viewText(s string) string { return strings.ReplaceAll(safe(s), "\n", " ") }

// wrapErrorLines word-wraps an error or banner string to width cells across at
// most maxLines lines so causes and next steps remain readable without pushing
// interactive controls off screen.
func (m *Model) wrapErrorLines(text string, width, maxLines int) []string {
	clean := viewText(text)
	if clean == "" {
		return nil
	}
	if width <= 0 {
		return []string{m.kit.StatusText(clean, theme.Error)}
	}
	wrapped := ui.Wrap(ansi.Wrap(clean, width, ""), width)
	parts := strings.Split(wrapped, "\n")
	if maxLines > 0 && len(parts) > maxLines {
		parts = parts[:maxLines]
		parts[maxLines-1] = m.kit.Truncate(parts[maxLines-1]+m.kit.Hint("…"), width)
	}
	out := make([]string, 0, len(parts))
	for _, line := range parts {
		out = append(out, m.kit.StatusText(line, theme.Error))
	}
	return out
}

// viewHeader is v's title, key hints and state lines, width cells wide.
func (m *Model) viewHeader(v *appView, width, height int) []string {
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
	if v.focalBlock() != nil {
		// The inspection hint leads so a narrow header keeps it whole.
		state := "m map"
		if v.inspect {
			state = "m controls"
		}
		hints = state + " · " + hints
	}
	lines := []string{m.kit.Heading(viewText(v.data.Title)), m.kit.Muted(m.kit.Hint(hints))}
	if v.busy || v.data.State == sdk.ViewLoading {
		lines = append(lines, m.kit.Badge(m.kit.Hint("Loading…"), theme.Busy, max(0, width)))
	}
	maxErrLines := 6
	if height > 0 {
		maxErrLines = max(1, min(6, height-4))
	}
	if v.banner != "" {
		lines = append(lines, m.wrapErrorLines(v.banner, width, maxErrLines)...)
	}
	if v.data.Error != "" {
		lines = append(lines, m.wrapErrorLines(v.data.Error, width, maxErrLines)...)
	}
	return lines
}

// regionWindow reports v's scroll region window height and top line for a
// content box of width×height cells. The region delegates its clamp to
// bubbles/viewport (issue #185); regionPinned means "stay at the tail", so
// GotoBottom is the pin.
func (m *Model) regionWindow(v *appView, width, height int) (window, top int) {
	visible := max(1, height-len(m.viewHeader(v, width, height))-1)
	window = min(20, max(4, visible/2))
	if p := m.viewPanes(v, width, height); p.primary > 0 {
		// Beside a focal block the region lives in the independently
		// focus-windowed flow, so its own window must fit there.
		window = min(20, max(1, p.secondary/2))
	}
	vp := viewport.New(viewport.WithHeight(window))
	vp.SetContentLines(textLines(v.region().Text))
	if v.regionPinned {
		vp.GotoBottom()
	} else {
		vp.SetYOffset(v.regionTop)
	}
	return window, vp.YOffset()
}

// syncPresentationWindow stores the region window the focused window is
// about to show. The regionTop field stays canonical: scrollRegion moves it
// between renders by the window drawn last, and tests observe it directly.
func (m *Model) syncPresentationWindow() {
	v := m.focusedView()
	if !m.onWindows() || v.region() == nil || m.paletteOpen {
		return
	}
	if w, h := m.windowContentSize(v); w > 0 && h > 0 {
		v.regionWindow, v.regionTop = m.regionWindow(v, w, h)
	}
}

func (m *Model) viewLines(v *appView, width, height int) []string {
	lines, _ := m.viewLayout(v, width, height)
	return lines
}

// inspectLayout gives a hinted focal block the whole body: one hint row and
// a bounded window over the block. Focus, the selected grid cell, the
// selected item, and field drafts are untouched, so returning to controls
// resumes exactly where the user left off.
func (m *Model) inspectLayout(v *appView, width, height int) []string {
	hint := "Map · m controls · ↑↓ scroll"
	if width < 56 {
		hint = "Map · m controls"
	}
	lines := []string{m.kit.Muted(m.kit.Hint(hint))}
	return append(lines, m.primaryBlockRows(v, v.focalBlock(), width, max(0, height-1), true)...)
}

// surfaceLayout shows an action-revealed region from its first line as a
// reading view: the panel gets the body instead of competing with the map,
// meters, and focused control for the same few rows. The next ordinary
// navigation clears it and returns to the control window, while Enter still
// reaches the focused action so the same key can toggle a panel shut.
func (m *Model) surfaceLayout(v *appView, width, height int) []string {
	lines := m.viewHeader(v, width, height)
	contentRows := max(1, height-len(lines)-1)
	label := viewText(v.region().Label)
	if label != "" && contentRows > 1 {
		// One row is all the panel can offer: spend it on the content rather
		// than a label the toggle already names.
		lines = append(lines, m.kit.Muted(label))
		contentRows--
	}
	if contentRows > 0 {
		all := textLines(v.region().Text)
		top := min(max(0, v.regionTop), max(0, len(all)-contentRows))
		for _, line := range all[top:min(len(all), top+contentRows)] {
			lines = append(lines, m.kit.Text(line))
		}
	}
	lines = append(lines, m.kit.Muted(viewText(v.data.Status)))
	return lines
}

// viewLayout is v's lines in a content box of width×height cells, and where
// they show its actions and grid cells.
func (m *Model) viewLayout(v *appView, width, height int) ([]string, []viewTarget) {
	panes := m.viewPanes(v, width, height)
	if panes.inspect {
		return m.inspectLayout(v, width, height), nil
	}
	if v.surface != "" && v.region() != nil && v.region().ID == v.surface {
		return m.surfaceLayout(v, width, height), nil
	}
	lines := m.viewHeader(v, width, height)
	var rows []string
	// rowTargets holds the targets on each row, by its index in rows.
	rowTargets := map[int][]viewTarget{}
	focal := v.focalBlockIndex()
	for i, block := range v.data.Blocks {
		// The hinted block is drawn as the bounded primary viewport, or by
		// the inspection route on a narrow body. Unhinted blocks keep their
		// place in the ordinary flow.
		if i == focal {
			continue
		}
		if block.Label != "" {
			rows = append(rows, m.kit.Muted(viewText(block.Label)))
		}
		for _, line := range strings.Split(safe(block.Text), "\n") {
			rows = append(rows, m.kit.Text(line))
		}
	}
	if panes.primary == 0 {
		for _, meter := range v.data.Meters {
			rows = append(rows, m.meterRow(meter, width))
		}
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
	focusRow := formBase
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
			if i == v.focus {
				focusRow = len(rows) - 1
			}
			continue
		}
		value := viewText(v.values[field.ID])
		room := max(1, width-4-lipgloss.Width(label))
		text := label + ui.Tail(value, room)
		if i == v.focus {
			focusRow = len(rows)
			text = m.kit.FocusText(text + " ")
		} else {
			text = m.kit.Text(text)
		}
		rows = append(rows, text)
	}
	for i, action := range v.data.Actions {
		text := "[" + viewText(action.Label) + "]"
		if len(v.data.Fields)+i == v.focus {
			focusRow = len(rows)
		}
		if action.Disabled || v.busy || v.data.State == sdk.ViewLoading {
			text += " (disabled)"
			text = m.kit.StatusText(text, theme.Disabled)
		} else if len(v.data.Fields)+i == v.focus {
			text = m.kit.FocusText(text)
		} else {
			text = m.kit.Text(text)
		}
		rowTargets[len(rows)] = []viewTarget{{kind: viewTargetAction, index: i, id: action.ID, w: lipgloss.Width(text)}}
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
			var targets []viewTarget
			x := 0
			for col := 0; col < g.Columns; col++ {
				i := row*g.Columns + col
				if i >= len(g.Cells) {
					break
				}
				c := g.Cells[i]
				label := viewText(c.Label)
				text := "[" + label + "]"
				hovered := m.hoverAt(hoverGridCell, c.ID)
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
				case hovered:
					// The pointer's ASCII brackets are distinct from the
					// selected cell's marker and occupy the same two cells
					// as the normal brackets, so the grid never reflows.
					text = m.kit.Text("<" + label + ">")
				default:
					text = m.kit.Text(text)
				}
				cells = append(cells, text)
				w := lipgloss.Width(text)
				targets = append(targets, viewTarget{kind: viewTargetCell, index: i, id: c.ID, x: x, w: w})
				x += w + 1
			}
			rowTargets[len(rows)] = targets
			rows = append(rows, strings.Join(cells, " "))
		}
	}
	visible := max(1, panes.secondary)
	regionBase := -1
	if r := v.region(); r != nil {
		all := textLines(r.Text)
		window, top := m.regionWindow(v, width, height)
		label := viewText(r.Label)
		if len(all) > window {
			label += " " + m.kit.Muted(m.kit.Hint(fmt.Sprintf("– %d/%d", top+1, len(all))))
		}
		if v.regionFocused() {
			rows = append(rows, m.kit.FocusText(label))
		} else {
			rows = append(rows, m.kit.Muted(label))
		}
		regionBase = len(rows)
		for _, line := range all[top:min(len(all), top+window)] {
			rows = append(rows, m.kit.Text(line))
		}
	}
	focus := focusRow
	switch {
	case v.itemFocus:
		focus = itemBase + v.item
	case regionBase >= 0 && v.regionFocused():
		focus = len(rows) - 1
	case gridBase >= 0 && v.gridFocused():
		focus = gridBase + gridRow
	}
	start := max(0, min(focus-visible+1, max(0, len(rows)-visible))-v.scroll)
	// The primary viewport and pinned meters never scroll away with the
	// focused control; the secondary flow gets its own window around focus.
	primaryRows := m.primaryBlockRows(v, v.focalBlock(), width, panes.primary, false)
	lines = append(lines, primaryRows...)
	if panes.meters > 0 {
		for i := 0; i < min(panes.meters, len(v.data.Meters)); i++ {
			lines = append(lines, m.meterRow(v.data.Meters[i], width))
		}
	}
	var targets []viewTarget
	for r := start; r < min(len(rows), start+visible); r++ {
		for _, t := range rowTargets[r] {
			t.line = len(lines) + r - start
			targets = append(targets, t)
		}
	}
	lines = append(lines, rows[start:min(len(rows), start+visible)]...)
	lines = append(lines, m.kit.Muted(viewText(v.data.Status)))
	return lines, targets
}

func (m *Model) selectedView() tea.Cmd {
	if m.busy {
		return nil
	}
	if m.currentMode() == modeLauncher {
		filtered := m.filteredApps()
		if sel := min(m.selected, len(filtered)-1); sel >= 0 && filtered[sel].Running {
			return m.openWindow(filtered[sel])
		}
	}
	if m.currentMode() == modeHome {
		if hv := m.homeView(); hv.selected >= 0 && hv.items[hv.selected].kind == homeItemApp {
			return m.openWindow(hv.items[hv.selected].app)
		}
	}
	if len(m.apps) > 0 && m.selected < len(m.apps) && m.apps[m.selected].Running {
		return m.openWindow(m.apps[m.selected])
	}
	for _, a := range m.apps {
		if a.Running {
			return m.openWindow(a)
		}
	}
	m.append(entry{"Launch an app before opening its view.", "error"})
	return nil
}
