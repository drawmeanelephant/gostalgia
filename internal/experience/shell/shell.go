// Package shell is Gostalgia's Charm experience layer. Runtime and SDK never
// import it. All environment operations use Caller, not privileged internals.
package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/ipc"
	"gostalgia/internal/themedef"
)

type entry struct {
	text string
	kind string
}
type closedMsg struct{}

type viewMode int

const (
	modeHome viewMode = iota
	modeLauncher
	modePrompt
)

type FocusState int

const (
	FocusPrompt FocusState = iota
	FocusHome
	FocusLauncher
	FocusApp
	FocusPalette
	FocusTasks
	FocusNotifications
	FocusDialog
)

func (f FocusState) String() string {
	switch f {
	case FocusHome:
		return "home"
	case FocusLauncher:
		return "launcher"
	case FocusApp:
		return "app"
	case FocusPalette:
		return "palette"
	case FocusTasks:
		return "tasks"
	case FocusNotifications:
		return "notifications"
	case FocusDialog:
		return "dialog"
	default:
		return "prompt"
	}
}

type procTracking struct {
	state        string
	exitCode     int
	restartCount int
	crashLoop    bool
}

type tickMsg time.Time
type procUpdateMsg struct {
	procs []taskmanager.ProcessRow
	err   error
	// seq numbers the poll when it was sent.
	seq int
}
type procActionMsg struct {
	status string
	err    error
}
type procLogsMsg struct {
	pid     int32
	name    string
	content string
	err     error
	// seq numbers the request; open marks one that opens the log tail
	// rather than refreshing it.
	seq  uint64
	open bool
}
type receiptLogsMsg struct {
	pid       int32
	receiptID string
	logs      string
}

type openAppMsg struct {
	app appStatus
}

type configUpdateMsg struct {
	theme            string
	colorMode        string
	reducedMotion    *bool
	motionConfigured bool
	startupView      string
	dnd              *bool
	shortcuts        map[string]string
	// loaded is set for a refresh that read the configuration. mouse is
	// accessibility.mouse in it, nil when it is unset.
	loaded bool
	mouse  *bool
}

// Model owns UI state; tea.Cmd does IO off the event-loop goroutine.
type Model struct {
	ctx           context.Context
	client        Caller
	closed        <-chan struct{}
	width, height int
	cwd           string
	input         []rune
	cursor        int
	// transcript is the semantic history behind transcriptBuf: one entry
	// per logical line, oldest first, kept to restyle the lines when the
	// kit changes. See transcript.go.
	transcript         []entry
	transcriptBuf      *bloom.Buffer
	transcriptLook     transcriptLook
	transcriptPos      bloom.ViewportPos
	history            []string
	historyPos         int
	draft              string
	apps               []appStatus
	selected           int
	launcherTop        int // the first card a click left in view; see launcherCards
	shelf              bool
	busy               bool
	scroll             int
	kit                ui.Kit
	ui                 *bloom.Root // this model's own root; see root()
	uiKey              rootKey
	motion             motionState // this model's own clocks and finite effects
	themeMotionDefault bool        // active user or built-in theme's own ReducedMotion token
	configuredMotion   *bool       // last explicit non-default config value; nil uses the theme default
	motionOverride     *bool       // session-local motion command; outranks later config refreshes
	presentation       *appView
	viewEpoch          uint64
	viewSub            *ipc.ClientSubscription
	mode               viewMode
	homeData           homeData
	homeSelected       int
	launcherQuery      string
	paletteOpen        bool
	paletteQuery       string
	paletteSelected    int
	previousMode       viewMode
	// confirm is the open confirmation dialog, if any; see confirm.go.
	confirm *confirmation
	// procPending is set while a confirmed process stop or reap is in
	// flight, so another cannot be confirmed until its result arrives.
	procPending bool
	// procSent numbers process polls as they are sent and procShown is
	// the newest one the table shows. Polls overlap, so a slow older
	// result arriving after a newer one is dropped, and a confirmation
	// can tell a poll sent after it opened from an older one.
	procSent, procShown int
	// logsSent numbers proc/logs requests as they are sent. logsOpening is
	// the open request whose answer may open the log tail, superseding any
	// before it, and logsShown the newest answer applied for each PID, so a
	// slow older one cannot take the buffer back. One refresh at a time is
	// in flight while logsRefreshing.
	logsSent, logsOpening uint64
	logsShown             map[int32]uint64
	logsRefreshing        bool

	taskView  bool
	notifView bool
	tasks     *taskmanager.Model
	notifs    *notifications.Manager
	receipts  *receipts.Store
	lastProcs map[int32]procTracking

	shortcuts           map[string]string
	initialConfigLoaded bool
	pendingTheme        string // user theme name with an in-flight theme/get; only the latest resolves
	unresolvedTheme     string // config theme name that failed user-theme resolution; not retried

	attached     bool
	sessionID    string
	attachmentID string
	user         string

	attachClientType string
	attachClientID   string
	attachSessionID  string

	// mouseOff is set while this shell takes no mouse input; see mouse.go.
	// mouseConfig is the accessibility.mouse setting last read, nil while
	// it is unset.
	mouseOff    bool
	mouseConfig *bool

	// keyEnh records the keyboard enhancement flags the terminal actually
	// negotiated (KeyboardEnhancementsMsg). It is observational: every
	// enhanced chord arrives as a distinct keystroke or message type that
	// only a negotiating terminal can produce, so no handler needs to
	// gate on it — a terminal that never answers keeps today's keyset.
	keyEnh tea.KeyboardEnhancementsMsg
}

func (m *Model) SetUser(user string) {
	m.user = user
}

func (m *Model) User() string {
	if m.user == "" {
		return "guest"
	}
	return m.user
}

func (m *Model) SetAttached(attached bool) {
	m.attached = attached
}

// SetAttachIdentity overrides the parameters sent with session/attach when the
// shell runs attached. Transports other than a local terminal (e.g. the SSH
// attach listener) use it to name their client type and to pin the session the
// connection was authorized for. Empty sessionID keeps the runtime's default
// session resolution.
func (m *Model) SetAttachIdentity(clientType, clientID, sessionID string) {
	m.attachClientType = clientType
	m.attachClientID = clientID
	m.attachSessionID = sessionID
}

func (m *Model) Attached() bool {
	return m.attached
}

// SessionIdentity reports the session attachment the shell registered through
// session/attach, so a transport can release it explicitly when the connection
// ends without a clean quit — an SSH disconnect must not leave a stale
// attachment on the session. Empty strings when no attach completed.
func (m *Model) SessionIdentity() (sessionID, attachmentID string) {
	return m.sessionID, m.attachmentID
}

func (m *Model) currentMode() viewMode {
	if m.shelf {
		return modeLauncher
	}
	return m.mode
}

func (m *Model) setMode(mode viewMode) {
	m.mode = mode
	m.shelf = (mode == modeLauncher)
}

func (m *Model) Focus() FocusState {
	if m.confirm != nil {
		return FocusDialog
	}
	if m.paletteOpen {
		return FocusPalette
	}
	if m.presentation != nil {
		return FocusApp
	}
	if m.taskView {
		return FocusTasks
	}
	if m.notifView {
		return FocusNotifications
	}
	switch m.currentMode() {
	case modeHome:
		return FocusHome
	case modeLauncher:
		return FocusLauncher
	default:
		return FocusPrompt
	}
}

func (m *Model) initExperience() {
	if m.tasks == nil {
		m.tasks = taskmanager.New()
	}
	if m.notifs == nil {
		m.notifs = notifications.NewManager(100, 3, 8)
	}
	if m.receipts == nil {
		m.receipts = receipts.NewStore(50)
	}
	if m.lastProcs == nil {
		m.lastProcs = make(map[int32]procTracking)
	}
}

func New(ctx context.Context, c Caller, closed <-chan struct{}) *Model {
	return NewWithTheme(ctx, c, closed, theme.Nostalgia(), ui.ANSI256)
}

// NewWithTheme makes appearance and color capability explicit. Plain rendering
// is useful for snapshots and terminals without ANSI styling.
func NewWithTheme(ctx context.Context, c Caller, closed <-chan struct{}, t theme.Theme, mode ui.ColorMode) *Model {
	return NewWithThemeAndNotifs(ctx, c, closed, t, mode, notifications.NewManager(100, 3, 8))
}

// NewWithNotifs creates a shell that shares the supplied notification manager.
func NewWithNotifs(ctx context.Context, c Caller, closed <-chan struct{}, notifs *notifications.Manager) *Model {
	return NewWithThemeAndNotifs(ctx, c, closed, theme.Nostalgia(), ui.ANSI256, notifs)
}

// NewWithThemeAndNotifs makes appearance and color capability explicit and
// shares the supplied notification manager with services.
func NewWithThemeAndNotifs(ctx context.Context, c Caller, closed <-chan struct{}, t theme.Theme, mode ui.ColorMode, notifs *notifications.Manager) *Model {
	m := &Model{
		ctx: ctx, client: c, closed: closed, width: 80, height: 24, cwd: "/users/guest",
		mode:          modeHome,
		transcriptPos: bloom.ViewportEnd,
		tasks:         taskmanager.New(),
		notifs:        notifs,
		receipts:      receipts.NewStore(50),
		lastProcs:     make(map[int32]procTracking),
		user:          "guest",
	}
	m.motion.now = time.Now
	m.SetTheme(t, mode)
	m.append(entry{"Welcome home. A familiar prompt. A whole new environment.", "accent"})
	m.append(entry{"Type help to explore, or F2 to open your app shelf.", "muted"})
	return m
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *Model) pollProcessesCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	m.procSent++
	seq := m.procSent
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		rows, err := taskmanager.FetchProcesses(c, client)
		return procUpdateMsg{procs: rows, err: err, seq: seq}
	}
}

// fetchLogsCmd asks proc/logs for the tail of pid's log, to open its log
// tail with when open is set, and otherwise to refresh it.
func (m *Model) fetchLogsCmd(pid int32, name string, open bool) tea.Cmd {
	client := m.client
	ctx := m.ctx
	m.logsSent++
	seq := m.logsSent
	if open {
		m.logsOpening = seq
	} else {
		m.logsRefreshing = true
	}
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		logs, err := taskmanager.FetchLogs(c, client, pid, taskmanager.LogTailLines)
		return procLogsMsg{pid: pid, name: name, content: logs, err: err, seq: seq, open: open}
	}
}

// applyLogs takes a proc/logs answer. The latest open request opens its
// log tail while the table it was asked from is still showing; any other
// answer only refreshes a log tail already kept for its PID, open or not,
// so a late one never shows another process's lines.
func (m *Model) applyLogs(msg procLogsMsg) {
	if !msg.open {
		m.logsRefreshing = false
	}
	opening := msg.open && msg.seq == m.logsOpening
	if opening {
		m.logsOpening = 0
		opening = m.taskView && m.tasks.Mode() == taskmanager.ModeTable
	}
	if msg.err != nil {
		if opening {
			m.tasks.SetError(msg.err.Error())
		}
		return
	}
	if msg.seq <= m.logsShown[msg.pid] {
		return
	}
	if m.logsShown == nil {
		m.logsShown = make(map[int32]uint64)
	}
	if opening {
		m.logsShown[msg.pid] = msg.seq
		m.tasks.ShowLogs(msg.pid, msg.name, msg.content)
	} else if m.tasks.UpdateLogs(msg.pid, msg.content) {
		m.logsShown[msg.pid] = msg.seq
	}
}

func (m *Model) fetchLogsForReceiptCmd(pid int32, receiptID string) tea.Cmd {
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		logs, err := taskmanager.FetchLogs(c, client, pid, 30)
		if err != nil {
			logs = fmt.Sprintf("(failed to retrieve child logs: %v)", err)
		}
		return receiptLogsMsg{pid: pid, receiptID: receiptID, logs: logs}
	}
}

func (m *Model) stopProcessCmd(pid int32) tea.Cmd {
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := taskmanager.StopProcess(c, client, pid)
		status := fmt.Sprintf("Stopped process %d", pid)
		return procActionMsg{status: status, err: err}
	}
}

func (m *Model) reapProcessesCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := taskmanager.ReapProcesses(c, client)
		status := fmt.Sprintf("Reaped %d dead processes", n)
		return procActionMsg{status: status, err: err}
	}
}

func (m *Model) fetchConfigCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var out struct {
			Effective map[string]any `json:"effective"`
			Layers    map[string]any `json:"layers"`
		}
		if err := client.Call(c, "config/list", nil, &out); err != nil {
			return nil
		}
		msg := m.parseConfigMsg(out.Effective)
		msg.motionConfigured = hasConfiguredMotion(out.Layers)
		return msg
	}
}

type themeSavedMsg struct{ err error }

// themeResolveMsg carries a user theme resolved through the theme service.
// A nil theme means the name did not resolve.
type themeResolveMsg struct {
	name  string
	theme *theme.Theme
}

// resolveThemeCmd fetches a user theme definition via theme/get for a
// theme name the config layer selected that is not a built-in.
func (m *Model) resolveThemeCmd(name string) tea.Cmd {
	client, ctx := m.client, m.ctx
	if client == nil {
		return nil
	}
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var out struct {
			Definition themedef.Definition `json:"definition"`
		}
		if err := client.Call(c, "theme/get", map[string]string{"name": name}, &out); err != nil || out.Definition.Name == "" {
			return themeResolveMsg{name: name}
		}
		t := theme.FromDefinition(out.Definition)
		return themeResolveMsg{name: name, theme: &t}
	}
}

// persistThemeCmd writes the chosen theme to the user config layer so the
// prompt's `theme` command survives reboot like a Settings-app change does.
// The live switch has already happened, so a failed save means session-only.
func (m *Model) persistThemeCmd(name string) tea.Cmd {
	client, ctx := m.client, m.ctx
	if client == nil {
		return nil
	}
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return themeSavedMsg{err: client.Call(c, "config/set", map[string]any{
			"path":  "theme",
			"value": name,
		}, nil)}
	}
}

func (m *Model) parseConfigMsg(eff map[string]any) configUpdateMsg {
	msg := configUpdateMsg{
		shortcuts: make(map[string]string),
	}
	if eff == nil {
		return msg
	}
	msg.loaded = true
	if t, ok := eff["theme"].(string); ok {
		msg.theme = t
	}
	if acc, ok := eff["accessibility"].(map[string]any); ok {
		if cm, ok := acc["color_mode"].(string); ok {
			msg.colorMode = cm
		}
		if rm, ok := acc["reduced_motion"].(bool); ok {
			msg.reducedMotion = &rm
		}
		if mo, ok := acc[mouseConfigKey].(bool); ok {
			msg.mouse = &mo
		}
	}
	if st, ok := eff["startup"].(map[string]any); ok {
		if sv, ok := st["view"].(string); ok {
			msg.startupView = sv
		}
	}
	if notif, ok := eff["notifications"].(map[string]any); ok {
		if d, ok := notif["dnd"].(bool); ok {
			msg.dnd = &d
		}
	}
	if sc, ok := eff["shortcuts"].(map[string]any); ok {
		for k, v := range sc {
			if str, ok := v.(string); ok {
				msg.shortcuts[k] = str
			}
		}
	}
	return msg
}

// hasConfiguredMotion distinguishes an operator's explicit preference from
// the built-in accessibility.reduced_motion=false default. Theme defaults
// remain authoritative when no higher layer sets the preference.
func hasConfiguredMotion(layers map[string]any) bool {
	for _, name := range []string{"preview", "app", "user", "system"} {
		layer, ok := layers[name].(map[string]any)
		if !ok {
			continue
		}
		accessibility, ok := layer["accessibility"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := accessibility["reduced_motion"].(bool); ok {
			return true
		}
	}
	return false
}

// openSelectedTask opens the process selected in the Task Manager's table,
// as Enter does: its crash receipt if it has one, or else its log tail.
func (m *Model) openSelectedTask() tea.Cmd {
	sel := m.tasks.SelectedProcess()
	if sel == nil {
		return nil
	}
	if r, ok := m.receipts.GetByPID(sel.ID); ok {
		m.tasks.ShowReceipt(&r)
		return nil
	}
	return m.fetchLogsCmd(sel.ID, sel.Name, true)
}

// openSelectedNotification marks the selected notification seen and opens
// its crash receipt, if it has one, as Enter does.
func (m *Model) openSelectedNotification() tea.Cmd {
	if sel := m.notifs.Selected(); sel != nil {
		m.notifs.MarkSeen(sel.ID)
		if sel.Receipt != nil {
			m.taskView = true
			m.notifView = false
			m.tasks.ShowReceipt(sel.Receipt)
		}
	}
	return nil
}

func (m *Model) openAppByID(id string) tea.Cmd {
	m.closePalette()
	m.taskView = false
	m.notifView = false
	m.shelf = false
	for _, a := range m.apps {
		if a.Manifest.ID == id {
			if a.Running {
				return m.openView(a)
			}
			ctx := m.ctx
			client := m.client
			return func() tea.Msg {
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				var out json.RawMessage
				if err := client.Call(c, "app/launch", map[string]string{"id": id}, &out); err != nil {
					return resultMsg{err: err, failedLaunch: true}
				}
				var apps []appStatus
				_ = client.Call(c, "app/list", nil, &apps)
				for _, launched := range apps {
					if launched.Manifest.ID == id {
						return openAppMsg{app: launched}
					}
				}
				return resultMsg{text: "Launched " + id}
			}
		}
	}
	return func() tea.Msg {
		return resultMsg{err: fmt.Errorf("app %s not found", id), failedLaunch: true}
	}
}

// isShortcut stays on strings.EqualFold rather than bubbles/key (issue #185):
// key.Matches compares msg.String() exactly, while user-configured shortcuts
// in the config layer need case-insensitive matching ("Ctrl-P" == "ctrl+p").
func (m *Model) isShortcut(action, key string) bool {
	target, ok := m.shortcuts[action]
	if !ok || target == "" {
		switch action {
		case "home":
			target = "f1"
		case "launcher":
			target = "f2"
		case "stop":
			target = "f3"
		case "view":
			target = "f4"
		case "tasks":
			target = "f5"
		case "notifications":
			target = "f6"
		case "settings":
			target = "f7"
		case "palette":
			target = "ctrl+p"
		}
	}
	return strings.EqualFold(key, target)
}

func (m *Model) SetColorModeByName(name string) bool {
	var mode ui.ColorMode
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "plain", "none", "mono":
		mode = ui.Plain
	case "ansi256", "256":
		mode = ui.ANSI256
	case "truecolor", "24bit", "rgb":
		mode = ui.TrueColor
	case "ansi16", "16", "ansi":
		mode = ui.ANSI16
	default:
		return false
	}
	m.setKit(ui.New(m.kit.Theme(), mode))
	return true
}

func (m *Model) handleProcessUpdates(procs []taskmanager.ProcessRow) tea.Cmd {
	m.tasks.SetProcesses(procs)
	var cmds []tea.Cmd

	for _, p := range procs {
		prev, existed := m.lastProcs[p.ID]
		m.lastProcs[p.ID] = procTracking{
			state:        p.State,
			exitCode:     p.ExitCode,
			restartCount: p.RestartCount,
			crashLoop:    p.CrashLoop,
		}

		if !existed {
			continue
		}

		crashed := (p.State == "failed" || (p.State == "stopped" && p.ExitCode != 0) || p.CrashLoop)
		wasCrashed := (prev.state == "failed" || (prev.state == "stopped" && prev.exitCode != 0) || prev.crashLoop)

		if crashed && (!wasCrashed || p.RestartCount > prev.restartCount) {
			reason := p.Error
			if reason == "" {
				reason = fmt.Sprintf("Process exited with code %d", p.ExitCode)
			}
			receipt := receipts.New(p.ID, p.Name, p.State, p.ExitCode, reason, p.RestartCount, "")
			m.receipts.Add(receipt)

			m.notifs.Record(notifications.Notification{
				Title:     fmt.Sprintf("%s Crashed", p.Name),
				Message:   fmt.Sprintf("PID %d exit %d (%s)", p.ID, p.ExitCode, p.State),
				Level:     notifications.LevelError,
				Source:    "process",
				PID:       p.ID,
				Receipt:   &receipt,
				Timestamp: time.Now(),
			})
			cmds = append(cmds, m.fetchLogsForReceiptCmd(p.ID, receipt.ID))
		} else if p.State == "restarting" && prev.state != "restarting" {
			m.notifs.Record(notifications.Notification{
				Title:     fmt.Sprintf("%s Backoff", p.Name),
				Message:   fmt.Sprintf("PID %d backoff restart %d", p.ID, p.RestartCount),
				Level:     notifications.LevelWarning,
				Source:    "process",
				PID:       p.ID,
				Timestamp: time.Now(),
			})
		} else if p.State == "stopped" && p.ExitCode == 0 && prev.state != "stopped" && prev.state != "" {
			m.notifs.Record(notifications.Notification{
				Title:     fmt.Sprintf("%s Finished", p.Name),
				Message:   fmt.Sprintf("PID %d completed cleanly", p.ID),
				Level:     notifications.LevelInfo,
				Source:    "process",
				PID:       p.ID,
				Timestamp: time.Now(),
			})
		}
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}
	return nil
}

func (m *Model) Init() tea.Cmd {
	m.initExperience()
	m.busy = true // Initial discovery must finish before accepting a command.
	cwd := m.cwd
	refresh := func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		var apps []appStatus
		err := m.client.Call(ctx, "app/list", nil, &apps)
		msg := resultMsg{apps: apps, cwd: cwd, err: err}

		var status struct {
			UptimeSeconds float64 `json:"uptime_seconds"`
			User          string  `json:"user"`
			Processes     []any   `json:"processes"`
			Services      []any   `json:"services"`
		}
		if sErr := m.client.Call(ctx, "sys/status", nil, &status); sErr == nil {
			msg.status = sysStatusData{
				UptimeSeconds: status.UptimeSeconds,
				User:          status.User,
				ProcessCount:  len(status.Processes),
				ServicesCount: len(status.Services),
			}
			msg.hasStatus = true
		}

		userForDocs := "guest"
		if msg.hasStatus && msg.status.User != "" {
			userForDocs = msg.status.User
		}

		var dir struct {
			Entries []struct {
				Name  string `json:"name"`
				IsDir bool   `json:"is_dir"`
				Size  int64  `json:"size"`
			} `json:"entries"`
		}
		docsPath := fmt.Sprintf("/users/%s/documents", userForDocs)
		if fErr := m.client.Call(ctx, "fs/list", map[string]string{"path": docsPath}, &dir); fErr == nil {
			var docs []docShortcut
			for _, e := range dir.Entries {
				if !e.IsDir {
					docs = append(docs, docShortcut{
						Name: e.Name,
						Path: docsPath + "/" + e.Name,
						Size: e.Size,
					})
				}
			}
			msg.documents = docs
			msg.hasDocs = true
		}

		if m.attached {
			var attResp struct {
				SessionID    string `json:"session_id"`
				AttachmentID string `json:"attachment_id"`
				ActiveCount  int    `json:"active_count"`
				Workspace    struct {
					CurrentDir string   `json:"cwd"`
					ActiveView string   `json:"active_view"`
					History    []string `json:"history"`
				} `json:"workspace"`
			}
			attachParams := map[string]any{"client_type": "shell"}
			if m.attachClientType != "" {
				attachParams["client_type"] = m.attachClientType
			}
			if m.attachClientID != "" {
				attachParams["client_id"] = m.attachClientID
			}
			if m.attachSessionID != "" {
				attachParams["session_id"] = m.attachSessionID
			}
			if aErr := m.client.Call(ctx, "session/attach", attachParams, &attResp); aErr == nil {
				msg.sessionID = attResp.SessionID
				msg.attachmentID = attResp.AttachmentID
				msg.activeCount = attResp.ActiveCount
				if attResp.Workspace.CurrentDir != "" {
					msg.cwd = attResp.Workspace.CurrentDir
				}
				msg.workspaceHistory = attResp.Workspace.History
				msg.initialView = attResp.Workspace.ActiveView
			}
		} else {
			var ws struct {
				CurrentDir string   `json:"cwd"`
				ActiveView string   `json:"active_view"`
				History    []string `json:"history"`
			}
			if wErr := m.client.Call(ctx, "session/workspace/get", nil, &ws); wErr == nil {
				if ws.CurrentDir != "" {
					msg.cwd = ws.CurrentDir
				}
				msg.workspaceHistory = ws.History
				msg.initialView = ws.ActiveView
			}
		}

		return msg
	}
	watch := func() tea.Msg {
		select {
		case <-m.closed:
		case <-m.ctx.Done():
		}
		return closedMsg{}
	}
	return tea.Batch(refresh, watch, tickCmd(), m.pollProcessesCmd(), m.fetchConfigCmd())
}

func (m *Model) submit(line string) tea.Cmd {
	m.busy = true
	m.resetScroll()
	m.append(entry{dosPath(m.cwd) + "> " + line, "command"})
	m.history = append(m.history, line)
	if len(m.history) > 100 {
		m.history = m.history[len(m.history)-100:]
	}
	m.historyPos = len(m.history)
	m.input, m.cursor, m.draft = nil, 0, ""
	cwd := m.cwd
	hint := m.kit.Hint
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		return execute(ctx, m.client, cwd, line, hint)
	}
}

// Update passes msg to the bloom root, except the terminal's own colors and
// profile, then to the shell and this model's finite effects. Motion is
// updated after the shell so turning ReducedMotion on cannot schedule another
// frame from an effect that was active when the message arrived.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.MouseMsg); ok && m.mouseOff {
		// A report sent before the terminal saw the mouse turned off.
		return m, nil
	}
	var rootCmd tea.Cmd
	if forwardToRoot(msg) {
		rootCmd = m.root().Update(msg)
	}
	model, cmd := m.update(msg)
	return model, tea.Batch(rootCmd, cmd, m.updateMotion(msg))
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.initExperience()
		m.notifs.Tick()
		var cmds []tea.Cmd
		cmds = append(cmds, tickCmd())
		if !m.busy {
			cmds = append(cmds, m.pollProcessesCmd())
			if pid, open := m.tasks.LogsPID(); open && m.taskView && !m.logsRefreshing {
				cmds = append(cmds, m.fetchLogsCmd(pid, "", false))
			}
			if time.Time(msg).Second()%5 == 0 {
				cmds = append(cmds, m.fetchConfigCmd())
			}
		}
		return m, tea.Batch(cmds...)
	case openAppMsg:
		return m, m.openView(msg.app)
	case bloom.SelectionMsg:
		// Text selected with the mouse is copied as the selection ends.
		// The release or delayed double-click completion may belong to a
		// frame drawn before a modal opened.
		if !m.baseMouseAllowed() {
			return m, nil
		}
		return m, m.copyCmd(msg.Text)
	case copyMsg:
		m.initExperience()
		if msg.err == nil {
			// Routine success needs no history entry; a failure stays
			// there for whoever was away, or under Do-Not-Disturb.
			m.flash(copyNotice(msg))
		} else {
			m.notifs.Record(copyNotice(msg))
		}
		return m, nil
	case configUpdateMsg:
		var themeCmd tea.Cmd
		if msg.theme != "" {
			if m.SetThemeByName(msg.theme) {
				m.pendingTheme = ""
			} else if lower := strings.ToLower(msg.theme); !strings.EqualFold(m.kit.Theme().Name, msg.theme) &&
				m.unresolvedTheme != lower && m.pendingTheme != lower {
				// Not a built-in: it may be a user theme from /config/themes,
				// resolved asynchronously via theme/get. A name that failed
				// once is not retried on every poll.
				m.pendingTheme = lower
				themeCmd = m.resolveThemeCmd(lower)
			}
		}
		if msg.colorMode != "" {
			m.SetColorModeByName(msg.colorMode)
		}
		if msg.loaded {
			m.configuredMotion = nil
			if msg.motionConfigured {
				m.configuredMotion = msg.reducedMotion
			}
		} else if msg.reducedMotion != nil {
			// Direct configUpdateMsg values in integration tests and internal
			// callers represent explicit changes, not an effective-config poll.
			m.configuredMotion = msg.reducedMotion
		}
		if msg.loaded || msg.reducedMotion != nil || msg.theme != "" {
			m.applyMotionPolicy()
		}
		if msg.dnd != nil && m.notifs != nil {
			m.notifs.SetDND(*msg.dnd)
		}
		if msg.loaded {
			m.applyMouseConfig(msg.mouse)
		}
		if len(msg.shortcuts) > 0 {
			if m.shortcuts == nil {
				m.shortcuts = make(map[string]string)
			}
			for k, v := range msg.shortcuts {
				m.shortcuts[k] = v
			}
		}
		if !m.initialConfigLoaded {
			m.initialConfigLoaded = true
			if msg.startupView == "launcher" || msg.startupView == "shelf" {
				m.setMode(modeLauncher)
			} else if msg.startupView == "prompt" {
				m.setMode(modePrompt)
			} else if msg.startupView == "home" {
				m.setMode(modeHome)
			}
		}
		return m, themeCmd
	case themeResolveMsg:
		if msg.name != m.pendingTheme {
			// A newer selection superseded this lookup; drop it.
			return m, nil
		}
		m.pendingTheme = ""
		if msg.theme != nil {
			m.SetTheme(*msg.theme, m.kit.Mode())
			m.applyMotionPolicy()
		} else {
			m.unresolvedTheme = msg.name
		}
		return m, nil
	case procUpdateMsg:
		m.initExperience()
		if msg.err == nil && msg.procs != nil && msg.seq >= m.procShown {
			m.procShown = msg.seq
			cmd := m.handleProcessUpdates(msg.procs)
			return m, cmd
		}
		return m, nil
	case procLogsMsg:
		m.initExperience()
		m.applyLogs(msg)
		return m, nil
	case procActionMsg:
		m.initExperience()
		m.procPending = false
		if msg.err != nil {
			m.tasks.SetError(msg.err.Error())
		} else {
			m.tasks.SetStatus(msg.status)
		}
		return m, m.pollProcessesCmd()
	case receiptLogsMsg:
		m.initExperience()
		m.receipts.UpdateExcerpt(msg.receiptID, msg.logs)
		if r, ok := m.receipts.Get(msg.receiptID); ok {
			m.tasks.UpdateReceipt(&r)
		}
		return m, nil
	case viewMsg, viewTickMsg, viewCheckMsg, viewCancelMsg, viewSubscribedMsg, viewEventMsg:
		return m, m.updatePresentation(msg)
	case closedMsg:
		if m.presentation != nil && m.presentation.cancel != nil {
			m.presentation.cancel()
		}
		m.presentation = nil
		return m, tea.Quit
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case resultMsg:
		var cmd tea.Cmd
		m.busy = false
		m.initExperience()
		if msg.switchedUser != "" {
			m.user = msg.switchedUser
		} else if msg.status.User != "" && (m.user == "" || m.user == "guest") {
			m.user = msg.status.User
		}
		if msg.cwd != "" {
			m.cwd = msg.cwd
		}
		if msg.apps != nil {
			m.apps = msg.apps
			m.selected = min(m.selected, max(0, len(m.apps)-1))
		}
		if msg.hasStatus {
			m.homeData.UptimeSeconds = msg.status.UptimeSeconds
			m.homeData.User = msg.status.User
			m.homeData.ProcessCount = msg.status.ProcessCount
			m.homeData.ServicesCount = msg.status.ServicesCount
		}
		if msg.hasDocs {
			m.homeData.Documents = msg.documents
		}
		if msg.switchView == "tasks" {
			m.taskView = true
			m.shelf = false
			m.notifView = false
		} else if msg.switchView == "notifications" {
			m.notifView = true
			m.shelf = false
			m.taskView = false
		} else if msg.switchView == "shelf" || msg.switchView == "launcher" {
			m.setMode(modeLauncher)
			m.taskView = false
			m.notifView = false
		} else if msg.switchView == "settings" {
			return m, m.openAppByID("com.gostalgia.settings")
		}
		if msg.toggleDND {
			val := m.notifs.ToggleDND()
			stateStr := "OFF"
			if val {
				stateStr = "ON (toasts suppressed)"
			}
			m.append(entry{fmt.Sprintf("Do-Not-Disturb is now %s", stateStr), "accent"})
		} else if msg.setDND != nil {
			m.notifs.SetDND(*msg.setDND)
			stateStr := "OFF"
			if *msg.setDND {
				stateStr = "ON (toasts suppressed)"
			}
			m.append(entry{fmt.Sprintf("Do-Not-Disturb is now %s", stateStr), "accent"})
		}
		if msg.listReceipts {
			list := m.receipts.List()
			if len(list) == 0 {
				m.append(entry{"No crash receipts recorded.", "muted"})
			} else {
				m.append(entry{fmt.Sprintf("CRASH RECEIPTS (%d total):", len(list)), "accent"})
				for _, r := range list {
					m.append(entry{fmt.Sprintf("  #%-6s PID %-5d %-16s exit %-3d (%s) at %s",
						r.ID, r.PID, r.Name, r.ExitCode, r.State, r.Timestamp.Format("15:04:05")), "output"})
				}
				m.append(entry{"Type 'receipt PID' to inspect full receipt with logs.", "muted"})
			}
		} else if msg.receiptPID > 0 {
			r, ok := m.receipts.GetByPID(msg.receiptPID)
			if ok {
				m.taskView = true
				m.shelf = false
				m.notifView = false
				m.tasks.ShowReceipt(&r)
			} else {
				m.append(entry{fmt.Sprintf("No crash receipt recorded for PID %d", msg.receiptPID), "error"})
			}
		}
		if msg.setTheme != "" {
			if m.SetThemeByName(msg.setTheme) {
				m.pendingTheme = ""
				m.applyMotionPolicy()
				m.append(entry{fmt.Sprintf("Theme changed to %s", m.kit.Theme().Name), "accent"})
				cmd = m.persistThemeCmd(msg.setTheme)
			} else if msg.userTheme != nil {
				m.pendingTheme = ""
				m.SetTheme(*msg.userTheme, m.kit.Mode())
				m.applyMotionPolicy()
				m.append(entry{fmt.Sprintf("Theme changed to %s", m.kit.Theme().Name), "accent"})
				cmd = m.persistThemeCmd(m.kit.Theme().Name)
			} else {
				m.append(entry{fmt.Sprintf("Unknown theme %q. Available: %s", msg.setTheme, strings.Join(msg.themeNames, ", ")), "error"})
			}
		} else if msg.showTheme {
			rm := "disabled"
			if m.kit.Theme().ReducedMotion {
				rm = "enabled"
			}
			m.append(entry{fmt.Sprintf("Active theme: %s%sMode: %v%sReduced motion: %s", m.kit.Theme().Name, m.kit.Separator(), m.kit.Mode(), m.kit.Separator(), rm), "accent"})
			m.append(entry{"Available themes: " + strings.Join(msg.themeNames, ", "), "muted"})
		}
		if msg.toggleMotion {
			newMotion := !m.kit.Theme().ReducedMotion
			m.setSessionReducedMotion(newMotion)
			state := "disabled"
			if newMotion {
				state = "enabled"
			}
			m.append(entry{fmt.Sprintf("Reduced motion is now %s", state), "accent"})
		} else if msg.setMotion != nil {
			m.setSessionReducedMotion(*msg.setMotion)
			state := "disabled"
			if *msg.setMotion {
				state = "enabled"
			}
			m.append(entry{fmt.Sprintf("Reduced motion is now %s", state), "accent"})
		}
		if msg.setMouse != nil {
			m.setMouse(*msg.setMouse)
		}
		if msg.setMouse != nil || msg.showMouse {
			m.append(entry{m.mouseState(), "accent"})
		}
		if msg.sessionID != "" {
			m.sessionID = msg.sessionID
			m.attachmentID = msg.attachmentID
			if m.attached {
				m.append(entry{fmt.Sprintf("Attached to session %s (clients: %d)", msg.sessionID, msg.activeCount), "accent"})
			}
		}
		if len(msg.workspaceHistory) > 0 {
			m.history = msg.workspaceHistory
			m.historyPos = len(m.history)
		}
		if msg.clearHistory {
			m.history = nil
			m.historyPos = 0
		}
		if m.client != nil && !msg.detach && !msg.quit && (msg.executedLine != "" || msg.cwd != "") {
			c := m.client
			line := msg.executedLine
			cwd := m.cwd
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				params := map[string]any{"cwd": cwd}
				if line != "" {
					params["cmd"] = line
				}
				_ = c.Call(ctx, "session/workspace/set", params, nil)
			}()
		}
		if msg.text != "" {
			m.append(entry{msg.text, "output"})
		}
		if msg.err != nil {
			m.append(entry{msg.err.Error(), "error"})
			if failedLaunchResult(msg) {
				cmd = tea.Batch(cmd, m.startFailedLaunch())
			}
		}
		if msg.detach {
			if m.attached {
				if m.client != nil && m.sessionID != "" && m.attachmentID != "" {
					_ = m.client.Call(context.Background(), "session/detach", map[string]any{
						"session_id":    m.sessionID,
						"attachment_id": m.attachmentID,
					}, nil)
				}
				m.append(entry{"Detached from session.", "accent"})
				return m, tea.Quit
			}
			m.append(entry{"cannot detach from owned session: runtime is running in-process; use 'gostalgia boot' for a detachable headless runtime, or 'shutdown' to stop.", "error"})
			return m, nil
		}
		if msg.quit {
			if m.attached && m.client != nil && m.sessionID != "" && m.attachmentID != "" {
				_ = m.client.Call(context.Background(), "session/detach", map[string]any{
					"session_id":    m.sessionID,
					"attachment_id": m.attachmentID,
				}, nil)
			}
			return m, tea.Quit
		}
		return m, cmd
	case themeSavedMsg:
		if msg.err != nil {
			m.append(entry{fmt.Sprintf("Theme applies to this session only (could not save preference: %s)", msg.err), "muted"})
		} else {
			m.append(entry{"Theme saved to your settings.", "muted"})
		}
		return m, nil
	case tea.KeyboardEnhancementsMsg:
		// The terminal's answer to the enhancement flags View requests.
		m.keyEnh = msg
		return m, nil
	case tea.KeyReleaseMsg:
		// Releases exist only under negotiated event types; they end a
		// held key. Held-key semantics live on the press side (IsRepeat)
		// and the presentation contract stays semantic actions — apps
		// never see keystrokes — so a release is consumed here: key-up
		// never types, submits, or activates.
		return m, nil
	case tea.KeyPressMsg:
		// Kitty-protocol disambiguation reports keypad keys with their own
		// codes; fold them onto the main-keyboard twins so every handler
		// treats keypad input identically. Any associated text the decoder
		// attached (e.g. "\r" on keypad Enter) would shadow the keystroke
		// name, so it is dropped along with the keypad code.
		if code := canonicalKeyCode(msg.Code); code != msg.Code {
			msg.Code = code
			msg.Text = ""
		}
		keyStr := msg.String()
		switch keyStr {
		case "ctrl+c", "ctrl+d":
			if m.attached && m.client != nil && m.sessionID != "" && m.attachmentID != "" {
				_ = m.client.Call(context.Background(), "session/detach", map[string]any{
					"session_id":    m.sessionID,
					"attachment_id": m.attachmentID,
				}, nil)
			}
			return m, tea.Quit
		}

		// Modal layers own every other keystroke: nothing beneath them,
		// shortcuts included, sees a key while one is open.
		if m.confirm != nil {
			return m.handleConfirmKey(msg)
		}
		// super+space is a second palette chord that only key
		// disambiguation can name; it never collides with typing because
		// a super chord carries no text, and terminals without
		// enhancements simply never deliver it.
		if m.isShortcut("palette", keyStr) || keyStr == "super+space" {
			if m.paletteOpen {
				m.closePalette()
			} else {
				m.openPalette()
			}
			return m, nil
		}
		if m.paletteOpen {
			return m.handlePaletteKey(msg)
		}
		if m.taskView && m.tasks.Searching() {
			return m.handleLogSearchKey(msg)
		}
		if m.isShortcut("home", keyStr) {
			return m, m.switchScreen(screenHome, true)
		}
		if m.isShortcut("launcher", keyStr) {
			return m, m.switchScreen(screenApps, true)
		}
		if m.isShortcut("tasks", keyStr) {
			return m, m.switchScreen(screenTasks, true)
		}
		if m.isShortcut("notifications", keyStr) {
			return m, m.switchScreen(screenNotifications, true)
		}
		if m.isShortcut("settings", keyStr) {
			if m.presentation != nil && m.presentation.id == "com.gostalgia.settings" {
				cancel := m.dismissView()
				return m, cancel
			}
			cancel := m.dismissView()
			cmd := m.openAppByID("com.gostalgia.settings")
			if cancel != nil {
				return m, tea.Batch(cancel, cmd)
			}
			return m, cmd
		}
		if m.isShortcut("view", keyStr) {
			if m.presentation == nil {
				return m, m.selectedView()
			}
			return m, nil
		}
		if keyStr == "ctrl+y" {
			// Copy, the keyboard's way to what selecting with the mouse
			// copies. It only reads the screen, so it works while a
			// command runs. A shortcut configured on Ctrl-Y wins, above.
			cmd, ok := m.copyInView()
			if !ok {
				m.initExperience()
				m.flash(nothingToCopy)
			}
			return m, cmd
		}

		switch keyStr {
		case "esc":
			if m.presentation != nil {
				return m, m.viewKey(msg)
			}
			if m.taskView {
				if m.tasks != nil && m.tasks.Mode() != taskmanager.ModeTable {
					m.tasks.CloseModal()
				} else {
					m.taskView = false
				}
				return m, nil
			}
			if m.notifView {
				m.notifView = false
				return m, nil
			}
			if m.currentMode() == modeLauncher {
				return m.handleLauncherKey(msg)
			}
			m.setMode(modePrompt)
			m.shelf = false
			return m, nil
		case "pgup", "pgdown":
			// A focused version-3 region owns page scrolling, an app view
			// scrolls its rows, and an open log tail pages; otherwise the
			// keys page the transcript while it is on screen.
			switch {
			case m.presentation != nil && m.presentation.regionFocused():
				return m, m.viewKey(msg)
			case m.presentation != nil && keyStr == "pgup":
				m.scroll += max(1, m.height/2)
			case m.presentation != nil:
				m.scroll = max(0, m.scroll-max(1, m.height/2))
			case m.taskView && m.tasks.Mode() == taskmanager.ModeLogs:
				m.tasks.ScrollLogs(m.solve(), func(v bloom.ViewportView) bloom.ViewportPos {
					if keyStr == "pgup" {
						return v.PageUp()
					}
					return v.PageDown()
				})
			default:
				m.pageTranscript(keyStr == "pgdown")
			}
			return m, nil
		}
		// The log tail's keys only read what it has, so they work while
		// a command runs.
		if m.taskView && m.presentation == nil && m.tasks.Mode() == taskmanager.ModeLogs {
			return m.handleLogsKey(msg)
		}
		if m.busy {
			return m, nil
		}
		if m.presentation != nil {
			return m, m.viewKey(msg)
		}
		if m.taskView {
			switch msg.String() {
			case "up", "k":
				m.tasks.SelectPrev()
			case "down", "j":
				m.tasks.SelectNext()
			case "enter":
				if msg.IsRepeat {
					break // a held key drills in once, not per auto-repeat
				}
				if m.tasks.Mode() != taskmanager.ModeTable {
					m.tasks.CloseModal()
				} else {
					return m, m.openSelectedTask()
				}
			case "l":
				if m.tasks.Mode() == taskmanager.ModeTable {
					if sel := m.tasks.SelectedProcess(); sel != nil {
						return m, m.fetchLogsCmd(sel.ID, sel.Name, true)
					}
				}
			case "c":
				if m.tasks.Mode() == taskmanager.ModeTable {
					if sel := m.tasks.SelectedProcess(); sel != nil {
						if r, ok := m.receipts.GetByPID(sel.ID); ok {
							m.tasks.ShowReceipt(&r)
						} else {
							m.tasks.SetError(fmt.Sprintf("No crash receipt recorded for PID %d", sel.ID))
						}
					}
				}
			case "x":
				if m.tasks.Mode() == taskmanager.ModeTable && !msg.IsRepeat {
					if sel := m.tasks.SelectedProcess(); sel != nil {
						m.openConfirm(confirmation{kind: confirmStopProcess, pid: sel.ID, name: sel.Name})
					}
				}
			case "r":
				if m.tasks.Mode() == taskmanager.ModeTable && !msg.IsRepeat {
					m.openConfirm(confirmation{kind: confirmReap})
				}
			}
			return m, nil
		}
		if m.notifView {
			switch msg.String() {
			case "up", "k":
				m.notifs.SelectPrev()
			case "down", "j":
				m.notifs.SelectNext()
			case "enter":
				return m, m.openSelectedNotification()
			case "d":
				m.notifs.DismissSelected()
			case "c":
				m.notifs.Clear()
			case "n":
				dnd := m.notifs.ToggleDND()
				if dnd {
					m.append(entry{"Do-Not-Disturb is now ON (toasts suppressed)", "accent"})
				} else {
					m.append(entry{"Do-Not-Disturb is now OFF", "accent"})
				}
			case "a":
				m.notifs.MarkAllSeen()
			}
			return m, nil
		}
		if m.currentMode() == modeLauncher {
			return m.handleLauncherKey(msg)
		}
		if m.currentMode() == modeHome {
			return m.handleHomeKey(msg)
		}
		return m.handlePromptKey(msg)
	case tea.PasteMsg:
		// Bracketed paste delivers the whole paste as one message instead of
		// typed keys; route it into the same insert paths as text input.
		text := safe(msg.Content)
		if text == "" || m.confirm != nil {
			return m, nil
		}
		if m.paletteOpen {
			m.paletteQuery += strings.ReplaceAll(text, "\n", " ")
			m.paletteSelected = 0
			return m, nil
		}
		if m.taskView && m.tasks.Searching() {
			m.tasks.SearchInput(text)
			return m, nil
		}
		if m.busy {
			return m, nil
		}
		if m.presentation != nil {
			return m, m.viewKey(tea.KeyPressMsg{Code: tea.KeyExtended, Text: text})
		}
		if m.taskView || m.notifView {
			return m, nil
		}
		if m.currentMode() == modeLauncher {
			m.launcherQuery += strings.ReplaceAll(text, "\n", " ")
			m.selected = 0
			return m, nil
		}
		m.setMode(modePrompt)
		m.insertPromptText(text)
		return m, nil
	}
	return m, nil
}

// handleLogsKey scrolls, follows and searches the open log tail. Enter, as
// Esc, returns to the table.
func (m *Model) handleLogsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	scroll := func(move func(bloom.ViewportView) bloom.ViewportPos) { m.tasks.ScrollLogs(m.solve(), move) }
	switch msg.String() {
	case "up", "k":
		scroll(func(v bloom.ViewportView) bloom.ViewportPos { return v.Up(1) })
	case "down", "j":
		scroll(func(v bloom.ViewportView) bloom.ViewportPos { return v.Down(1) })
	case "home", "g":
		scroll(bloom.ViewportView.Top)
	case "end", "G":
		scroll(bloom.ViewportView.Bottom)
	case "f":
		m.tasks.ToggleFollow(m.solve())
	case "/":
		m.tasks.StartSearch()
	case "n":
		m.tasks.SeekMatch(m.solve(), true)
	case "N":
		m.tasks.SeekMatch(m.solve(), false)
	case "enter":
		if !msg.IsRepeat {
			m.tasks.CloseModal()
		}
	}
	return m, nil
}

// handleLogSearchKey types the log tail's search. Until Enter finds it or
// Esc cancels it, it takes every key but Ctrl-C, Ctrl-D and the palette,
// which, like any modal layer, comes back to the search as it was.
func (m *Model) handleLogSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.tasks.CancelSearch()
	case "enter":
		m.tasks.CommitSearch(m.solve())
	case "backspace", "ctrl+h":
		m.tasks.SearchBackspace()
	default:
		if msg.Text != "" {
			m.tasks.SearchInput(msg.Text)
		}
	}
	return m, nil
}

// deleteClusterBackward removes the grapheme cluster immediately before the
// cursor, the way Backspace and Ctrl-H do.
func (m *Model) deleteClusterBackward() {
	if m.cursor > 0 {
		prev := prevClusterRune(m.input, m.cursor)
		m.input = append(m.input[:prev], m.input[m.cursor:]...)
		m.cursor = prev
	}
}

// canonicalKeyCode maps keypad codes emitted under Kitty-protocol
// disambiguation onto their main-keyboard twins.
func canonicalKeyCode(code rune) rune {
	switch code {
	case tea.KeyKpEnter:
		return tea.KeyEnter
	case tea.KeyKpUp:
		return tea.KeyUp
	case tea.KeyKpDown:
		return tea.KeyDown
	case tea.KeyKpLeft:
		return tea.KeyLeft
	case tea.KeyKpRight:
		return tea.KeyRight
	case tea.KeyKpPgUp:
		return tea.KeyPgUp
	case tea.KeyKpPgDown:
		return tea.KeyPgDown
	case tea.KeyKpHome:
		return tea.KeyHome
	case tea.KeyKpEnd:
		return tea.KeyEnd
	case tea.KeyKpInsert:
		return tea.KeyInsert
	case tea.KeyKpDelete:
		return tea.KeyDelete
	case tea.KeyKpBegin:
		return tea.KeyBegin
	default:
		return code
	}
}

// insertPromptText splices text into the input at the cursor, folding
// newlines into spaces and honoring the input budget. Typed keys and
// bracketed paste share it.
func (m *Model) insertPromptText(text string) {
	text = strings.ReplaceAll(text, "\n", " ")
	runes := []rune(text)
	if len(m.input)+len(runes) <= 4096 {
		tail := append([]rune(nil), m.input[m.cursor:]...)
		m.input = append(append(m.input[:m.cursor], runes...), tail...)
		m.cursor += len(runes)
	}
}

// The prompt editor stays custom rather than adopting bubbles/textinput
// (issue #185). Upstream edits at rune granularity: Backspace and the arrow
// keys would split grapheme clusters (combining marks, ZWJ emoji) mid-cluster,
// which the cluster-navigation tests in this package pin against. It also
// owns a blinking cursor that needs scheduled ticks — against the
// ReducedMotion and stateless-render contract — plus a suggestion engine the
// prompt doesn't use. History and completion stay ours either way.
func (m *Model) handlePromptKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	if text := safe(key.Text); text != "" {
		// Text-bearing keys insert before control-chord handling so Windows
		// AltGr chords (Ctrl+Alt with text) reach the input.
		if len(m.input) == 0 && text == "/" {
			m.openPalette()
			return m, nil
		}
		m.insertPromptText(text)
		return m, nil
	}
	// Ctrl-Enter (kitty-distinguishable) is exempt from the chord table so
	// it reaches the Enter case and submits, matching its app-view meaning.
	if key.Mod.Contains(tea.ModCtrl) && key.Code != tea.KeyEnter {
		switch key.Code {
		case 'a':
			m.cursor = 0
		case 'e':
			m.cursor = len(m.input)
		case 'u':
			m.input = nil
			m.cursor = 0
		case 'h', tea.KeyBackspace:
			m.deleteClusterBackward()
		}
		return m, nil
	}
	switch key.Code {
	case tea.KeyEnter:
		line := strings.TrimSpace(string(m.input))
		if line == "" {
			m.append(entry{dosPath(m.cwd) + ">", "command"})
			m.input = nil
			m.cursor = 0
			return m, nil
		}
		if strings.EqualFold(line, "cls") || strings.EqualFold(line, "clear") {
			m.clearTranscript()
			m.input = nil
			m.cursor = 0
			m.scroll = 0
			return m, nil
		}
		if strings.EqualFold(line, "home") {
			m.input = nil
			m.cursor = 0
			m.setMode(modeHome)
			return m, nil
		}
		if strings.EqualFold(line, "launcher") || strings.EqualFold(line, "shelf") {
			m.input = nil
			m.cursor = 0
			m.setMode(modeLauncher)
			return m, nil
		}
		if strings.EqualFold(line, "palette") {
			m.input = nil
			m.cursor = 0
			m.openPalette()
			return m, nil
		}
		return m, m.run(line)
	case tea.KeyBackspace:
		m.deleteClusterBackward()
	case tea.KeyDelete:
		if m.cursor < len(m.input) {
			next := nextClusterRune(m.input, m.cursor)
			m.input = append(m.input[:m.cursor], m.input[next:]...)
		}
	case tea.KeyLeft:
		m.cursor = prevClusterRune(m.input, m.cursor)
	case tea.KeyRight:
		m.cursor = nextClusterRune(m.input, m.cursor)
	case tea.KeyHome:
		m.cursor = 0
	case tea.KeyEnd:
		m.cursor = len(m.input)
	case tea.KeyUp, tea.KeyDown:
		if len(m.history) == 0 {
			break
		}
		if m.historyPos == len(m.history) {
			m.draft = string(m.input)
		}
		if key.Code == tea.KeyUp {
			m.historyPos = max(0, m.historyPos-1)
		} else {
			m.historyPos = min(len(m.history), m.historyPos+1)
		}
		text := m.draft
		if m.historyPos < len(m.history) {
			text = m.history[m.historyPos]
		}
		m.input = []rune(text)
		m.cursor = len(m.input)
	case tea.KeyTab:
		m.complete()
	}
	return m, nil
}

func (m *Model) complete() {
	prefix := string(m.input)
	choices := []string{"help", "apps", "pkg", "package", "backup", "launch", "run", "stop", "echo", "call", "dir", "ls", "cd", "type", "cat", "ps", "logs", "log", "status", "cls", "exit", "shutdown", "home", "palette", "tasks", "taskmanager", "notifications", "alerts", "receipt", "reap", "dnd", "theme", "motion", "mouse", "settings", "preferences", "profile", "profiles"}
	if verb, partial, ok := strings.Cut(prefix, " "); ok {
		if verb != "launch" && verb != "run" && verb != "stop" && verb != "theme" && verb != "motion" && verb != "mouse" && verb != "profile" && verb != "backup" {
			return
		}
		if verb == "theme" {
			names := []string{"nostalgia", "midnight", "monochrome", "high-contrast", "high-contrast-light"}
			if m.client != nil {
				cctx, cancel := context.WithTimeout(m.ctx, time.Second)
				names = themeNames(cctx, m.client)
				cancel()
			}
			choices = choices[:0]
			for _, n := range names {
				choices = append(choices, "theme "+n)
			}
		} else if verb == "motion" {
			choices = []string{"motion on", "motion off"}
		} else if verb == "mouse" {
			choices = []string{"mouse on", "mouse off"}
		} else if verb == "profile" {
			choices = []string{"profile list", "profile switch", "profile create", "profile delete"}
		} else if verb == "backup" {
			choices = []string{"backup export", "backup inspect", "backup preview", "backup restore"}
		} else {
			choices = nil
			for _, a := range m.apps {
				if strings.HasPrefix(a.Manifest.ID, partial) {
					choices = append(choices, verb+" "+a.Manifest.ID)
				}
			}
		}
	}
	var matches []string
	for _, c := range choices {
		if strings.HasPrefix(c, prefix) {
			matches = append(matches, c)
		}
	}
	sort.Strings(matches)
	if len(matches) == 1 {
		m.input = []rune(matches[0])
		m.cursor = len(m.input)
	}
}

func clusterRuneOffsets(input []rune) []int {
	if len(input) == 0 {
		return []int{0}
	}
	s := string(input)
	clusters := ui.GraphemeClusters(s)
	offsets := make([]int, 0, len(clusters)+1)
	runeIdx := 0
	offsets = append(offsets, runeIdx)
	for _, c := range clusters {
		runeIdx += len([]rune(c))
		offsets = append(offsets, runeIdx)
	}
	return offsets
}

func prevClusterRune(input []rune, cursor int) int {
	if cursor <= 0 {
		return 0
	}
	offsets := clusterRuneOffsets(input)
	prev := 0
	for _, off := range offsets {
		if off >= cursor {
			break
		}
		prev = off
	}
	return prev
}

func nextClusterRune(input []rune, cursor int) int {
	if cursor >= len(input) {
		return len(input)
	}
	offsets := clusterRuneOffsets(input)
	for _, off := range offsets {
		if off > cursor {
			return off
		}
	}
	return len(input)
}

// Theme returns the shell's active theme tokens.
func (m *Model) Theme() theme.Theme {
	return m.kit.Theme()
}

// SetTheme updates the shell's active theme and color capability mode.
func (m *Model) SetTheme(t theme.Theme, mode ui.ColorMode) {
	m.themeMotionDefault = t.ReducedMotion
	m.setKit(ui.New(t, mode))
}

// SetReducedMotion updates the reduced motion preference of the active theme.
func (m *Model) SetReducedMotion(enabled bool) {
	th := m.kit.Theme().WithReducedMotion(enabled)
	m.setKit(ui.New(th, m.kit.Mode()))
}

// applyMotionPolicy reapplies this session's preference after a theme or
// config refresh. With neither an override nor explicit config, it restores
// the recorded user or built-in theme default.
func (m *Model) applyMotionPolicy() {
	if m.motionOverride != nil {
		m.SetReducedMotion(*m.motionOverride)
		return
	}
	if m.configuredMotion != nil {
		m.SetReducedMotion(*m.configuredMotion)
		return
	}
	m.SetReducedMotion(m.themeMotionDefault)
}

func (m *Model) setSessionReducedMotion(enabled bool) {
	m.motionOverride = &enabled
	m.applyMotionPolicy()
}

// SetThemeByName switches the active theme by case-insensitive built-in
// name: nostalgia, midnight, monochrome (or mono), high-contrast (or hc),
// high-contrast-light (or hcl). User themes from /config/themes are not
// handled here — they are resolved through theme/get (see resolveUserTheme
// and resolveThemeCmd). Returns true if the theme was recognized.
func (m *Model) SetThemeByName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "nostalgia", "default":
		m.SetTheme(theme.Nostalgia(), ui.ANSI256)
		return true
	case "midnight", "dark":
		m.SetTheme(theme.Midnight(), ui.ANSI256)
		return true
	case "monochrome", "mono", "plain":
		m.SetTheme(theme.Monochrome(), ui.Plain)
		return true
	case "high-contrast", "highcontrast", "hc":
		m.SetTheme(theme.HighContrast(), ui.ANSI256)
		return true
	case "high-contrast-light", "highcontrastlight", "hcl":
		m.SetTheme(theme.HighContrastLight(), ui.ANSI256)
		return true
	default:
		return false
	}
}

func safe(s string) string { return ui.Sanitize(s) }

func dosPath(p string) string { return "C:" + strings.ReplaceAll(p, "/", `\`) }

// RestoreTerminal writes explicit control sequences to stdout to restore standard
// terminal state (show cursor, exit alternate screen, reset styles, disable mouse
// tracking, disable bracketed paste, reset keyboard enhancements) on normal and
// abnormal exits.
func RestoreTerminal() {
	Restore(os.Stdout)
}

// Restore writes reset and recovery sequences to the given writer.
func Restore(w io.Writer) {
	if w == nil {
		return
	}
	// \x1b[<1u pops the Kitty keyboard enhancement entry and \x1b[>4m resets
	// modifyOtherKeys — the keyboard modes v2 enables while running.
	_, _ = io.WriteString(w, "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[<1u\x1b[>4m\x1b[?25h\x1b[0m\x1b[?1049l")
}

// Run takes over the terminal, restoring it on every exit. Passing options is
// useful for tests; production runs in the alternate screen declared by View.
func Run(ctx context.Context, c Caller, closed <-chan struct{}, notifs *notifications.Manager, options ...tea.ProgramOption) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The kit owns color downsampling by contract; pin the program's profile
	// so terminal detection (or NO_COLOR) never re-downsamples kit output
	// under the user's color_mode.
	opts := []tea.ProgramOption{tea.WithContext(ctx), tea.WithColorProfile(colorprofile.TrueColor)}
	opts = append(opts, options...)
	model := NewWithNotifs(ctx, c, closed, notifs)
	defer func() {
		if cleanup := model.dismissView(); cleanup != nil {
			cleanup()
		}
		RestoreTerminal()
	}()
	_, err = tea.NewProgram(model, opts...).Run()
	return err
}

// RunAttached runs the Charm shell attached to an existing runtime session.
// In attached mode, exit, detach, and interrupt leave the headless runtime running.
func RunAttached(ctx context.Context, c Caller, closed <-chan struct{}, options ...tea.ProgramOption) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Same profile pin as Run: the kit downsamples, the program does not.
	opts := []tea.ProgramOption{tea.WithContext(ctx), tea.WithColorProfile(colorprofile.TrueColor)}
	opts = append(opts, options...)
	model := New(ctx, c, closed)
	model.SetAttached(true)
	defer func() {
		if cleanup := model.dismissView(); cleanup != nil {
			cleanup()
		}
		RestoreTerminal()
	}()
	_, err = tea.NewProgram(model, opts...).Run()
	return err
}

// AttachedProgram is the program-construction half of RunAttached for
// transports that must drive the program loop and the terminal themselves —
// the SSH attach listener wires input, output, environment and window size
// from the SSH session. It returns the program, the model (for
// SetAttachIdentity and SessionIdentity), and the teardown the caller must
// run after Program.Run exits: presentation-view dismissal plus context
// release. Restoring the caller's terminal is the transport's job (Restore).
func AttachedProgram(ctx context.Context, c Caller, closed <-chan struct{}, options ...tea.ProgramOption) (*tea.Program, *Model, func()) {
	ctx, cancel := context.WithCancel(ctx)
	opts := []tea.ProgramOption{tea.WithContext(ctx)}
	opts = append(opts, options...)
	model := New(ctx, c, closed)
	model.SetAttached(true)
	teardown := func() {
		defer cancel()
		if cleanup := model.dismissView(); cleanup != nil {
			cleanup()
		}
	}
	return tea.NewProgram(model, opts...), model, teardown
}

// Keep the public boundary small: no runtime imports and no terminal IO in apps.
var _ tea.Model = (*Model)(nil)
