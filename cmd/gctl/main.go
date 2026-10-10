// Command gctl controls a running Gostalgia environment over IPC. It is
// the operator's window into the environment and the proof that the IPC
// transport works end to end from outside the process.
//
//	gctl [--root DIR] <command> [args]
//
// Commands: status, ps, apps, echo MSG, ls PATH, cat PATH, shutdown,
// call METHOD [JSON-PARAMS]
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"gostalgia/internal/ipc"
	"gostalgia/internal/pkg"
	"gostalgia/internal/recovery"
	"gostalgia/internal/runtime"
	"gostalgia/platform"
)

const usageText = `gctl — control a running Gostalgia environment

Usage:
  gctl [--root DIR] status              runtime status
  gctl [--root DIR] ps                  environment processes
  gctl [--root DIR] history             bounded process exit history
  gctl [--root DIR] reap                reap inactive process objects
  gctl [--root DIR] logs PID [TAIL]     view child process logs and diagnostics
  gctl [--root DIR] apps                installed applications
  gctl [--root DIR] pkg SUBCOMMAND      inspect/install/update/rollback packages
  gctl [--root DIR] backup SUBCOMMAND   export/inspect/preview/restore portable backups
  gctl [--root DIR] echo MESSAGE        send a message to the echo app
  gctl [--root DIR] ls [PATH]           list a directory in the VFS
  gctl [--root DIR] cat PATH            print a file from the VFS
  gctl [--root DIR] shutdown            request a clean shutdown
  gctl [--root DIR] call METHOD [JSON]  raw IPC call (debug)

Packages (archive paths are VFS paths):
  pkg list
  pkg inspect APP-ID | pkg inspect --archive VFS-PATH
  pkg install VFS-PATH [--confirm-permissions]
  pkg update VFS-PATH [--confirm-permissions]
  pkg rollback APP-ID [--confirm-permissions]
  pkg uninstall APP-ID

Backups (archive paths are VFS paths):
  backup export [PATH] [--profile ID] [--no-system] [--description DESC]
  backup inspect PATH
  backup preview PATH
  backup restore PATH [--strategy abort|overwrite|skip] [--profile ID]
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Print(usageText)
		return
	}

	fs := flag.NewFlagSet("gctl", flag.ExitOnError)
	root := fs.String("root", "", "environment root directory (default $GOSTALGIA_ROOT or ~/.gostalgia)")
	fs.Parse(args)

	cmd := "status"
	rest := fs.Args()
	if len(rest) > 0 {
		cmd = rest[0]
		rest = rest[1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := connect(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gctl:", err)
		os.Exit(1)
	}
	defer client.Close()

	if err := run(ctx, client, cmd, rest); err != nil {
		fmt.Fprintln(os.Stderr, "gctl:", err)
		os.Exit(1)
	}
}

// connect locates a running environment and authenticates.
func connect(root string) (*ipc.Client, error) {
	dir, err := runtime.ResolveRoot(root)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(dir + "/runtime.json")
	if err != nil {
		return nil, fmt.Errorf("no running environment found at %s (is gostalgia booted?)", dir)
	}
	var info struct {
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(data, &info); err != nil || info.Endpoint == "" {
		return nil, fmt.Errorf("corrupt runtime.json in %s", dir)
	}
	conn, err := platform.DialIPC(info.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("environment at %s is not reachable: %w", info.Endpoint, err)
	}
	return ipc.NewClient(conn, info.Token)
}

func run(ctx context.Context, client *ipc.Client, cmd string, args []string) error {
	switch cmd {
	case "pkg", "package":
		method, params, err := pkg.ParseCommand(args)
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := client.Call(ctx, method, params, &raw); err != nil {
			return err
		}
		return pretty(raw)
	case "backup", "recovery":
		method, params, err := recovery.ParseCommand(args)
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := client.Call(ctx, method, params, &raw); err != nil {
			return err
		}
		return pretty(raw)
	case "status":
		var raw json.RawMessage
		if err := client.Call(ctx, "sys/status", nil, &raw); err != nil {
			return err
		}
		return pretty(raw)

	case "ps":
		var procs []struct {
			ID           int32     `json:"id"`
			Name         string    `json:"name"`
			Kind         string    `json:"kind"`
			State        string    `json:"state"`
			StartedAt    time.Time `json:"started_at"`
			ExitedAt     time.Time `json:"exited_at"`
			ExitCode     int       `json:"exit_code"`
			RestartCount int       `json:"restart_count"`
			CrashLoop    bool      `json:"crash_loop"`
		}
		if err := client.Call(ctx, "proc/list", nil, &procs); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PID\tNAME\tKIND\tSTATE\tRESTARTS\tEXIT\tTIME")
		for _, p := range procs {
			exitStr := "-"
			if p.State == "stopped" || p.State == "failed" {
				exitStr = fmt.Sprintf("%d", p.ExitCode)
			}
			timeStr := "-"
			if !p.StartedAt.IsZero() {
				if !p.ExitedAt.IsZero() {
					timeStr = formatDuration(p.ExitedAt.Sub(p.StartedAt))
				} else {
					timeStr = formatDuration(time.Since(p.StartedAt))
				}
			}
			restartsStr := fmt.Sprintf("%d", p.RestartCount)
			if p.CrashLoop {
				restartsStr += " (crashloop)"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, sanitizeTerminal(p.Name), p.Kind, p.State, restartsStr, exitStr, timeStr)
		}
		return w.Flush()

	case "history":
		var history []struct {
			ID           int32  `json:"id"`
			Name         string `json:"name"`
			Kind         string `json:"kind"`
			State        string `json:"state"`
			ExitCode     int    `json:"exit_code"`
			Duration     string `json:"duration"`
			RestartCount int    `json:"restart_count"`
			CrashLoop    bool   `json:"crash_loop"`
			Err          string `json:"error"`
		}
		if err := client.Call(ctx, "proc/history", nil, &history); err != nil {
			return err
		}
		if len(history) == 0 {
			fmt.Println("No process history recorded.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PID\tNAME\tKIND\tSTATE\tEXIT\tRESTARTS\tDURATION\tERROR")
		for _, h := range history {
			restartsStr := fmt.Sprintf("%d", h.RestartCount)
			if h.CrashLoop {
				restartsStr += " (crashloop)"
			}
			errStr := sanitizeTerminal(h.Err)
			if errStr == "" {
				errStr = "-"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				h.ID, sanitizeTerminal(h.Name), h.Kind, h.State, h.ExitCode, restartsStr, h.Duration, errStr)
		}
		return w.Flush()

	case "reap":
		var res struct {
			Reaped int `json:"reaped"`
		}
		if err := client.Call(ctx, "proc/reap", nil, &res); err != nil {
			return err
		}
		fmt.Printf("Reaped %d inactive processes.\n", res.Reaped)
		return nil

	case "logs":
		if len(args) == 0 {
			return fmt.Errorf("usage: gctl logs PID [TAIL]")
		}
		var pid int32
		if _, err := fmt.Sscan(args[0], &pid); err != nil || pid <= 0 {
			return fmt.Errorf("invalid pid: %s", args[0])
		}
		params := map[string]any{"id": pid}
		if len(args) > 1 {
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
		if err := client.Call(ctx, "proc/logs", params, &logs); err != nil {
			return err
		}
		exitStr := "-"
		if logs.State == "stopped" || logs.State == "failed" {
			exitStr = fmt.Sprintf("%d", logs.ExitCode)
		}
		durStr := logs.Duration
		if durStr == "" {
			durStr = "-"
		}
		fmt.Printf("Process %d (%s) [%s] — %s (exit: %s, duration: %s)\n",
			logs.ID, sanitizeTerminal(logs.Name), logs.Kind, logs.State, exitStr, durStr)
		fmt.Printf("Stdout: %d bytes (buffered: %d, dropped: %d, truncated: %v)\n",
			logs.Stdout.TotalBytes, logs.Stdout.BufferedBytes, logs.Stdout.DroppedBytes, logs.Stdout.Truncated)
		fmt.Printf("Stderr: %d bytes (buffered: %d, dropped: %d, truncated: %v)\n",
			logs.Stderr.TotalBytes, logs.Stderr.BufferedBytes, logs.Stderr.DroppedBytes, logs.Stderr.Truncated)

		if logs.Stdout.Content != "" {
			fmt.Println("--- stdout ---")
			fmt.Print(sanitizeTerminalBlock(logs.Stdout.Content))
			if !strings.HasSuffix(logs.Stdout.Content, "\n") {
				fmt.Println()
			}
		}
		if logs.Stderr.Content != "" {
			fmt.Println("--- stderr ---")
			fmt.Print(sanitizeTerminalBlock(logs.Stderr.Content))
			if !strings.HasSuffix(logs.Stderr.Content, "\n") {
				fmt.Println()
			}
		}
		if logs.Stdout.Content == "" && logs.Stderr.Content == "" {
			fmt.Println("(no output recorded)")
		}
		return nil

	case "apps":
		var apps []struct {
			Manifest struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"manifest"`
			Running bool  `json:"running"`
			PID     int32 `json:"pid"`
		}
		if err := client.Call(ctx, "app/list", nil, &apps); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tVERSION\tSTATE")
		for _, a := range apps {
			state := "stopped"
			if a.Running {
				state = fmt.Sprintf("running (pid %d)", a.PID)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sanitizeTerminal(a.Manifest.ID), sanitizeTerminal(a.Manifest.Name), sanitizeTerminal(a.Manifest.Version), state)
		}
		return w.Flush()

	case "echo":
		if len(args) == 0 {
			return fmt.Errorf("usage: gctl echo MESSAGE")
		}
		var out struct {
			Msg    string `json:"msg"`
			Echoes int64  `json:"echoes"`
		}
		params := map[string]string{"msg": join(args)}
		if err := client.Call(ctx, "app/com.gostalgia.echo/echo", params, &out); err != nil {
			return err
		}
		fmt.Printf("%s (echo #%d)\n", sanitizeTerminal(out.Msg), out.Echoes)
		return nil

	case "ls":
		path := "/"
		if len(args) > 0 {
			path = args[0]
		}
		var out struct {
			Path    string `json:"path"`
			Entries []struct {
				Name  string `json:"name"`
				IsDir bool   `json:"is_dir"`
				Size  int64  `json:"size"`
			} `json:"entries"`
		}
		if err := client.Call(ctx, "fs/list", map[string]string{"path": path}, &out); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "%s:\n", sanitizeTerminal(out.Path))
		for _, e := range out.Entries {
			if e.IsDir {
				fmt.Fprintf(w, "  %s/\n", sanitizeTerminal(e.Name))
				continue
			}
			fmt.Fprintf(w, "  %s\t(%d bytes)\n", sanitizeTerminal(e.Name), e.Size)
		}
		return w.Flush()

	case "cat":
		if len(args) == 0 {
			return fmt.Errorf("usage: gctl cat PATH")
		}
		var out struct {
			Data string `json:"data_base64"`
		}
		if err := client.Call(ctx, "fs/read", map[string]string{"path": args[0]}, &out); err != nil {
			return err
		}
		data, err := base64.StdEncoding.DecodeString(out.Data)
		if err != nil {
			return err
		}
		os.Stdout.Write(data)
		return nil

	case "shutdown":
		var out struct {
			Reason string `json:"reason"`
		}
		if err := client.Call(ctx, "sys/shutdown", map[string]string{"reason": "requested by gctl"}, &out); err != nil {
			return err
		}
		fmt.Println("shutdown requested")
		return nil

	case "call":
		if len(args) == 0 {
			return fmt.Errorf("usage: gctl call METHOD [JSON-PARAMS]")
		}
		var params any
		if len(args) > 1 {
			if err := json.Unmarshal([]byte(args[1]), &params); err != nil {
				return fmt.Errorf("params must be JSON: %w", err)
			}
		}
		var raw json.RawMessage
		if err := client.Call(ctx, args[0], params, &raw); err != nil {
			return err
		}
		return pretty(raw)

	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usageText)
	}
}

func join(args []string) string {
	out := args[0]
	for _, a := range args[1:] {
		out += " " + a
	}
	return out
}

func pretty(raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		fmt.Println(string(raw))
		return nil
	}
	fmt.Println(buf.String())
	return nil
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\].*?(\x07|\x1b\\)`)

// sanitizeTerminal scrubs server-supplied strings rendered into single-line
// table fields. ANSI/OSC sequences and control characters are removed, and
// newlines, carriage returns, and tabs become visible escapes so a hostile
// process name or error string cannot forge rows or columns in the
// ps/history/logs tabwriter tables.
func sanitizeTerminal(s string) string {
	s = ansiRegex.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 32 || r == 127 || unicode.IsControl(r) {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeTerminalBlock is sanitizeTerminal for multi-line log content: the
// same sequence stripping, but real newlines and tabs survive so captured
// process output keeps its shape. It never feeds tabwriter fields.
func sanitizeTerminalBlock(s string) string {
	s = ansiRegex.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 32 || r == 127 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
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
