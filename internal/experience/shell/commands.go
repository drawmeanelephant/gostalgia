package shell

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	"gostalgia/internal/pkg"
	"gostalgia/internal/recovery"
)

// Caller is the single environment API used by the experience layer.
type Caller interface {
	Call(context.Context, string, any, any) error
}

type appStatus struct {
	Manifest struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Version     string   `json:"version"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	} `json:"manifest"`
	Running bool  `json:"running"`
	PID     int32 `json:"pid"`
}

type sysStatusData struct {
	UptimeSeconds float64
	User          string
	ProcessCount  int
	ServicesCount int
}

type resultMsg struct {
	text             string
	err              error
	cwd              string
	apps             []appStatus
	status           sysStatusData
	documents        []docShortcut
	hasStatus        bool
	hasDocs          bool
	quit             bool
	switchView       string
	receiptPID       int32
	listReceipts     bool
	toggleDND        bool
	setDND           *bool
	setTheme         string
	showTheme        bool
	setMotion        *bool
	toggleMotion     bool
	detach           bool
	executedLine     string
	clearHistory     bool
	sessionID        string
	attachmentID     string
	activeCount      int
	workspaceHistory []string
	initialView      string
	switchedUser     string
}

const helpText = `COMMAND CENTER
  apps                     installed apps and their grants
  pkg / package            list, inspect, install, update, uninstall, rollback
  backup                   export, inspect, preview, restore portable backups
  launch / run APP-ID       start a manifest-declared app
  stop APP-ID               stop, clean up, retract routes
  echo MESSAGE              talk to the Echo demo
  call METHOD [JSON]        invoke any app or service route
  dir / ls [PATH]           browse the environment drive
  cd PATH                   change directory (C: is the VFS)
  type / cat PATH           read a file
  open PATH [APP-ID]        open document with associated app (handoff)
  search QUERY [PATH]       search documents in environment drive
  recents · favorites       list recent or pinned documents
  tasks / taskmanager       live Task Manager process dashboard
  notifications / alerts    notification center and alert history
  receipt [PID]             view process crash receipts
  reap                      clean up terminated processes
  dnd [on|off]              toggle or set Do-Not-Disturb
  theme [NAME]              switch theme (nostalgia, midnight, monochrome, high-contrast, high-contrast-light)
  motion [on|off]           toggle or set reduced-motion mode
  settings / preferences    interactive system preferences and themes
  history [clear]           view or clear command history
  profile [list|switch|create|delete] manage workspace profiles and switch active user
  session                   view session and client attachment info
  detach                    detach shell (runtime remains running)
  ps · status               processes · system dashboard
  logs / log PID [TAIL]     view child process logs and diagnostics
  cls / clear               clear the transcript
  exit                      leave shell (owned boot shuts down; attached detaches)
  shutdown                  shut down the environment

F1 home · F2 apps · F5 tasks · F6 alerts · F7 settings · Ctrl+P / / palette · F3 stop · F4 view
Tab complete · ↑↓ history · Paths accept /users/guest or C:\users\guest.`

// words handles quoted paths and messages. Backslashes remain literal for DOS
// paths. Raw call JSON is parsed separately so JSON escaping is unchanged.
func words(line string) ([]string, error) {
	var out []string
	var word strings.Builder
	var quote rune
	started := false
	for _, r := range line {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			started = true
		case unicode.IsSpace(r):
			if started {
				out = append(out, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	if started {
		out = append(out, word.String())
	}
	return out, nil
}

func envPath(cwd, input string) string {
	input = strings.ReplaceAll(input, `\`, "/")
	if len(input) >= 2 && strings.EqualFold(input[:2], "c:") {
		input = "/" + strings.TrimPrefix(input[2:], "/")
	}
	if strings.HasPrefix(input, "/") {
		return path.Clean(input)
	}
	return path.Join(cwd, input)
}

func execute(ctx context.Context, c Caller, cwd, line string) resultMsg {
	result := resultMsg{cwd: cwd, executedLine: line}
	text, next, quit, err := command(ctx, c, cwd, line)
	result.text, result.cwd, result.quit, result.err = text, next, quit, err
	if result.text == "__DETACH__" {
		result.detach = true
		result.text = ""
	} else if result.text == "__HISTORY_CLEARED__" {
		result.clearHistory = true
		result.text = "Command history cleared"
	} else if strings.HasPrefix(result.text, "__SWITCH_VIEW__:") {
		result.switchView = strings.TrimPrefix(result.text, "__SWITCH_VIEW__:")
		result.text = ""
	} else if strings.HasPrefix(result.text, "__SHOW_RECEIPT__:") {
		var pid int32
		fmt.Sscan(strings.TrimPrefix(result.text, "__SHOW_RECEIPT__:"), &pid)
		result.receiptPID = pid
		result.text = ""
	} else if result.text == "__LIST_RECEIPTS__" {
		result.listReceipts = true
		result.text = ""
	} else if result.text == "__TOGGLE_DND__" {
		result.toggleDND = true
		result.text = ""
	} else if result.text == "__SET_DND_ON__" {
		on := true
		result.setDND = &on
		result.text = ""
	} else if result.text == "__SET_DND_OFF__" {
		off := false
		result.setDND = &off
		result.text = ""
	} else if strings.HasPrefix(result.text, "__SET_THEME__:") {
		result.setTheme = strings.TrimPrefix(result.text, "__SET_THEME__:")
		result.text = ""
	} else if result.text == "__SHOW_THEME__" {
		result.showTheme = true
		result.text = ""
	} else if strings.HasPrefix(result.text, "__SET_MOTION__:") {
		val := strings.TrimPrefix(result.text, "__SET_MOTION__:") == "on"
		result.setMotion = &val
		result.text = ""
	} else if result.text == "__TOGGLE_MOTION__" {
		result.toggleMotion = true
		result.text = ""
	} else if strings.HasPrefix(result.text, "__SWITCH_PROFILE__:") {
		targetID := strings.TrimPrefix(result.text, "__SWITCH_PROFILE__:")
		result.switchedUser = targetID
		result.cwd = "/users/" + targetID
		result.text = fmt.Sprintf("Switched to profile %s", targetID)
	}
	if !quit {
		var apps []appStatus
		if refreshErr := c.Call(ctx, "app/list", nil, &apps); refreshErr != nil {
			if result.err == nil {
				result.err = fmt.Errorf("refresh apps: %w", refreshErr)
			}
		} else {
			result.apps = apps
		}

		var status struct {
			UptimeSeconds float64 `json:"uptime_seconds"`
			User          string  `json:"user"`
			Processes     []any   `json:"processes"`
			Services      []any   `json:"services"`
		}
		if err := c.Call(ctx, "sys/status", nil, &status); err == nil {
			result.status = sysStatusData{
				UptimeSeconds: status.UptimeSeconds,
				User:          status.User,
				ProcessCount:  len(status.Processes),
				ServicesCount: len(status.Services),
			}
			result.hasStatus = true
		}

		userForDocs := "guest"
		if result.switchedUser != "" {
			userForDocs = result.switchedUser
		} else if result.status.User != "" {
			userForDocs = result.status.User
		}

		var dir struct {
			Entries []struct {
				Name  string `json:"name"`
				IsDir bool   `json:"is_dir"`
				Size  int64  `json:"size"`
			} `json:"entries"`
		}
		docsPath := fmt.Sprintf("/users/%s/documents", userForDocs)
		if err := c.Call(ctx, "fs/list", map[string]string{"path": docsPath}, &dir); err == nil {
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
			result.documents = docs
			result.hasDocs = true
		}
	}
	return result
}

func command(ctx context.Context, c Caller, cwd, line string) (string, string, bool, error) {
	ok := func(s string) (string, string, bool, error) { return s, cwd, false, nil }
	fail := func(err error) (string, string, bool, error) { return "", cwd, false, err }
	// Preserve everything after the raw IPC method as JSON, including quotes.
	head, tail, _ := strings.Cut(strings.TrimSpace(line), " ")
	if strings.EqualFold(head, "call") {
		tail = strings.TrimSpace(tail)
		method, raw, _ := strings.Cut(tail, " ")
		if method == "" {
			return fail(fmt.Errorf("usage: call METHOD [JSON]"))
		}
		var params any
		if strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &params); err != nil {
				return fail(fmt.Errorf("params must be JSON: %w", err))
			}
		}
		var out json.RawMessage
		if err := c.Call(ctx, method, params, &out); err != nil {
			return fail(err)
		}
		pretty, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fail(err)
		}
		return ok(safe(string(pretty)))
	}
	args, err := words(line)
	if err != nil {
		return fail(err)
	}
	if len(args) == 0 {
		return ok("")
	}
	cmd, args := strings.ToLower(args[0]), args[1:]
	switch cmd {
	case "pkg", "package":
		method, params, err := pkg.ParseCommand(args)
		if err != nil {
			return fail(err)
		}
		if params.Path != "" {
			params.Path = envPath(cwd, params.Path)
		}
		var out json.RawMessage
		if err := c.Call(ctx, method, params, &out); err != nil {
			return fail(err)
		}
		pretty, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fail(err)
		}
		return ok(safe(string(pretty)))
	case "backup", "recovery":
		method, params, err := recovery.ParseCommand(args)
		if err != nil {
			return fail(err)
		}
		if ep, ok := params.(recovery.ExportParams); ok && ep.Path != "" {
			ep.Path = envPath(cwd, ep.Path)
			params = ep
		} else if pp, ok := params.(recovery.PathParams); ok && pp.Path != "" {
			pp.Path = envPath(cwd, pp.Path)
			params = pp
		} else if rp, ok := params.(recovery.RestoreParams); ok && rp.Path != "" {
			rp.Path = envPath(cwd, rp.Path)
			params = rp
		}
		var out json.RawMessage
		if err := c.Call(ctx, method, params, &out); err != nil {
			return fail(err)
		}
		pretty, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fail(err)
		}
		return ok(safe(string(pretty)))
	case "help", "?":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: help"))
		}
		return ok(helpText)
	case "detach":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: detach"))
		}
		return "__DETACH__", cwd, true, nil
	case "history":
		if len(args) > 1 {
			return fail(fmt.Errorf("usage: history [clear]"))
		}
		if len(args) == 1 {
			if strings.ToLower(args[0]) == "clear" {
				var out struct {
					History []string `json:"history"`
				}
				if err := c.Call(ctx, "session/workspace/clear", map[string]any{"clear_history": true}, &out); err != nil {
					return fail(err)
				}
				return "__HISTORY_CLEARED__", cwd, false, nil
			}
			return fail(fmt.Errorf("usage: history [clear]"))
		}
		var ws struct {
			History []string `json:"history"`
		}
		if err := c.Call(ctx, "session/workspace/get", nil, &ws); err != nil {
			return fail(err)
		}
		if len(ws.History) == 0 {
			return ok("(no history recorded)")
		}
		var lines []string
		for i, h := range ws.History {
			lines = append(lines, fmt.Sprintf("%4d  %s", i+1, safe(h)))
		}
		return ok(strings.Join(lines, "\n"))
	case "profile", "profiles":
		if len(args) == 0 || (len(args) == 1 && strings.EqualFold(args[0], "list")) {
			var resp struct {
				Profiles []struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"profiles"`
				Active string `json:"active"`
			}
			if err := c.Call(ctx, "profile/list", nil, &resp); err != nil {
				return fail(err)
			}
			var lines []string
			lines = append(lines, "PROFILES")
			for _, p := range resp.Profiles {
				marker := " "
				activeTag := ""
				if p.ID == resp.Active {
					marker = "*"
					activeTag = " [active]"
				}
				desc := ""
				if p.Description != "" {
					desc = " - " + viewText(p.Description)
				}
				lines = append(lines, fmt.Sprintf("%s %s (%s)%s%s", marker, viewText(p.ID), viewText(p.Name), activeTag, desc))
			}
			return ok(strings.Join(lines, "\n"))
		}
		sub := strings.ToLower(args[0])
		switch sub {
		case "switch":
			if len(args) != 2 {
				return fail(fmt.Errorf("usage: profile switch ID"))
			}
			target := strings.TrimSpace(args[1])
			var switched struct {
				ID string `json:"id"`
			}
			if err := c.Call(ctx, "profile/switch", map[string]string{"id": target}, &switched); err != nil {
				return fail(err)
			}
			return "__SWITCH_PROFILE__:" + target, cwd, false, nil
		case "create":
			if len(args) < 2 {
				return fail(fmt.Errorf("usage: profile create ID [NAME]"))
			}
			id := args[1]
			name := id
			if len(args) > 2 {
				name = strings.Join(args[2:], " ")
			}
			var created struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := c.Call(ctx, "profile/create", map[string]string{"id": id, "name": name}, &created); err != nil {
				return fail(err)
			}
			return ok(fmt.Sprintf("Profile %s (%s) created", viewText(created.ID), viewText(created.Name)))
		case "delete":
			if len(args) != 2 {
				return fail(fmt.Errorf("usage: profile delete ID"))
			}
			id := args[1]
			var delResp map[string]any
			if err := c.Call(ctx, "profile/delete", map[string]string{"id": id}, &delResp); err != nil {
				return fail(err)
			}
			return ok(fmt.Sprintf("Profile %s deleted", viewText(id)))
		default:
			return fail(fmt.Errorf("usage: profile [list|switch|create|delete]"))
		}
	case "session":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: session"))
		}
		var detail struct {
			ID   string `json:"id"`
			User struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
			StartedAt   time.Time `json:"started_at"`
			Active      bool      `json:"active"`
			Attachments []struct {
				ID         string    `json:"id"`
				ClientID   string    `json:"client_id"`
				ClientType string    `json:"client_type"`
				AttachedAt time.Time `json:"attached_at"`
			} `json:"attachments"`
		}
		if err := c.Call(ctx, "session/get", nil, &detail); err != nil {
			return fail(err)
		}
		var out []string
		out = append(out, fmt.Sprintf("SESSION %s · User: %s (%s)", viewText(detail.ID), viewText(detail.User.Name), viewText(detail.User.ID)))
		out = append(out, fmt.Sprintf("Started: %s · Active: %t", detail.StartedAt.Format("2006-01-02 15:04:05"), detail.Active))
		out = append(out, fmt.Sprintf("Attached clients: %d", len(detail.Attachments)))
		for _, a := range detail.Attachments {
			out = append(out, fmt.Sprintf("  - %s (%s) client %s, attached %s", viewText(a.ID), viewText(a.ClientType), viewText(a.ClientID), a.AttachedAt.Format("15:04:05")))
		}
		return ok(strings.Join(out, "\n"))
	case "exit", "quit":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: exit"))
		}
		return "", cwd, true, nil
	case "cls", "clear":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: cls"))
		}
		return ok("")
	case "apps":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: apps"))
		}
		var apps []appStatus
		if err := c.Call(ctx, "app/list", nil, &apps); err != nil {
			return fail(err)
		}
		var rows []string
		for _, a := range apps {
			state := "READY"
			if a.Running {
				state = fmt.Sprintf("LIVE · PID %d", a.PID)
			}
			rows = append(rows, fmt.Sprintf("%s  %s v%s\n  %s · caps: %s", safe(a.Manifest.ID), safe(a.Manifest.Name), safe(a.Manifest.Version), state, strings.Join(a.Manifest.Permissions, ", ")))
		}
		return ok(strings.Join(rows, "\n"))
	case "launch", "run", "stop":
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: %s APP-ID", cmd))
		}
		method := "app/launch"
		verb := "Launched"
		if cmd == "stop" {
			method, verb = "app/stop", "Stopped"
		}
		var out json.RawMessage
		if err := c.Call(ctx, method, map[string]string{"id": args[0]}, &out); err != nil {
			return fail(err)
		}
		return ok(verb + " " + safe(args[0]))
	case "echo":
		if len(args) == 0 {
			return fail(fmt.Errorf("usage: echo MESSAGE"))
		}
		var out struct {
			Msg    string `json:"msg"`
			Echoes int64  `json:"echoes"`
		}
		if err := c.Call(ctx, "app/com.gostalgia.echo/echo", map[string]string{"msg": strings.Join(args, " ")}, &out); err != nil {
			return fail(err)
		}
		return ok(fmt.Sprintf("%s   [echo #%d]", safe(out.Msg), out.Echoes))
	case "ls", "dir", "cd":
		if len(args) > 1 || (cmd == "cd" && len(args) != 1) {
			return fail(fmt.Errorf("usage: %s PATH", cmd))
		}
		p := cwd
		if len(args) == 1 {
			p = envPath(cwd, args[0])
		}
		var out struct {
			Entries []struct {
				Name  string `json:"name"`
				IsDir bool   `json:"is_dir"`
				Size  int64  `json:"size"`
			} `json:"entries"`
		}
		if err := c.Call(ctx, "fs/list", map[string]string{"path": p}, &out); err != nil {
			return fail(err)
		}
		if cmd == "cd" {
			return "Directory: " + safe(p), p, false, nil
		}
		rows := []string{"Directory of " + safe(p)}
		for _, e := range out.Entries {
			if e.IsDir {
				rows = append(rows, "  <DIR>       "+safe(e.Name))
			} else {
				rows = append(rows, fmt.Sprintf("  %8d    %s", e.Size, safe(e.Name)))
			}
		}
		return ok(strings.Join(rows, "\n"))
	case "cat", "type":
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: %s PATH", cmd))
		}
		var out struct {
			Data string `json:"data_base64"`
		}
		if err := c.Call(ctx, "fs/read", map[string]string{"path": envPath(cwd, args[0])}, &out); err != nil {
			return fail(err)
		}
		data, err := base64.StdEncoding.DecodeString(out.Data)
		if err != nil {
			return fail(err)
		}
		return ok(safe(string(data)))
	case "open":
		if len(args) < 1 {
			return fail(fmt.Errorf("usage: open PATH [APP-ID]"))
		}
		target := envPath(cwd, args[0])
		appID := ""
		if len(args) >= 2 {
			appID = args[1]
		}
		var res struct {
			Success bool   `json:"success"`
			AppID   string `json:"app_id"`
			Message string `json:"message"`
		}
		if err := c.Call(ctx, "doc/handoff", map[string]any{
			"version": 1,
			"path":    target,
			"app_id":  appID,
			"mode":    "read",
		}, &res); err != nil {
			return fail(fmt.Errorf("open: %w", err))
		}
		if res.Success && res.AppID != "" {
			return ok(fmt.Sprintf("__SWITCH_VIEW__:%s", res.AppID))
		}
		return ok(safe(res.Message))
	case "search", "find":
		if len(args) < 1 {
			return fail(fmt.Errorf("usage: search QUERY [PATH]"))
		}
		q := args[0]
		searchPath := ""
		if len(args) >= 2 {
			searchPath = envPath(cwd, args[1])
		}
		var res struct {
			Results []struct {
				Path string `json:"path"`
				Size int64  `json:"size"`
			} `json:"results"`
			Total int `json:"total"`
		}
		if err := c.Call(ctx, "doc/search", map[string]any{
			"query": q,
			"path":  searchPath,
		}, &res); err != nil {
			return fail(fmt.Errorf("search: %w", err))
		}
		if res.Total == 0 {
			return ok(fmt.Sprintf("No documents found matching %q", q))
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Found %d document(s):\n", res.Total)
		for _, r := range res.Results {
			fmt.Fprintf(&b, "  %-40s %8d B\n", viewText(r.Path), r.Size)
		}
		return ok(strings.TrimRight(b.String(), "\n"))
	case "recents":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: recents"))
		}
		var res struct {
			Entries []struct {
				Path   string `json:"path"`
				Exists bool   `json:"exists"`
			} `json:"entries"`
		}
		if err := c.Call(ctx, "doc/recents", map[string]bool{"verify_exists": true}, &res); err != nil {
			return fail(fmt.Errorf("recents: %w", err))
		}
		if len(res.Entries) == 0 {
			return ok("No recent documents")
		}
		var b strings.Builder
		b.WriteString("RECENT DOCUMENTS\n")
		for _, e := range res.Entries {
			st := ""
			if !e.Exists {
				st = " (missing)"
			}
			fmt.Fprintf(&b, "  %s%s\n", viewText(e.Path), st)
		}
		return ok(strings.TrimRight(b.String(), "\n"))
	case "favorites":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: favorites"))
		}
		var res struct {
			Entries []struct {
				Path  string `json:"path"`
				Label string `json:"label"`
			} `json:"entries"`
		}
		if err := c.Call(ctx, "doc/favorites", nil, &res); err != nil {
			return fail(fmt.Errorf("favorites: %w", err))
		}
		if len(res.Entries) == 0 {
			return ok("No favorite documents")
		}
		var b strings.Builder
		b.WriteString("FAVORITE DOCUMENTS\n")
		for _, e := range res.Entries {
			fmt.Fprintf(&b, "  %-20s %s\n", viewText(e.Label), viewText(e.Path))
		}
		return ok(strings.TrimRight(b.String(), "\n"))
	case "ps":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: ps"))
		}
		var procs []struct {
			ID           int32     `json:"id"`
			Name         string    `json:"name"`
			Kind         string    `json:"kind"`
			State        string    `json:"state"`
			Caps         []string  `json:"caps"`
			StartedAt    time.Time `json:"started_at"`
			ExitedAt     time.Time `json:"exited_at"`
			ExitCode     int       `json:"exit_code"`
			RestartCount int       `json:"restart_count"`
			CrashLoop    bool      `json:"crash_loop"`
		}
		if err := c.Call(ctx, "proc/list", nil, &procs); err != nil {
			return fail(err)
		}
		rows := []string{"PID   PROCESS                          STATE / STATUS     TIME     GRANT"}
		for _, p := range procs {
			stateDesc := p.State
			if p.CrashLoop {
				stateDesc = fmt.Sprintf("crashloop (%d)", p.RestartCount)
			} else if p.RestartCount > 0 && (p.State == "running" || p.State == "restarting") {
				stateDesc = fmt.Sprintf("%s (%d)", p.State, p.RestartCount)
			} else if p.State == "stopped" || p.State == "failed" {
				stateDesc = fmt.Sprintf("%s (exit %d)", p.State, p.ExitCode)
			}
			timeStr := "-"
			if !p.StartedAt.IsZero() {
				if !p.ExitedAt.IsZero() {
					timeStr = formatDuration(p.ExitedAt.Sub(p.StartedAt))
				} else {
					timeStr = formatDuration(time.Since(p.StartedAt))
				}
			}
			rows = append(rows, fmt.Sprintf("%-5d %-32s %-18s %-8s [%s]",
				p.ID, safe(p.Name), stateDesc, timeStr, strings.Join(p.Caps, ",")))
		}
		return ok(strings.Join(rows, "\n"))
	case "logs", "log":
		if len(args) == 0 || len(args) > 2 {
			return fail(fmt.Errorf("usage: logs PID [TAIL]"))
		}
		var pid int32
		if _, err := fmt.Sscan(args[0], &pid); err != nil || pid <= 0 {
			return fail(fmt.Errorf("invalid pid: %s", args[0]))
		}
		params := map[string]any{"id": pid}
		if len(args) == 2 {
			var tail int
			if _, err := fmt.Sscan(args[1], &tail); err == nil && tail > 0 {
				params["tail"] = tail
			}
		}
		var logs struct {
			ID        int32     `json:"id"`
			Name      string    `json:"name"`
			Kind      string    `json:"kind"`
			State     string    `json:"state"`
			ExitCode  int       `json:"exit_code"`
			StartedAt time.Time `json:"started_at"`
			ExitedAt  time.Time `json:"exited_at"`
			Duration  string    `json:"duration"`
			Stdout    struct {
				TotalBytes    int64  `json:"total_bytes"`
				BufferedBytes int    `json:"buffered_bytes"`
				DroppedBytes  int64  `json:"dropped_bytes"`
				Truncated     bool   `json:"truncated"`
				Content       string `json:"content"`
			} `json:"stdout"`
			Stderr struct {
				TotalBytes    int64  `json:"total_bytes"`
				BufferedBytes int    `json:"buffered_bytes"`
				DroppedBytes  int64  `json:"dropped_bytes"`
				Truncated     bool   `json:"truncated"`
				Content       string `json:"content"`
			} `json:"stderr"`
		}
		if err := c.Call(ctx, "proc/logs", params, &logs); err != nil {
			return fail(err)
		}
		exitStr := "-"
		if logs.State == "stopped" || logs.State == "failed" {
			exitStr = fmt.Sprintf("%d", logs.ExitCode)
		}
		durStr := logs.Duration
		if durStr == "" {
			durStr = "-"
		}
		var out []string
		out = append(out, fmt.Sprintf("PROCESS %d (%s) · %s · %s (exit %s, time %s)",
			logs.ID, safe(logs.Name), logs.Kind, logs.State, exitStr, durStr))
		out = append(out, fmt.Sprintf("stdout: %dB (dropped %dB) · stderr: %dB (dropped %dB)",
			logs.Stdout.TotalBytes, logs.Stdout.DroppedBytes, logs.Stderr.TotalBytes, logs.Stderr.DroppedBytes))
		if logs.Stdout.Content != "" {
			out = append(out, "--- STDOUT ---")
			out = append(out, safe(strings.TrimRight(logs.Stdout.Content, "\r\n")))
		}
		if logs.Stderr.Content != "" {
			out = append(out, "--- STDERR ---")
			out = append(out, safe(strings.TrimRight(logs.Stderr.Content, "\r\n")))
		}
		if logs.Stdout.Content == "" && logs.Stderr.Content == "" {
			out = append(out, "(no output recorded)")
		}
		return ok(strings.Join(out, "\n"))
	case "tasks", "taskmanager", "top":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: %s", cmd))
		}
		return ok("__SWITCH_VIEW__:tasks")
	case "notifications", "alerts":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: %s", cmd))
		}
		return ok("__SWITCH_VIEW__:notifications")
	case "settings", "preferences", "pref":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: %s", cmd))
		}
		return ok("__SWITCH_VIEW__:settings")
	case "reap":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: reap"))
		}
		var resp struct {
			Reaped int `json:"reaped"`
		}
		if err := c.Call(ctx, "proc/reap", map[string]any{}, &resp); err != nil {
			return fail(err)
		}
		return ok(fmt.Sprintf("Reaped %d terminated processes", resp.Reaped))
	case "receipt", "receipts":
		if len(args) > 1 {
			return fail(fmt.Errorf("usage: receipt [PID]"))
		}
		if len(args) == 0 {
			return ok("__LIST_RECEIPTS__")
		}
		var pid int32
		if _, err := fmt.Sscan(args[0], &pid); err != nil || pid <= 0 {
			return fail(fmt.Errorf("invalid pid: %s", args[0]))
		}
		return ok(fmt.Sprintf("__SHOW_RECEIPT__:%d", pid))
	case "dnd":
		if len(args) > 1 {
			return fail(fmt.Errorf("usage: dnd [on|off]"))
		}
		if len(args) == 0 {
			return ok("__TOGGLE_DND__")
		}
		switch strings.ToLower(args[0]) {
		case "on", "enable", "true", "1":
			return ok("__SET_DND_ON__")
		case "off", "disable", "false", "0":
			return ok("__SET_DND_OFF__")
		default:
			return fail(fmt.Errorf("usage: dnd [on|off]"))
		}
	case "theme":
		if len(args) == 0 {
			return ok("__SHOW_THEME__")
		}
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: theme [NAME]"))
		}
		return ok("__SET_THEME__:" + strings.ToLower(args[0]))
	case "motion":
		if len(args) == 0 {
			return ok("__TOGGLE_MOTION__")
		}
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: motion [on|off|reduce|normal]"))
		}
		arg := strings.ToLower(args[0])
		if arg == "on" || arg == "reduce" || arg == "reduced" {
			return ok("__SET_MOTION__:on")
		} else if arg == "off" || arg == "normal" || arg == "standard" {
			return ok("__SET_MOTION__:off")
		}
		return fail(fmt.Errorf("usage: motion [on|off|reduce|normal]"))
	case "status":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: status"))
		}
		var out json.RawMessage
		if err := c.Call(ctx, "sys/status", nil, &out); err != nil {
			return fail(err)
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fail(err)
		}
		return ok(safe(string(b)))
	case "shutdown":
		if len(args) != 0 {
			return fail(fmt.Errorf("usage: shutdown"))
		}
		if err := c.Call(ctx, "sys/shutdown", map[string]string{"reason": "requested by Charm shell"}, nil); err != nil {
			return fail(err)
		}
		return "Shutdown requested", cwd, true, nil
	default:
		return fail(fmt.Errorf("unknown command %q — type help", cmd))
	}
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
