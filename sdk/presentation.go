package sdk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// PresentationVersion is the newest data/action protocol version, independent
// of an app's manifest version. Versions 2 and 3 are additive: every
// earlier-version collection keeps its meaning, while blocks, meters, and grid
// arrived at version 2 and multiline fields, the scroll region, and view
// invalidation arrive at version 3. Apps supply data, never terminal widgets
// or keymaps.
const PresentationVersion = 3

// MinPresentationVersion is the oldest protocol version still answered. A
// request may name any version in [MinPresentationVersion, PresentationVersion];
// the response is stamped with the requested version and must validate under
// it, so version-2 elements in a snapshot served to a version-1 request are an
// error rather than silently dropped content.
const MinPresentationVersion = 1

// Snapshot bounds shared by every version. New collections get their own
// deliberate budgets instead of reusing item byte limits.
const (
	maxViewItems   = 100
	maxViewFields  = 16
	maxViewActions = 16
	maxViewBlocks  = 4
	maxBlockLines  = 32
	maxBlockBytes  = 16384
	maxViewMeters  = 8
	maxGridColumns = 16
	maxGridCells   = 64
	// maxAreaValue is the multiline field value budget: a deliberately
	// larger allowance than the 4096-byte single-line field value, sized
	// for editing real documents without pretending to be unbounded.
	maxAreaValue   = 32768
	maxRegionLines = 2048
	maxRegionBytes = 65536
)

type ViewState string

const (
	ViewReady   ViewState = "ready"
	ViewLoading ViewState = "loading"
	ViewError   ViewState = "error"
)

type Item struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// Field is a text input. IDs, not labels or screen positions, route input
// back to the app. The experience owns editing and focus. Fields are
// single-line unless Multiline is set; a multiline field accepts newlines,
// carries the larger maxAreaValue budget instead of the single-line 4096,
// and exists only on version 3 and later. Live marks fields whose edits
// affect snapshot metadata (such as unsaved status or size); the experience
// sends live field drafts on view requests and refreshes when a live field
// changes. Live exists only on version 3 and later.
type Field struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	Required  bool   `json:"required"`
	Multiline bool   `json:"multiline,omitempty"`
	Live      bool   `json:"live,omitempty"`
}

type Action struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Block is a preformatted multi-line text region, such as ASCII art. Lines are
// sanitized and rendered verbatim in order: they clip at the viewport edge and
// never wrap, so apps can draw fixed-width figures. Label is an optional
// caption. Version 2 and later.
type Block struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Text  string `json:"text"`
	// Focus is an optional hint naming the block cell that matters most,
	// such as a map's player position. The experience may crop a hinted
	// block to a bounded viewport that keeps this cell visible; a block
	// without one renders exactly as before. Version 3 and later.
	Focus *BlockFocus `json:"focus,omitempty"`
}

// BlockFocus names one cell of a Block. X counts Unicode code points from
// the left edge of Focus.Y's line; Y counts lines from the block's top.
// Both are validated against the block's own text, so a focus never names a
// cell the block does not contain. The experience converts the column to
// display cells when it crops, keeping the named cell in view.
//
// Because the experience sanitizes block text before rendering, a hinted
// block must be valid UTF-8 and carry no terminal controls other than
// newlines: tabs, escape sequences, and other controls are expanded or
// dropped, which would move the named cell. Control-free text, including
// wide and combining runes, is rendered unchanged, so its coordinates stay
// exact. Blocks without a focus keep the ordinary sanitization.
type BlockFocus struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Meter is a labeled gauge rendered in the theme's progress vocabulary. Value
// is a finite fraction; the renderer clamps it to [0,1]. Version 2 and later.
type Meter struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// Cell is one selectable element of a Grid. Action names the request sent
// when the cell is activated; it does not have to appear in Actions. Both
// share one enabled/disabled namespace, so a same-named entry in Actions
// with Disabled set also disables the cell's declaration. An empty Action
// makes the cell a plain selection datum whose id rides along as cell_id on
// action requests. Disabled cells are drawn dimmed and activating one is a
// no-op, like a disabled Action. Version 2 and later.
type Cell struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Action   string `json:"action,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Grid is a rows x columns pad of selectable cells, filled row-major with a
// possibly short final row. Rows are implicit: ceil(len(Cells)/Columns).
// Version 2 and later.
type Grid struct {
	Label   string `json:"label"`
	Columns int    `json:"columns"`
	Cells   []Cell `json:"cells"`
}

// Region is a scrollable multi-line text viewport for long app output —
// transcripts, articles, feed bodies — that would overflow the item list or
// a fixed Block. Text is a full snapshot, not an append delta; the shell
// owns the scroll position, pins the view to the tail until the user scrolls
// up, and clips each line at the viewport edge without wrapping. Label is an
// optional caption. Unlike Block, a Region is a focus stop: it accepts
// scroll keys. Version 3 and later.
type Region struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Text  string `json:"text"`
}

// PathGrantRequest is a declarative request from an app's presentation view
// asking the experience to seek explicit operator approval for a single-path
// VFS grant and retry the named action on approval. Recursive directory grants
// are intentionally unrepresentable here: the experience always confirms with
// the operator and issues a non-recursive, session-bound grant through operator
// IPC only on approval. Version 3 and later.
type PathGrantRequest struct {
	Path   string `json:"path"`
	Access string `json:"access"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// View is a complete snapshot, not a patch or a tree of speculative widgets.
// Present supplies Version and Instance; Instance changes on every launch.
// Blocks, meters, and grid are additive version-2 elements; a version-1
// snapshot must not carry them. Multiline fields and the scroll region are
// additive version-3 elements; a snapshot stamped below version 3 must not
// carry them.
type View struct {
	Version      int               `json:"version"`
	Instance     string            `json:"instance"`
	Title        string            `json:"title"`
	State        ViewState         `json:"state"`
	Items        []Item            `json:"items"`
	Fields       []Field           `json:"fields"`
	Actions      []Action          `json:"actions"`
	Blocks       []Block           `json:"blocks,omitempty"`
	Meters       []Meter           `json:"meters,omitempty"`
	Grid         *Grid             `json:"grid,omitempty"`
	Region       *Region           `json:"region,omitempty"`
	GrantRequest *PathGrantRequest `json:"grant_request,omitempty"`
	Status       string            `json:"status"`
	Error        string            `json:"error"`
}

type ViewRequest struct {
	Version int               `json:"version"`
	Values  map[string]string `json:"values,omitempty"`
}

// ActionRequest carries semantic input only. RequestID identifies an in-flight
// operation for cancellation; it is not a durable idempotency key. ItemID is
// the selected item and CellID the selected grid cell, when those collections
// exist; an action declared on a cell is only enabled together with that
// cell's cell_id, and a same-named top-level action's Disabled flag disables
// the cell's declaration too.
type ActionRequest struct {
	Version   int               `json:"version"`
	Instance  string            `json:"instance"`
	RequestID string            `json:"request_id"`
	Action    string            `json:"action"`
	ItemID    string            `json:"item_id,omitempty"`
	CellID    string            `json:"cell_id,omitempty"`
	Values    map[string]string `json:"values,omitempty"`
}

type CancelRequest struct {
	Version   int    `json:"version"`
	Instance  string `json:"instance"`
	RequestID string `json:"request_id"`
}

type CancelResult struct {
	Canceled bool `json:"canceled"`
}

// Invalidate reports that the app's snapshot changed out of band — a timer
// fired, a background fetch landed — so open views should refetch now rather
// than on the poll cadence. It is a hint, not a data channel: nothing rides
// along and the shell still pulls the whole snapshot. Best-effort by design:
// hosts or shells that predate version 3 simply never act on it, so callers
// may ignore the error. Version 3 and later.
func (c *Context) Invalidate(ctx context.Context) error {
	return c.call(ctx, "app/invalidate", nil, nil)
}

type requestVersionKey struct{}

// PresentationRequestVersion reports the protocol version a view or action
// request was negotiated at, so one callback can serve version-appropriate
// content to older and newer clients. Outside a presentation callback it
// returns zero.
func PresentationRequestVersion(ctx context.Context) int {
	v, _ := ctx.Value(requestVersionKey{}).(int)
	return v
}

type requestValuesKey struct{}

// PresentationDraftValues reports live field drafts supplied on a version-3
// view request, keyed by field ID. View callbacks must treat the map as
// read-only and reflect drafts without mutating committed app state. Outside
// a view callback with drafts it returns nil.
func PresentationDraftValues(ctx context.Context) map[string]string {
	v, _ := ctx.Value(requestValuesKey{}).(map[string]string)
	return v
}

// Validate bounds snapshots and rejects ambiguous IDs and unsupported states.
// All text remains untrusted; the experience must strip terminal controls.
func (v View) Validate() error {
	if v.Version < MinPresentationVersion || v.Version > PresentationVersion || v.Instance == "" {
		return fmt.Errorf("presentation: unsupported version or missing instance")
	}
	if strings.TrimSpace(v.Title) == "" || len(v.Title) > 256 ||
		len(v.Status) > 4096 || len(v.Error) > 4096 {
		return fmt.Errorf("presentation: invalid title, status, or error")
	}
	switch v.State {
	case ViewReady, ViewLoading:
	case ViewError:
		if strings.TrimSpace(v.Error) == "" {
			return fmt.Errorf("presentation: error state requires an error banner")
		}
	default:
		return fmt.Errorf("presentation: unknown view state %q", v.State)
	}
	if len(v.Items) > maxViewItems || len(v.Fields) > maxViewFields || len(v.Actions) > maxViewActions ||
		len(v.Blocks) > maxViewBlocks || len(v.Meters) > maxViewMeters {
		return fmt.Errorf("presentation: snapshot exceeds element limits")
	}
	if v.Version < 2 && (len(v.Blocks) > 0 || len(v.Meters) > 0 || v.Grid != nil) {
		return fmt.Errorf("presentation: blocks, meters, and grid require version 2")
	}
	hasMultiline := false
	hasLive := false
	for _, f := range v.Fields {
		hasMultiline = hasMultiline || f.Multiline
		hasLive = hasLive || f.Live
	}
	hasFocus := false
	for _, block := range v.Blocks {
		hasFocus = hasFocus || block.Focus != nil
	}
	if v.Version < 3 && (v.Region != nil || hasMultiline || hasFocus || hasLive || v.GrantRequest != nil) {
		return fmt.Errorf("presentation: multiline fields, live fields, the scroll region, block focus, and grant requests require version 3")
	}
	check := func(id, label string, seen map[string]bool) error {
		if !entryPattern.MatchString(id) || len(id) > 64 || seen[id] ||
			strings.TrimSpace(label) == "" || len(label) > 256 {
			return fmt.Errorf("presentation: invalid or duplicate ID/label %q", id)
		}
		seen[id] = true
		return nil
	}
	seen := make(map[string]bool)
	for _, item := range v.Items {
		if err := check(item.ID, item.Label, seen); err != nil {
			return err
		}
		if len(item.Detail) > 4096 {
			return fmt.Errorf("presentation: item detail exceeds limit")
		}
	}
	seen = make(map[string]bool)
	for _, field := range v.Fields {
		if err := check(field.ID, field.Label, seen); err != nil {
			return err
		}
		if len(field.Value) > fieldValueLimit(field) {
			return fmt.Errorf("presentation: field %q value exceeds limit", field.ID)
		}
	}
	seen = make(map[string]bool)
	for _, action := range v.Actions {
		if err := check(action.ID, action.Label, seen); err != nil {
			return err
		}
	}
	seen = make(map[string]bool)
	for _, block := range v.Blocks {
		if !entryPattern.MatchString(block.ID) || len(block.ID) > 64 || seen[block.ID] || len(block.Label) > 256 {
			return fmt.Errorf("presentation: invalid or duplicate block %q", block.ID)
		}
		seen[block.ID] = true
		// Art gets its own cell budget, not item bytes: bounded lines and total
		// size. Lines are never wrapped; the renderer clips them honestly. One
		// trailing newline merely terminates the last line and is not counted
		// as an extra line.
		text := strings.TrimSuffix(block.Text, "\n")
		if len(block.Text) > maxBlockBytes || strings.Count(text, "\n")+1 > maxBlockLines {
			return fmt.Errorf("presentation: block %q exceeds line or size budget", block.ID)
		}
		if block.Focus != nil {
			if !focusTextStable(block.Text) {
				return fmt.Errorf("presentation: block %q focus requires control-free text", block.ID)
			}
			lines := strings.Split(text, "\n")
			if block.Focus.Y < 0 || block.Focus.Y >= len(lines) ||
				block.Focus.X < 0 || block.Focus.X >= utf8.RuneCountInString(lines[block.Focus.Y]) {
				return fmt.Errorf("presentation: block %q focus is outside its text", block.ID)
			}
		}
	}
	seen = make(map[string]bool)
	for _, meter := range v.Meters {
		if err := check(meter.ID, meter.Label, seen); err != nil {
			return err
		}
		if math.IsNaN(meter.Value) || math.IsInf(meter.Value, 0) {
			return fmt.Errorf("presentation: meter %q value is not finite", meter.ID)
		}
	}
	if g := v.Grid; g != nil {
		// An empty grid is a contract error, not a hidden element: omit Grid
		// entirely when a snapshot has no pad to show.
		if g.Columns < 1 || g.Columns > maxGridColumns || len(g.Cells) < 1 ||
			len(g.Cells) > maxGridCells || len(g.Label) > 256 {
			return fmt.Errorf("presentation: invalid grid dimensions or label")
		}
		seen = make(map[string]bool)
		for _, cell := range g.Cells {
			if err := check(cell.ID, cell.Label, seen); err != nil {
				return err
			}
			if cell.Action != "" && (!entryPattern.MatchString(cell.Action) || len(cell.Action) > 64) {
				return fmt.Errorf("presentation: invalid cell action %q", cell.Action)
			}
		}
	}
	if r := v.Region; r != nil {
		if !entryPattern.MatchString(r.ID) || len(r.ID) > 64 || len(r.Label) > 256 {
			return fmt.Errorf("presentation: invalid region %q", r.ID)
		}
		// One trailing newline merely terminates the last line, as in Block.
		text := strings.TrimSuffix(r.Text, "\n")
		if len(r.Text) > maxRegionBytes || strings.Count(text, "\n")+1 > maxRegionLines {
			return fmt.Errorf("presentation: region %q exceeds line or size budget", r.ID)
		}
	}
	if g := v.GrantRequest; g != nil {
		if !validGrantRequestPath(g.Path) {
			return fmt.Errorf("presentation: invalid grant request path %q", g.Path)
		}
		if g.Access != "read" && g.Access != "read-write" {
			return fmt.Errorf("presentation: invalid grant request access %q", g.Access)
		}
		if !entryPattern.MatchString(g.Action) || len(g.Action) > 64 || len(g.Reason) > 512 {
			return fmt.Errorf("presentation: invalid grant request action or reason")
		}
		found := false
		for _, a := range v.Actions {
			if a.ID == g.Action && !a.Disabled {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("presentation: grant request action %q is not an enabled action", g.Action)
		}
	}
	return nil
}

func validGrantRequestPath(p string) bool {
	if p == "" || len(p) > 1024 || !strings.HasPrefix(p, "/") || p == "/" || strings.HasSuffix(p, "/") {
		return false
	}
	if strings.ContainsRune(p, '\\') || strings.ContainsRune(p, 0) {
		return false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// focusTextStable reports whether the experience sanitizer leaves text
// coordinate-for-coordinate unchanged. Sanitization strips ANSI sequences,
// expands tabs, drops other terminal controls, and replaces invalid UTF-8;
// each of those moves the code-point columns a BlockFocus validated against
// the raw text. Newlines are structural and preserved, so a hinted block may
// contain them. Unhinted blocks keep the legacy sanitization.
func focusTextStable(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if r != '\n' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// fieldValueLimit is the per-field value budget: multiline fields get the
// larger document-editing allowance, single-line fields keep 4096.
func fieldValueLimit(f Field) int {
	if f.Multiline {
		return maxAreaValue
	}
	return 4096
}

// Present declares view, action, and cancel during Init. Callbacks obey the
// usual app-scoped context and concurrency rules. Returning an error produces
// an IPC error; returning ViewError keeps a recoverable error banner on screen.
// Actions must honor cancellation before committing work. Cancellation is
// cooperative and cannot undo an operation that has already committed.
//
// Requests may negotiate any version in [MinPresentationVersion,
// PresentationVersion]; snapshots are stamped and validated at the requested
// version. PresentationRequestVersion reports the negotiated version inside
// callbacks so one snapshot can serve older and newer clients.
func (c *Context) Present(snapshot func(context.Context) (View, error), action func(context.Context, ActionRequest) (View, error)) error {
	if snapshot == nil || action == nil {
		return fmt.Errorf("presentation: snapshot and action callbacks are required")
	}
	instance := rand.Text()
	var mu sync.Mutex
	active := make(map[string]context.CancelFunc)
	// The socket can dispatch cancel before action. Remember a bounded recent
	// window of cancellation intents so that race does not execute canceled work.
	var canceled []string
	finish := func(version int, v View, err error) (any, error) {
		if err != nil {
			return nil, err
		}
		v.Version, v.Instance = version, instance
		return v, v.Validate()
	}
	check := func(version int, owner, request string) error {
		if version < MinPresentationVersion || version > PresentationVersion || owner != instance {
			return fmt.Errorf("presentation: unsupported version or stale instance")
		}
		if !entryPattern.MatchString(request) || len(request) > 64 {
			return fmt.Errorf("presentation: invalid request_id")
		}
		return nil
	}
	if err := c.Handle("view", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ViewRequest
		if err := decodePresentation(raw, &p); err != nil {
			return nil, err
		}
		if p.Version < MinPresentationVersion || p.Version > PresentationVersion {
			return nil, &Error{
				Code:    CodeUnsupportedVersion,
				Message: fmt.Sprintf("presentation: unsupported version %d", p.Version),
			}
		}
		if err := validateViewValues(p.Version, p.Values); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, requestVersionKey{}, p.Version)
		if len(p.Values) > 0 {
			ctx = context.WithValue(ctx, requestValuesKey{}, p.Values)
		}
		v, err := snapshot(ctx)
		return finish(p.Version, v, err)
	}); err != nil {
		return err
	}
	if err := c.Handle("action", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ActionRequest
		if err := decodePresentation(raw, &p); err != nil {
			return nil, err
		}
		if err := check(p.Version, p.Instance, p.RequestID); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		ctx = context.WithValue(ctx, requestVersionKey{}, p.Version)
		mu.Lock()
		for _, id := range canceled {
			if id == p.RequestID {
				mu.Unlock()
				return nil, context.Canceled
			}
		}
		if _, exists := active[p.RequestID]; exists || len(active) >= 16 {
			mu.Unlock()
			return nil, fmt.Errorf("presentation: duplicate or too many active requests")
		}
		active[p.RequestID] = cancel
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(active, p.RequestID)
			mu.Unlock()
		}()
		v, err := snapshot(ctx)
		if err != nil {
			return nil, err
		}
		v.Version, v.Instance = p.Version, instance
		if err := v.Validate(); err != nil {
			return nil, err
		}
		if err := validateAction(v, p); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v2, err := action(ctx, p)
		return finish(p.Version, v2, err)
	}); err != nil {
		return err
	}
	return c.Handle("cancel", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p CancelRequest
		if err := decodePresentation(raw, &p); err != nil {
			return nil, err
		}
		if err := check(p.Version, p.Instance, p.RequestID); err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if cancel := active[p.RequestID]; cancel != nil {
			cancel()
		}
		canceled = append(canceled, p.RequestID)
		if len(canceled) > 64 {
			canceled = canceled[len(canceled)-64:]
		}
		return CancelResult{Canceled: true}, nil
	})
}

func validateViewValues(version int, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	if version < 3 {
		return fmt.Errorf("presentation: view draft values require version 3")
	}
	if len(values) > maxViewFields {
		return fmt.Errorf("presentation: view draft values exceed field limit")
	}
	for id, value := range values {
		if !entryPattern.MatchString(id) || len(id) > 64 {
			return fmt.Errorf("presentation: invalid draft field %q", id)
		}
		if len(value) > maxAreaValue {
			return fmt.Errorf("presentation: draft field %q value exceeds limit", id)
		}
	}
	return nil
}

func validateAction(v View, p ActionRequest) error {
	// Cell-declared and top-level actions share one namespace: an action id
	// Disabled in Actions is disabled everywhere, including on a cell that
	// declares it.
	enabled := false
	topDisabled := false
	for _, a := range v.Actions {
		if a.ID == p.Action {
			enabled = !a.Disabled
			topDisabled = a.Disabled
		}
	}
	if p.CellID != "" {
		var cell *Cell
		if v.Grid != nil {
			for i := range v.Grid.Cells {
				if v.Grid.Cells[i].ID == p.CellID {
					cell = &v.Grid.Cells[i]
				}
			}
		}
		if cell == nil || cell.Disabled {
			return fmt.Errorf("presentation: unknown or disabled cell %q", p.CellID)
		}
		// A cell-declared action is enabled only alongside its own enabled
		// cell, and only while a same-named top-level action is not disabled.
		if cell.Action != "" && cell.Action == p.Action && !topDisabled {
			enabled = true
		}
	}
	if !enabled || v.State == ViewLoading {
		return fmt.Errorf("presentation: unknown, disabled, or loading action %q", p.Action)
	}
	if p.ItemID != "" {
		found := false
		for _, item := range v.Items {
			found = found || item.ID == p.ItemID
		}
		if !found {
			return fmt.Errorf("presentation: unknown item %q", p.ItemID)
		}
	}
	limits := make(map[string]int)
	for _, f := range v.Fields {
		limits[f.ID] = fieldValueLimit(f)
		if f.Required && strings.TrimSpace(p.Values[f.ID]) == "" {
			return fmt.Errorf("presentation: field %q is required", f.ID)
		}
	}
	for id, value := range p.Values {
		limit, known := limits[id]
		if !known || len(value) > limit {
			return fmt.Errorf("presentation: unknown or oversized field %q", id)
		}
	}
	return nil
}

func decodePresentation(raw json.RawMessage, out any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("presentation: params object is required")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("presentation: bad params: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("presentation: expected one params object")
	}
	return nil
}
