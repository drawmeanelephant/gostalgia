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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

const maxTranscript = 400

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
	theme         string
	colorMode     string
	reducedMotion *bool
	startupView   string
	dnd           *bool
	shortcuts     map[string]string
}

// Model owns UI state; tea.Cmd does IO off the event-loop goroutine.
type Model struct {
	ctx             context.Context
	client          Caller
	closed          <-chan struct{}
	width, height   int
	cwd             string
	input           []rune
	cursor          int
	transcript      []entry
	history         []string
	historyPos      int
	draft           string
	apps            []appStatus
	selected        int
	shelf           bool
	busy            bool
	scroll          int
	kit             ui.Kit
	presentation    *appView
	viewEpoch       uint64
	mode            viewMode
	homeData        homeData
	homeSelected    int
	launcherQuery   string
	paletteOpen     bool
	paletteQuery    string
	paletteSelected int
	previousMode    viewMode

	taskView  bool
	notifView bool
	tasks     *taskmanager.Model
	notifs    *notifications.Manager
	receipts  *receipts.Store
	lastProcs map[int32]procTracking

	shortcuts           map[string]string
	initialConfigLoaded bool

	attached     bool
	sessionID    string
	attachmentID string
	user         string
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

func (m *Model) Attached() bool {
	return m.attached
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
	return &Model{
		ctx: ctx, client: c, closed: closed, kit: ui.New(t, mode), width: 80, height: 24, cwd: "/users/guest",
		mode: modeHome,
		transcript: []entry{
			{"Welcome home. A familiar prompt. A whole new environment.", "accent"},
			{"Type help to explore, or F2 to open your app shelf.", "muted"},
		},
		tasks:     taskmanager.New(),
		notifs:    notifs,
		receipts:  receipts.NewStore(50),
		lastProcs: make(map[int32]procTracking),
		user:      "guest",
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *Model) pollProcessesCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		rows, err := taskmanager.FetchProcesses(c, client)
		return procUpdateMsg{procs: rows, err: err}
	}
}

func (m *Model) fetchLogsCmd(pid int32, name string) tea.Cmd {
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		logs, err := taskmanager.FetchLogs(c, client, pid, 100)
		return procLogsMsg{pid: pid, name: name, content: logs, err: err}
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
		}
		if err := client.Call(c, "config/list", nil, &out); err != nil {
			return nil
		}
		return m.parseConfigMsg(out.Effective)
	}
}

func (m *Model) parseConfigMsg(eff map[string]any) configUpdateMsg {
	msg := configUpdateMsg{
		shortcuts: make(map[string]string),
	}
	if eff == nil {
		return msg
	}
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
					return resultMsg{err: err}
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
		return resultMsg{err: fmt.Errorf("app %s not found", id)}
	}
}

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
	m.kit = ui.New(m.kit.Theme(), mode)
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
			if aErr := m.client.Call(ctx, "session/attach", map[string]any{"client_type": "shell"}, &attResp); aErr == nil {
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
	m.scroll = 0
	m.append(entry{dosPath(m.cwd) + "> " + line, "command"})
	m.history = append(m.history, line)
	if len(m.history) > 100 {
		m.history = m.history[len(m.history)-100:]
	}
	m.historyPos = len(m.history)
	m.input, m.cursor, m.draft = nil, 0, ""
	cwd := m.cwd
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		return execute(ctx, m.client, cwd, line)
	}
}

func (m *Model) append(e entry) {
	// Never render terminal controls from app/file output (escape sequences,
	// OSC clipboard commands, carriage return spoofing, etc.).
	e.text = safe(e.text)
	// Keep memory bounded even when a service returns an enormous file.
	r := []rune(e.text)
	if len(r) > 12000 {
		e.text = string(r[:12000]) + "\n[output truncated]"
	}
	for _, line := range strings.Split(e.text, "\n") {
		m.transcript = append(m.transcript, entry{line, e.kind})
	}
	if len(m.transcript) > maxTranscript {
		m.transcript = m.transcript[len(m.transcript)-maxTranscript:]
	}
	if e.kind == "error" {
		if m.presentation == nil {
			m.setMode(modePrompt)
		}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.initExperience()
		m.notifs.Tick()
		var cmds []tea.Cmd
		cmds = append(cmds, tickCmd())
		if !m.busy {
			cmds = append(cmds, m.pollProcessesCmd())
			if time.Time(msg).Second()%5 == 0 {
				cmds = append(cmds, m.fetchConfigCmd())
			}
		}
		return m, tea.Batch(cmds...)
	case openAppMsg:
		return m, m.openView(msg.app)
	case configUpdateMsg:
		if msg.theme != "" {
			m.SetThemeByName(msg.theme)
		}
		if msg.colorMode != "" {
			m.SetColorModeByName(msg.colorMode)
		}
		if msg.reducedMotion != nil {
			m.SetReducedMotion(*msg.reducedMotion)
		}
		if msg.dnd != nil && m.notifs != nil {
			m.notifs.SetDND(*msg.dnd)
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
		return m, nil
	case procUpdateMsg:
		m.initExperience()
		if msg.err == nil && msg.procs != nil {
			cmd := m.handleProcessUpdates(msg.procs)
			return m, cmd
		}
		return m, nil
	case procLogsMsg:
		m.initExperience()
		if msg.err != nil {
			m.tasks.SetError(msg.err.Error())
		} else {
			m.tasks.ShowLogs(msg.pid, msg.name, msg.content)
		}
		return m, nil
	case procActionMsg:
		m.initExperience()
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
	case viewMsg, viewTickMsg, viewCheckMsg, viewCancelMsg:
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
				m.append(entry{fmt.Sprintf("Theme changed to %s", m.kit.Theme().Name), "accent"})
			} else {
				m.append(entry{fmt.Sprintf("Unknown theme %q. Available: nostalgia, midnight, monochrome, high-contrast, high-contrast-light", msg.setTheme), "error"})
			}
		} else if msg.showTheme {
			rm := "disabled"
			if m.kit.Theme().ReducedMotion {
				rm = "enabled"
			}
			m.append(entry{fmt.Sprintf("Active theme: %s · Mode: %v · Reduced motion: %s", m.kit.Theme().Name, m.kit.Mode(), rm), "accent"})
			m.append(entry{"Available themes: nostalgia, midnight, monochrome, high-contrast, high-contrast-light", "muted"})
		}
		if msg.toggleMotion {
			newMotion := !m.kit.Theme().ReducedMotion
			m.SetReducedMotion(newMotion)
			state := "disabled"
			if newMotion {
				state = "enabled"
			}
			m.append(entry{fmt.Sprintf("Reduced motion is now %s", state), "accent"})
		} else if msg.setMotion != nil {
			m.SetReducedMotion(*msg.setMotion)
			state := "disabled"
			if *msg.setMotion {
				state = "enabled"
			}
			m.append(entry{fmt.Sprintf("Reduced motion is now %s", state), "accent"})
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
	case tea.KeyMsg:
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

		if m.isShortcut("palette", keyStr) {
			if m.paletteOpen {
				m.closePalette()
			} else {
				m.openPalette()
			}
			return m, nil
		}
		if m.isShortcut("home", keyStr) {
			if m.paletteOpen {
				m.closePalette()
			}
			cancel := m.dismissView()
			m.taskView = false
			m.notifView = false
			if m.currentMode() == modeHome {
				m.setMode(modePrompt)
			} else {
				m.setMode(modeHome)
			}
			m.scroll = 0
			return m, cancel
		}
		if m.isShortcut("launcher", keyStr) {
			if m.paletteOpen {
				m.closePalette()
			}
			cancel := m.dismissView()
			m.taskView = false
			m.notifView = false
			if m.currentMode() == modeLauncher {
				m.setMode(modePrompt)
			} else {
				m.setMode(modeLauncher)
			}
			m.scroll = 0
			return m, cancel
		}
		if m.isShortcut("tasks", keyStr) {
			if m.paletteOpen {
				m.closePalette()
			}
			cancel := m.dismissView()
			m.taskView = !m.taskView
			if m.taskView {
				m.shelf = false
				m.notifView = false
				m.setMode(modePrompt)
			}
			m.scroll = 0
			return m, cancel
		}
		if m.isShortcut("notifications", keyStr) {
			if m.paletteOpen {
				m.closePalette()
			}
			cancel := m.dismissView()
			m.notifView = !m.notifView
			if m.notifView {
				m.shelf = false
				m.taskView = false
				m.setMode(modePrompt)
			}
			m.scroll = 0
			return m, cancel
		}
		if m.isShortcut("settings", keyStr) {
			if m.paletteOpen {
				m.closePalette()
			}
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

		switch keyStr {
		case "esc":
			if m.paletteOpen {
				m.closePalette()
				return m, nil
			}
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
		case "pgup":
			m.scroll += max(1, m.height/2)
			return m, nil
		case "pgdown":
			m.scroll = max(0, m.scroll-max(1, m.height/2))
			return m, nil
		}
		if m.paletteOpen {
			return m.handlePaletteKey(msg)
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
				if m.tasks.Mode() != taskmanager.ModeTable {
					m.tasks.CloseModal()
				} else if sel := m.tasks.SelectedProcess(); sel != nil {
					if r, ok := m.receipts.GetByPID(sel.ID); ok {
						m.tasks.ShowReceipt(&r)
					} else {
						return m, m.fetchLogsCmd(sel.ID, sel.Name)
					}
				}
			case "l":
				if m.tasks.Mode() == taskmanager.ModeTable {
					if sel := m.tasks.SelectedProcess(); sel != nil {
						return m, m.fetchLogsCmd(sel.ID, sel.Name)
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
				if m.tasks.Mode() == taskmanager.ModeTable {
					if sel := m.tasks.SelectedProcess(); sel != nil {
						return m, m.stopProcessCmd(sel.ID)
					}
				}
			case "r":
				if m.tasks.Mode() == taskmanager.ModeTable {
					return m, m.reapProcessesCmd()
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
				if sel := m.notifs.Selected(); sel != nil {
					m.notifs.MarkSeen(sel.ID)
					if sel.Receipt != nil {
						m.taskView = true
						m.notifView = false
						m.tasks.ShowReceipt(sel.Receipt)
					}
				}
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
	}
	return m, nil
}

func (m *Model) handlePromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		line := strings.TrimSpace(string(m.input))
		if line == "" {
			m.append(entry{dosPath(m.cwd) + ">", "command"})
			m.input = nil
			m.cursor = 0
			return m, nil
		}
		if strings.EqualFold(line, "cls") || strings.EqualFold(line, "clear") {
			m.transcript = nil
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
		return m, m.submit(line)
	case tea.KeyBackspace, tea.KeyCtrlH:
		if m.cursor > 0 {
			prev := prevClusterRune(m.input, m.cursor)
			m.input = append(m.input[:prev], m.input[m.cursor:]...)
			m.cursor = prev
		}
	case tea.KeyDelete:
		if m.cursor < len(m.input) {
			next := nextClusterRune(m.input, m.cursor)
			m.input = append(m.input[:m.cursor], m.input[next:]...)
		}
	case tea.KeyLeft:
		m.cursor = prevClusterRune(m.input, m.cursor)
	case tea.KeyRight:
		m.cursor = nextClusterRune(m.input, m.cursor)
	case tea.KeyHome, tea.KeyCtrlA:
		m.cursor = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		m.cursor = len(m.input)
	case tea.KeyCtrlU:
		m.input = nil
		m.cursor = 0
	case tea.KeyUp, tea.KeyDown:
		if len(m.history) == 0 {
			break
		}
		if m.historyPos == len(m.history) {
			m.draft = string(m.input)
		}
		if msg.Type == tea.KeyUp {
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
	case tea.KeyRunes, tea.KeySpace:
		if len(m.input) == 0 && msg.Type == tea.KeyRunes && string(msg.Runes) == "/" {
			m.openPalette()
			return m, nil
		}
		text := safe(string(msg.Runes))
		if msg.Type == tea.KeySpace {
			text = " "
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if len(m.input)+len([]rune(text)) <= 4096 {
			tail := append([]rune(nil), m.input[m.cursor:]...)
			m.input = append(append(m.input[:m.cursor], []rune(text)...), tail...)
			m.cursor += len([]rune(text))
		}
	}
	return m, nil
}

func (m *Model) complete() {
	prefix := string(m.input)
	choices := []string{"help", "apps", "pkg", "package", "backup", "launch", "run", "stop", "echo", "call", "dir", "ls", "cd", "type", "cat", "ps", "logs", "log", "status", "cls", "exit", "shutdown", "home", "palette", "tasks", "taskmanager", "notifications", "alerts", "receipt", "reap", "dnd", "theme", "motion", "settings", "preferences", "profile", "profiles"}
	if verb, partial, ok := strings.Cut(prefix, " "); ok {
		if verb != "launch" && verb != "run" && verb != "stop" && verb != "theme" && verb != "motion" && verb != "profile" && verb != "backup" {
			return
		}
		if verb == "theme" {
			choices = []string{"theme nostalgia", "theme midnight", "theme monochrome", "theme high-contrast", "theme high-contrast-light"}
		} else if verb == "motion" {
			choices = []string{"motion on", "motion off"}
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
	m.kit = ui.New(t, mode)
}

// SetReducedMotion updates the reduced motion preference of the active theme.
func (m *Model) SetReducedMotion(enabled bool) {
	th := m.kit.Theme().WithReducedMotion(enabled)
	m.kit = ui.New(th, m.kit.Mode())
}

// SetThemeByName switches the active theme by case-insensitive name.
// Supported names: nostalgia, midnight, monochrome (or mono), high-contrast (or hc),
// high-contrast-light (or hcl). Returns true if the theme was recognized.
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

func (m *Model) View() string {
	m.initExperience()
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	bounds := ui.Bounds{Width: m.width, Height: m.height}
	if m.width < 30 || m.height < 10 {
		return ui.Fit(m.kit.Text("GOSTALGIA\nResize to 30×10 or larger.\nCtrl-C exits."), bounds)
	}
	panelBounds := ui.Bounds{Width: m.width, Height: m.height - 1}
	w := m.kit.PanelContentBounds(panelBounds).Width

	active := 0
	switch m.currentMode() {
	case modeHome:
		active = 0
	case modeLauncher:
		active = 1
	case modePrompt:
		active = 2
	}
	if m.taskView {
		active = 3
	} else if m.notifView {
		active = 4
	}

	notifLabel := "Notifications"
	if m.notifs != nil && m.notifs.UnseenCount() > 0 {
		notifLabel = fmt.Sprintf("Notifications (%d)", m.notifs.UnseenCount())
	}
	tabItems := []ui.Tab{
		{Label: "Home"},
		{Label: "Apps"},
		{Label: "Prompt"},
		{Label: "Tasks"},
		{Label: notifLabel},
	}
	if m.presentation != nil {
		tabItems = append(tabItems, ui.Tab{Label: viewText(m.presentation.data.Title)})
		active = len(tabItems) - 1
	}
	tabs := m.kit.Tabs(tabItems, active, w)
	statusLabel := "ONLINE"
	if m.attached {
		statusLabel = "ATTACHED"
	}
	userName := viewText(m.user)
	if userName == "" {
		userName = "guest"
	}
	badge := m.kit.Badge(statusLabel, theme.Success, w) + m.kit.Muted(fmt.Sprintf("   %s · C: environment drive", userName))

	toastView := ""
	toastLines := 0
	if m.notifs != nil && !m.notifView {
		toastView = m.notifs.RenderToasts(m.kit, w)
		if toastView != "" {
			toastLines = strings.Count(toastView, "\n") + 1
		}
	}

	bodyHeight := max(1, m.height-9-toastLines)
	var lines []string

	if m.paletteOpen {
		lines = m.renderPalette(w, bodyHeight)
	} else if m.taskView && m.tasks != nil {
		panelStr := m.tasks.Render(m.kit, ui.Bounds{Width: w, Height: bodyHeight})
		lines = strings.Split(panelStr, "\n")
	} else if m.notifView && m.notifs != nil {
		panelStr := m.notifs.RenderPanel(m.kit, ui.Bounds{Width: w, Height: bodyHeight})
		lines = strings.Split(panelStr, "\n")
	} else if m.presentation != nil {
		lines = m.viewLines(bodyHeight)
		for i, line := range lines {
			lines[i] = ui.Truncate(line, w)
		}
	} else {
		switch m.currentMode() {
		case modeLauncher:
			lines = m.renderLauncher(w, bodyHeight)
		case modeHome:
			lines = m.renderHome(w, bodyHeight)
		default:
			for _, e := range m.transcript {
				line := ui.Wrap(e.text, w)
				switch e.kind {
				case "accent":
					line = m.kit.Heading(line)
				case "muted":
					line = m.kit.Muted(line)
				case "command":
					line = m.kit.Heading(line)
				case "error":
					line = m.kit.StatusText(line, theme.Error)
				default:
					line = m.kit.Text(line)
				}
				lines = append(lines, strings.Split(line, "\n")...)
			}
		}
	}

	if m.paletteOpen || m.currentMode() == modeLauncher || m.currentMode() == modeHome || m.presentation != nil || m.taskView || m.notifView {
		if len(lines) > bodyHeight {
			lines = lines[:bodyHeight]
		}
	} else {
		end := max(0, len(lines)-min(m.scroll, max(0, len(lines)-bodyHeight)))
		start := max(0, end-bodyHeight)
		lines = lines[start:end]
	}
	body := ui.Fit(strings.Join(lines, "\n"), ui.Bounds{Width: w, Height: bodyHeight})
	if toastView != "" {
		body = body + "\n" + toastView
	}

	promptPath := safe(dosPath(m.cwd))
	if lipgloss.Width(promptPath) > w/2 {
		promptPath = "C:…" + ui.Tail(promptPath, max(1, w/2-3))
	}
	prompt := m.kit.Heading(promptPath + "> ")
	if m.paletteOpen {
		prompt = m.kit.Heading("PALETTE> ") + m.kit.Muted("Type to filter · ↑↓ navigate · Enter execute · Esc close")
	} else if m.presentation != nil {
		prompt += m.kit.Muted("App view owns focus. Esc returns to prompt.")
	} else if m.taskView {
		prompt += m.kit.Muted("Task Manager owns focus. Esc returns to prompt.")
	} else if m.notifView {
		prompt += m.kit.Muted("Notification Center owns focus. Esc returns to prompt.")
	} else if m.busy {
		prompt += m.kit.Badge("", theme.Busy, max(0, w-lipgloss.Width(prompt)))
	} else if m.currentMode() == modeLauncher {
		filtered := m.filteredApps()
		if len(filtered) > 0 && m.selected < len(filtered) && filtered[m.selected].Running {
			prompt += m.kit.StatusText(fmt.Sprintf("%s is LIVE (PID %d). Enter relaunch, F3 stop, F4 view.", viewText(filtered[m.selected].Manifest.Name), filtered[m.selected].PID), theme.Success)
		} else if len(filtered) > 0 && m.selected < len(filtered) {
			prompt += m.kit.Muted(fmt.Sprintf("Enter launches %s. F3 stop · F4 view · Esc prompt.", viewText(filtered[m.selected].Manifest.Name)))
		} else {
			prompt += m.kit.Muted("Esc returns to prompt.")
		}
	} else if m.currentMode() == modeHome {
		prompt += m.kit.Muted("Home view owns focus. Esc returns to prompt · Ctrl-P palette.")
	} else {
		// Show a cursor-centered slice rather than allowing long pasted input to
		// push the prompt off screen. Cell width, not bytes, controls the slice.
		room := max(1, w-lipgloss.Width(prompt)-1)
		before := ui.Tail(string(m.input[:m.cursor]), room/2)
		after := ui.Fit(string(m.input[m.cursor:]), ui.Bounds{Width: room - lipgloss.Width(before), Height: 1})
		prompt += m.kit.Text(before) + m.kit.Selection(" ") + m.kit.Text(after)
	}

	exitHelp := "EXIT"
	if m.attached {
		exitHelp = "DETACH"
	}

	var bindings []ui.Binding
	if m.paletteOpen {
		items := m.filteredPaletteItems()
		bindings = []ui.Binding{
			{Key: "Esc", Help: "CLOSE"},
			{Key: "Enter", Help: "EXECUTE", Disabled: len(items) == 0},
			{Key: "↑↓", Help: "NAVIGATE", Disabled: len(items) == 0},
			{Key: "Ctrl-C", Help: exitHelp},
		}
	} else if m.taskView && m.tasks != nil {
		if m.tasks.Mode() == taskmanager.ModeLogs {
			bindings = []ui.Binding{
				{Key: "Esc", Help: "BACK"}, {Key: "↑↓", Help: "SCROLL"}, {Key: "Ctrl-C", Help: exitHelp},
			}
		} else if m.tasks.Mode() == taskmanager.ModeReceipt {
			bindings = []ui.Binding{
				{Key: "Esc", Help: "BACK"}, {Key: "Ctrl-C", Help: exitHelp},
			}
		} else {
			bindings = []ui.Binding{
				{Key: "Esc", Help: "PROMPT"}, {Key: "Enter", Help: "VIEW"},
				{Key: "l", Help: "LOGS"}, {Key: "c", Help: "RECEIPT"},
				{Key: "x", Help: "STOP"}, {Key: "r", Help: "REAP"},
				{Key: "↑↓", Help: "SELECT"}, {Key: "Ctrl-C", Help: exitHelp},
			}
		}
	} else if m.notifView {
		bindings = []ui.Binding{
			{Key: "Esc", Help: "PROMPT"}, {Key: "Enter", Help: "VIEW"},
			{Key: "d", Help: "DISMISS"}, {Key: "c", Help: "CLEAR"},
			{Key: "n", Help: "DND"}, {Key: "↑↓", Help: "SELECT"},
			{Key: "Ctrl-C", Help: exitHelp},
		}
	} else if m.presentation != nil {
		v := m.presentation
		blocked := v.busy || v.instance == "" || v.data.State == sdk.ViewLoading
		action := max(0, v.focus-len(v.data.Fields))
		actionBlocked := blocked || action >= len(v.data.Actions) || v.data.Actions[action].Disabled
		bindings = []ui.Binding{
			{Key: "Esc", Help: "CANCEL/BACK"}, {Key: "Ctrl-C", Help: exitHelp},
			{Key: "Tab", Help: "FOCUS", Disabled: blocked}, {Key: "Enter", Help: "ACTION", Disabled: actionBlocked},
			{Key: "↑↓", Help: "ITEM", Disabled: blocked}, {Key: "F2", Help: "APPS"},
		}
	} else if m.currentMode() == modeLauncher {
		filtered := m.filteredApps()
		bindings = []ui.Binding{
			{Key: "Esc", Help: "PROMPT"},
			{Key: "Enter", Help: "RUN", Disabled: m.busy || len(filtered) == 0},
			{Key: "F3", Help: "STOP", Disabled: m.busy || len(filtered) == 0},
			{Key: "F4", Help: "VIEW", Disabled: m.busy || len(filtered) == 0},
			{Key: "F5", Help: "TASKS"},
			{Key: "↑↓", Help: "SELECT", Disabled: m.busy || len(filtered) == 0},
			{Key: "Ctrl-P", Help: "PALETTE"},
		}
	} else if m.currentMode() == modeHome {
		items := m.homeItems()
		bindings = []ui.Binding{
			{Key: "F1", Help: "PROMPT"},
			{Key: "F2", Help: "APPS"},
			{Key: "F5", Help: "TASKS"},
			{Key: "F6", Help: "ALERTS"},
			{Key: "Ctrl-P", Help: "PALETTE"},
			{Key: "↑↓", Help: "SELECT", Disabled: len(items) == 0},
			{Key: "Enter", Help: "OPEN", Disabled: len(items) == 0},
			{Key: "Ctrl-C", Help: exitHelp},
		}
	} else {
		bindings = []ui.Binding{
			{Key: "Ctrl-C", Help: exitHelp},
			{Key: "F1", Help: "HOME"},
			{Key: "F2", Help: "APPS"},
			{Key: "F5", Help: "TASKS"},
			{Key: "F6", Help: "ALERTS"},
			{Key: "Tab", Help: "COMPLETE", Disabled: m.busy},
			{Key: "↑↓", Help: "HISTORY", Disabled: m.busy},
			{Key: "Ctrl-P", Help: "PALETTE"},
		}
	}

	content := strings.Join([]string{tabs, ui.Truncate(badge, w), "", body, "", ui.Truncate(prompt, w)}, "\n")
	return m.kit.Panel(ui.Panel{Title: "G O S T A L G I A  /  PERSONAL COMPUTING", Body: content},
		panelBounds) + "\n" + m.kit.HelpBar(bindings, m.width)
}

// RestoreTerminal writes explicit control sequences to stdout to restore standard
// terminal state (show cursor, exit alternate screen, reset styles, disable mouse
// tracking, disable bracketed paste) on normal and abnormal exits.
func RestoreTerminal() {
	Restore(os.Stdout)
}

// Restore writes reset and recovery sequences to the given writer.
func Restore(w io.Writer) {
	if w == nil {
		return
	}
	_, _ = io.WriteString(w, "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?25h\x1b[0m\x1b[?1049l")
}

// Run takes over the terminal, restoring it on every exit. Passing options is
// useful for tests; production runs in the alternate screen with bracketed paste.
func Run(ctx context.Context, c Caller, closed <-chan struct{}, notifs *notifications.Manager, options ...tea.ProgramOption) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(ctx)}
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
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(ctx)}
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

// Keep the public boundary small: no runtime imports and no terminal IO in apps.
var _ tea.Model = (*Model)(nil)
