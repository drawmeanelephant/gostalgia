package shell

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"getsteep.dev/steep"
	"getsteep.dev/steep/reel"
	"getsteep.dev/steep/snapshot"
	xansi "github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/ipc"
	"gostalgia/internal/runtime"
	"gostalgia/internal/service"
	"gostalgia/sdk"
)

const (
	steepShellHelperEnv = "GOSTALGIA_STEEP_SHELL_HELPER"
	steepShellRootEnv   = "GOSTALGIA_STEEP_SHELL_ROOT"
	steepNotifierID     = "com.test.steepnotifier"
)

// TestSteepShellProcess is the exec-harness entry point. It boots the same
// real runtime and authenticated socket client as TestBubbleTeaSocketAppLifecycle,
// then uses the production Run path so Ctrl-C also exercises Run's restoration.
func TestSteepShellProcess(t *testing.T) {
	if os.Getenv(steepShellHelperEnv) != "1" {
		return
	}
	os.Exit(runSteepShellProcess(os.Getenv(steepShellRootEnv)))
}

type steepNotificationSink struct{ manager *notifications.Manager }

func (s steepNotificationSink) Record(r service.NotificationRecord) bool {
	return s.manager.Record(notifications.Notification{
		Title: r.Title, Message: r.Message, Level: notifications.Level(r.Level),
		Source: r.Source, PID: r.PID, Timestamp: r.Timestamp,
	})
}

func runSteepShellProcess(root string) int {
	notifs := notifications.NewManager(100, 3, 8)
	rt, client, err := bootShellTestRuntime(root, runtime.Options{Notifications: steepNotificationSink{notifs}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "steep shell setup: %v\n", err)
		return 1
	}
	defer rt.Shutdown("steep shell cleanup")
	defer client.Close()
	if err := rt.Apps.Registry().RegisterExternal(sdk.Manifest{
		ID:              steepNotifierID,
		Name:            "Steep Notifier",
		Version:         "1.0.0",
		Mode:            sdk.ModeExternal,
		Executable:      os.Args[0],
		Args:            []string{"-test.run=^TestSteepNotifyAppProcess$"},
		ProtocolVersion: sdk.ProtocolVersion,
		Permissions:     []string{sdk.CapIPC, sdk.CapNotify},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "steep notifier setup: %v\n", err)
		return 1
	}
	if err := Run(context.Background(), client, rt.Done(), notifs); err != nil {
		fmt.Fprintf(os.Stderr, "steep shell: %v\n", err)
		return 1
	}
	return 0
}

// TestSteepNotifyAppProcess is launched by the runtime as a real external SDK
// app. Its route gives the toast scenario a deliberate, authenticated
// notify/post boundary instead of injecting a notification into the shell.
func TestSteepNotifyAppProcess(t *testing.T) {
	if os.Getenv("GOSTALGIA_APP_ID") != steepNotifierID {
		return
	}
	if err := sdk.Serve(&steepNotifyApp{}); err != nil {
		fmt.Fprintf(os.Stderr, "steep notifier: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type steepNotifyApp struct{}

func (a *steepNotifyApp) Init(app *sdk.Context) error {
	return app.Handle("post", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := app.Call(ctx, "notify/post", map[string]string{
			"severity": "info",
			"title":    "STEEP TOAST",
			"body":     "real application notification",
		}, nil); err != nil {
			return nil, err
		}
		return map[string]bool{"posted": true}, nil
	})
}

func (a *steepNotifyApp) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (a *steepNotifyApp) Stop(context.Context) error { return nil }

func newSteepRuntime(t *testing.T, opts ...steep.Option) (*steep.Harness, *ipc.Client) {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSteepShellProcess$")
	cmd.Env = append(os.Environ(), steepShellHelperEnv+"=1", steepShellRootEnv+"="+root)
	base := []steep.Option{
		steep.WithWindowSize(80, 24),
		steep.WithTimeout(30 * time.Second),
		steep.WithColor(steep.ColorANSI256),
	}
	h := steep.NewExecHarness(t, cmd, append(base, opts...)...)
	// Initial discovery is complete only after sys/status has populated the
	// real service count. The first Home paint alone is not a readiness barrier.
	h.Wait("Services: 15")
	client, err := dialShellTestRuntime(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return h, client
}

var steepPIDPattern = regexp.MustCompile(`PID [0-9]+`)

func steepSnapshot(h *steep.Harness, name string) {
	// Finite entrances settle within their ≤200 ms budget; a reviewed static
	// golden must never capture a partial reveal.
	h.WaitSettle()
	h.RequireSnapshot(
		snapshot.WithSuffix(name),
		snapshot.WithANSI(false),
		snapshot.WithTransform(normalizeSteepSnapshot),
	)
}

func normalizeSteepSnapshot(in []byte) []byte {
	lines := bytes.Split(in, []byte("\n"))
	for i, line := range lines {
		if bytes.Contains(line, []byte("Uptime:")) {
			lines[i] = normalizeSteepUptime(line)
			continue
		}
		lines[i] = steepPIDPattern.ReplaceAllFunc(line, func(match []byte) []byte {
			return append([]byte("PID "), bytes.Repeat([]byte("N"), len(match)-len("PID "))...)
		})
	}
	return bytes.Join(lines, []byte("\n"))
}

func normalizeSteepUptime(line []byte) []byte {
	start := bytes.Index(line, []byte("Uptime:"))
	end := bytes.Index(line, []byte(" · Processes:"))
	if start < 0 || end < start {
		return line
	}
	right := string(line[end:])
	if !strings.HasSuffix(right, "┃") {
		return line
	}
	right = strings.TrimRight(strings.TrimSuffix(right, "┃"), " ")
	base := string(line[:start]) + "Uptime: <t>" + right
	padding := xansi.StringWidth(string(line)) - xansi.StringWidth(base) - 1
	if padding < 0 {
		return line
	}
	return []byte(base + strings.Repeat(" ", padding) + "┃")
}

func TestSteepSnapshotNormalization(t *testing.T) {
	uptimeLine := func(value string) []byte {
		base := "┃ Uptime: " + value + " · Processes: 1 · Services: 15"
		return []byte(base + strings.Repeat(" ", 90-xansi.StringWidth(base)-1) + "┃")
	}
	rawShort := append(uptimeLine("0s"), []byte("\n┃ PID 1 ┃")...)
	rawLong := append(uptimeLine("12m 4s"), []byte("\n┃ PID 234 ┃")...)
	short := normalizeSteepSnapshot(rawShort)
	long := normalizeSteepSnapshot(rawLong)
	shortLines := bytes.Split(short, []byte("\n"))
	longLines := bytes.Split(long, []byte("\n"))
	if !bytes.Equal(shortLines[0], longLines[0]) {
		t.Fatalf("uptime normalization differs:\n%s\n%s", shortLines[0], longLines[0])
	}
	for _, tc := range []struct {
		name       string
		normalized []byte
		original   []byte
	}{{"short", short, rawShort}, {"long", long, rawLong}} {
		normalizedLines := bytes.Split(tc.normalized, []byte("\n"))
		originalLines := bytes.Split(tc.original, []byte("\n"))
		for i := range normalizedLines {
			if got, want := xansi.StringWidth(string(normalizedLines[i])), xansi.StringWidth(string(originalLines[i])); got != want {
				t.Errorf("%s normalized line %d width = %d, want %d", tc.name, i, got, want)
			}
		}
	}
	if got, want := string(shortLines[1]), "┃ PID N ┃"; got != want {
		t.Errorf("normalized PID = %q, want %q", got, want)
	}
	if got, want := string(longLines[1]), "┃ PID NNN ┃"; got != want {
		t.Errorf("normalized multi-digit PID = %q, want %q", got, want)
	}
}

func steepAppRunning(t *testing.T, client *ipc.Client, id string, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var apps []appStatus
	if err := client.Call(ctx, "app/list", nil, &apps); err != nil {
		t.Fatalf("app/list: %v", err)
	}
	for _, app := range apps {
		if app.Manifest.ID == id {
			if app.Running != want {
				t.Fatalf("%s running = %v, want %v", id, app.Running, want)
			}
			return
		}
	}
	t.Fatalf("%s missing from real runtime app list", id)
}

func steepEchoCount(t *testing.T, client *ipc.Client, want int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out struct {
		Echoes int64 `json:"echoes"`
	}
	if err := client.Call(ctx, "app/com.gostalgia.echo/stats", nil, &out); err != nil {
		t.Fatalf("echo stats: %v", err)
	}
	if out.Echoes != want {
		t.Fatalf("real echo count = %d, want %d", out.Echoes, want)
	}
}

func steepLaunchNotifier(t *testing.T, h *steep.Harness) {
	t.Helper()
	h.Type("launch " + steepNotifierID).Key("enter").Wait("Launched " + steepNotifierID)
}

func steepStopSelectedEcho(t *testing.T, h *steep.Harness, client *ipc.Client) {
	t.Helper()
	h.Key("f3").Wait([]string{"Stop Echo?", "Unsaved work may be lost."})
	h.Key("tab").Key("enter").Wait("Enter launches Echo")
	steepAppRunning(t, client, "com.gostalgia.echo", false)
}

func TestSteepRuntimeScenarios(t *testing.T) {
	t.Run("home", func(t *testing.T) {
		h, _ := newSteepRuntime(t)
		// The boot golden pins Echo's row and the Apps badge, which arrive with
		// a later process poll than the headings on a loaded runner.
		h.Wait([]string{"SYSTEM ACTIVITY", "RUNNING APPLICATIONS", "DOCUMENT SHORTCUTS", "Apps (1)", "LIVE / PID"})
		steepSnapshot(h, "boot")
	})

	t.Run("launcher_launch_stop", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Key("f2").Wait("SEARCH: type to filter apps...")
		h.Type("echo").Wait("com.gostalgia.echo").Wait("LIVE / PID")
		h.Key("f3").Wait([]string{"Stop Echo?", "Unsaved work may be lost."})
		steepSnapshot(h, "stop-confirmation")
		// Cancel has focus by default; Tab selects the destructive action.
		h.Key("tab").Key("enter").Wait("Enter launches Echo")
		steepAppRunning(t, client, "com.gostalgia.echo", false)
		// Enter on the stopped app launches it and opens its window.
		h.Key("enter").Wait([]string{"Windows (1)", "Echo", "Ctrl-W CLOSE"})
		steepAppRunning(t, client, "com.gostalgia.echo", true)
		// Esc returns to the Apps shelf with Echo marked LIVE; Enter focuses
		// its window again without attempting a single-instance relaunch.
		h.KeyEsc().Wait([]string{"SEARCH: echo", "LIVE / PID"})
		h.Key("enter").Wait([]string{"Windows (1)", "Echo", "Ctrl-W CLOSE"})
		h.AssertNot("LAUNCH FAILED")
		h.KeyEsc().Wait([]string{"SEARCH: echo", "LIVE / PID"})
		steepStopSelectedEcho(t, h, client)
	})

	t.Run("launcher_dogcalc_launch_and_focus", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Key("f2").Wait("SEARCH: type to filter apps...")
		h.Type("dog").Wait("com.gostalgia.dogcalc")
		// "dog" matches Calculator's description first; Down selects Dogcalc.
		h.Key("down").Wait("Enter launches Dogcalc")
		steepAppRunning(t, client, "com.gostalgia.dogcalc", false)
		// First Enter launches Dogcalc and makes its window visible.
		h.Key("enter").Wait([]string{"Windows (1)", "Dogcalc", "BOOP ="})
		steepAppRunning(t, client, "com.gostalgia.dogcalc", true)
		// Esc returns to the filtered Apps shelf where Dogcalc is now LIVE.
		h.KeyEsc().Wait([]string{"SEARCH: dog", "Dogcalc is LIVE", "Enter OPEN"})
		// Second Enter focuses Dogcalc's window without an invalid relaunch.
		h.Key("enter").Wait([]string{"Windows (1)", "Dogcalc", "BOOP ="})
		h.AssertNot("already running or initializing")
		h.AssertNot("LAUNCH FAILED")
		// Explicit F3 stop from the shelf remains preserved.
		h.KeyEsc().Wait([]string{"SEARCH: dog", "Dogcalc is LIVE"})
		h.Key("f3").Wait([]string{"Stop Dogcalc?", "Unsaved work may be lost."})
		h.Key("tab").Key("enter").Wait("Enter launches Dogcalc")
		steepAppRunning(t, client, "com.gostalgia.dogcalc", false)
	})

	t.Run("launcher_external_lifecycle_refresh", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Key("f2").Wait("SEARCH: type to filter apps...")
		h.Type("echo").Wait("LIVE / PID")
		steepAppRunning(t, client, "com.gostalgia.echo", true)

		// Another authenticated client stops Echo while the shelf is open.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := client.Call(ctx, "app/stop", map[string]string{"id": "com.gostalgia.echo"}, nil); err != nil {
			t.Fatalf("external app/stop: %v", err)
		}
		steepAppRunning(t, client, "com.gostalgia.echo", false)

		// No prompt command runs in between: clearing the filter, leaving
		// and reopening the shelf must read the registry again.
		h.KeyEsc().KeyEsc()
		h.Key("f2").Wait("SEARCH: type to filter apps...")
		h.Type("echo").Wait("Enter launches Echo").WaitNot("LIVE / PID")

		var launched struct {
			ID  string `json:"id"`
			PID int32  `json:"pid"`
		}
		ctx2, cancel2 := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel2()
		if err := client.Call(ctx2, "app/launch", map[string]string{"id": "com.gostalgia.echo"}, &launched); err != nil {
			t.Fatalf("external app/launch: %v", err)
		}
		if launched.PID == 0 {
			t.Fatalf("external app/launch returned no PID: %+v", launched)
		}
		steepAppRunning(t, client, "com.gostalgia.echo", true)

		h.KeyEsc().KeyEsc()
		h.Key("f2").Wait("SEARCH: type to filter apps...")
		h.Type("echo").Wait(fmt.Sprintf("LIVE / PID %d", launched.PID))
	})

	t.Run("home_external_lifecycle_badge", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		// Stay on Home for the whole flow: no Apps visit and no shell
		// command runs. Echo is the boot app, so its badge is already up.
		h.Wait([]string{"SYSTEM ACTIVITY", "Apps (1)"})

		// Another authenticated client stops Echo. The existing 500 ms
		// process poll must update Home's badge and app list.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := client.Call(ctx, "app/stop", map[string]string{"id": "com.gostalgia.echo"}, nil); err != nil {
			t.Fatalf("external app/stop: %v", err)
		}
		steepAppRunning(t, client, "com.gostalgia.echo", false)
		h.WaitNot("Apps (1)")
		h.Wait("No applications running")

		// A later external launch appears on the same poll path with its
		// live PID, still without leaving Home.
		var launched struct {
			ID  string `json:"id"`
			PID int32  `json:"pid"`
		}
		ctx2, cancel2 := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel2()
		if err := client.Call(ctx2, "app/launch", map[string]string{"id": "com.gostalgia.echo"}, &launched); err != nil {
			t.Fatalf("external app/launch: %v", err)
		}
		if launched.PID == 0 {
			t.Fatalf("external app/launch returned no PID: %+v", launched)
		}
		steepAppRunning(t, client, "com.gostalgia.echo", true)
		h.Wait("Apps (1)")
		h.Wait(fmt.Sprintf("LIVE / PID %d", launched.PID))
	})

	t.Run("apps_command_opens_shelf", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Type("apps").Key("enter").Wait("SEARCH: type to filter apps...")
		h.Type("echo").Wait("LIVE / PID")
		steepAppRunning(t, client, "com.gostalgia.echo", true)
	})

	t.Run("palette", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Key("ctrl+p").Wait("COMMAND PALETTE")
		h.Type("steepnotifier").Wait([]string{"> steepnotifier", "Launch Steep Notifier"})
		steepSnapshot(h, "filtered")
		h.KeyEsc().WaitNot("COMMAND PALETTE").Wait("SYSTEM ACTIVITY")
		// Dismissing the overlay did not activate its covered or selected action.
		steepAppRunning(t, client, steepNotifierID, false)
	})

	t.Run("echo", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Type("echo steep-runtime").Key("enter").Wait("[echo #1]")
		steepEchoCount(t, client, 1)
		steepSnapshot(h, "result")
	})

	t.Run("resize_below_minimum_back", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Resize(29, 10).Wait("Resize to 30×10")
		steepSnapshot(h, "below-minimum")
		h.Resize(80, 24).Wait("SYSTEM ACTIVITY")
		h.Type("echo resize-recovered").Key("enter").Wait("[echo #1]")
		steepEchoCount(t, client, 1)
		steepSnapshot(h, "recovered")
	})

	t.Run("bracketed_paste", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		h.Type("echo ").Paste("first\nsecond").Wait("echo first second")
		h.AssertNot("[echo #1]")
		steepSnapshot(h, "draft")
		h.Key("enter").Wait("[echo #1]").Wait("first second")
		steepEchoCount(t, client, 1)
		steepSnapshot(h, "submitted")
	})

	t.Run("mouse", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		// Filter the external app with the keyboard, then navigate and launch
		// through actual decoded mouse reports. An app that was not running
		// keeps unrelated process-exit notifications out of this static frame;
		// launcher_launch_stop owns Echo stop/relaunch coverage.
		h.Key("f2").Wait("SEARCH: type to filter apps...")
		h.Type("steepnotifier").Wait("com.test.steepnotifier")
		steepAppRunning(t, client, steepNotifierID, false)
		h.Locate(reel.Contains("Home")).LeftClick().Wait("SYSTEM ACTIVITY")
		h.Locate(reel.Contains("Apps")).LeftClick().Wait("SEARCH: steepnotifier")
		h.Locate(reel.Contains("Steep Notifier")).LeftClick(reel.Repeat(2)).Wait("LIVE / PID")
		steepAppRunning(t, client, steepNotifierID, true)
		steepSnapshot(h, "launched")
	})

	t.Run("toast_stable", func(t *testing.T) {
		h, client := newSteepRuntime(t)
		steepLaunchNotifier(t, h)
		for i := 1; i <= 12; i++ {
			h.Type(fmt.Sprintf("echo TOAST-LINE-%02d", i)).Key("enter").Wait(fmt.Sprintf("[echo #%d]", i))
		}
		h.Type("echo TOAST-ANCHOR").Key("enter").Wait("[echo #13]")
		steepEchoCount(t, client, 13)
		h.RequireNot("STEEP TOAST")
		beforeTranscript := steepToastTranscriptLines(t, h)
		beforeAnchor := h.Locate(reel.Contains("TOAST-ANCHOR   [echo #13]"))
		beforeHelp := h.Locate(reel.Contains("Ctrl-C EXIT"))
		beforeCursor := h.CursorPosition()
		steepSnapshot(h, "before")

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var posted struct {
			Posted bool `json:"posted"`
		}
		if err := client.Call(ctx, "app/"+steepNotifierID+"/post", nil, &posted); err != nil {
			t.Fatalf("real application notify/post: %v", err)
		}
		if !posted.Posted {
			t.Fatal("real application notification was not posted")
		}
		h.Wait([]string{"STEEP TOAST", "real application"})
		steepSnapshot(h, "visible")
		assertSteepScreenAnchor(t, h, beforeAnchor, beforeHelp, beforeCursor)
		assertSteepToastTranscript(t, h, beforeTranscript)

		h.WaitNot("STEEP TOAST")
		steepSnapshot(h, "expired")
		assertSteepScreenAnchor(t, h, beforeAnchor, beforeHelp, beforeCursor)
		assertSteepToastTranscript(t, h, beforeTranscript)
	})

	t.Run("ctrl_c_protocol_restore", func(t *testing.T) {
		castPath := filepath.Join(t.TempDir(), "ctrl-c.cast")
		h, _ := newSteepRuntime(t,
			steep.WithRecording(reel.Profile{Outputs: reel.MustFiles(castPath)}),
			steep.WithExportAlways(),
		)
		if !h.IsAltScreen() {
			t.Fatal("shell did not enter the alternate screen")
		}
		h.KeyCtrlC()

		if status := h.ExitStatus(); status != 0 {
			t.Fatalf("Ctrl-C exit status = %d, want 0", status)
		}
		if h.IsAltScreen() {
			t.Error("Ctrl-C left the alternate screen active")
		}
		if !h.CursorVisible() {
			t.Error("Ctrl-C left the cursor hidden")
		}
		for _, mode := range []xansi.Mode{
			xansi.ModeMouseNormal, xansi.ModeMouseButtonEvent, xansi.ModeMouseAnyEvent,
			xansi.ModeMouseExtSgr, xansi.ModeBracketedPaste,
		} {
			if got := h.ModeSetting(mode); got != xansi.ModeReset {
				t.Errorf("terminal mode %v = %v after Ctrl-C, want reset", mode, got)
			}
		}
		timeline := h.Timeline()
		h.Close()
		assertSteepRestorationOutput(t, timeline)
	})
}

type steepToastTranscriptLine struct {
	text  string
	point image.Point
}

func steepToastTranscriptLines(t *testing.T, h *steep.Harness) []steepToastTranscriptLine {
	t.Helper()
	var visible []steepToastTranscriptLine
	for y, line := range strings.Split(h.View(), "\n") {
		for i := 1; i <= 12; i++ {
			marker := fmt.Sprintf("TOAST-LINE-%02d   [echo #%d]", i, i)
			if strings.Contains(line, marker) {
				visible = append(visible, steepToastTranscriptLine{text: marker, point: image.Pt(strings.Index(line, marker), y)})
			}
		}
		if marker := "TOAST-ANCHOR   [echo #13]"; strings.Contains(line, marker) {
			visible = append(visible, steepToastTranscriptLine{text: marker, point: image.Pt(strings.Index(line, marker), y)})
		}
	}
	if len(visible) < 4 {
		t.Fatalf("toast transcript is not scrollable: only %d numbered result lines visible\n%s", len(visible), h.View())
	}
	if !strings.HasPrefix(visible[len(visible)-1].text, "TOAST-ANCHOR") {
		t.Fatalf("bottom toast anchor is not visible: %+v", visible)
	}
	return visible
}

func assertSteepToastTranscript(t *testing.T, h *steep.Harness, want []steepToastTranscriptLine) {
	t.Helper()
	got := steepToastTranscriptLines(t, h)
	if len(got) != len(want) {
		t.Fatalf("visible transcript line count changed from %d to %d\nbefore: %+v\nafter: %+v", len(want), len(got), want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("visible transcript line %d moved/changed from %+v to %+v", i, want[i], got[i])
		}
	}
}

func assertSteepScreenAnchor(t *testing.T, h *steep.Harness, anchor, help steep.Location, cursor image.Point) {
	t.Helper()
	if got := h.Locate(reel.Contains("TOAST-ANCHOR   [echo #13]")); got.X != anchor.X || got.Y != anchor.Y {
		t.Errorf("transcript anchor moved from (%d,%d) to (%d,%d)", anchor.X, anchor.Y, got.X, got.Y)
	}
	if got := h.Locate(reel.Contains("Ctrl-C EXIT")); got.X != help.X || got.Y != help.Y {
		t.Errorf("help moved from (%d,%d) to (%d,%d)", help.X, help.Y, got.X, got.Y)
	}
	if got := h.CursorPosition(); got != cursor {
		t.Errorf("prompt cursor moved from %v to %v", cursor, got)
	}
}

func assertSteepRestorationOutput(t *testing.T, timeline *reel.Timeline) {
	t.Helper()
	var all, raw []byte
	var inputEncodings []string
	ctrlCSeen := false
	for event := range timeline.Events() {
		switch event := event.(type) {
		case reel.Input:
			inputEncodings = append(inputEncodings, fmt.Sprintf("%q", event.Data))
			if bytes.Equal(event.Data, []byte{0x03}) || bytes.Equal(event.Data, []byte("\x1b[99;5u")) {
				ctrlCSeen = true
				raw = nil
			}
		case reel.Output:
			all = append(all, event.Data...)
			if ctrlCSeen {
				raw = append(raw, event.Data...)
			}
		}
	}
	if !ctrlCSeen {
		t.Fatalf("Ctrl-C recording contains no literal or Kitty Ctrl-C input event; inputs: %v", inputEncodings)
	}
	if len(raw) == 0 {
		t.Fatal("Ctrl-C recording contains no terminal output after Ctrl-C")
	}
	expected := map[string]string{
		"alternate screen reset": "\x1b[?1049l",
		"cursor visible":         "\x1b[?25h",
		"style reset":            "\x1b[0m",
		"mouse 1000 reset":       "\x1b[?1000l",
		"mouse 1002 reset":       "\x1b[?1002l",
		"mouse 1003 reset":       "\x1b[?1003l",
		"mouse SGR reset":        "\x1b[?1006l",
		"bracketed paste reset":  "\x1b[?2004l",
		"kitty keyboard reset":   "\x1b[<1u",
		"modifyOtherKeys reset":  "\x1b[>4m",
	}
	if goruntime.GOOS == "windows" {
		// ConPTY filters unsupported normal-mouse mode 1000 in both
		// directions. The scenario's ModeSetting assertions still prove that
		// its emulator state was restored; every supported reset is required.
		delete(expected, "mouse 1000 reset")
		// ConPTY repaints instead of forwarding Run's ESC[0m, and writes its
		// own reset only while its pen still holds a style. Whether one
		// follows Ctrl-C depends on how ConPTY painted the last frame, so the
		// pen check below carries the requirement there.
		delete(expected, "style reset")
	}
	missing := false
	if last, ok := finalSGRResetsPen(all); !ok {
		missing = true
		t.Errorf("terminal output leaves the pen styled: last SGR is %q, want a reset", last)
	}
	for name, sequence := range expected {
		if !bytes.Contains(raw, []byte(sequence)) {
			missing = true
			t.Errorf("post-Ctrl-C output lacks %s (%q)", name, sequence)
		}
	}
	if missing {
		t.Errorf("post-Ctrl-C raw output was %q", raw)
	}
}

var sgrSequence = regexp.MustCompile(`\x1b\[([0-9;:]*)m`)

// finalSGRResetsPen reports whether the last SGR sequence in out, if any, is
// a bare reset such as ESC[m or ESC[0m, so the terminal is left unstyled.
func finalSGRResetsPen(out []byte) (last string, ok bool) {
	matches := sgrSequence.FindAllSubmatch(out, -1)
	if len(matches) == 0 {
		return "", true
	}
	final := matches[len(matches)-1]
	for _, param := range strings.FieldsFunc(string(final[1]), func(r rune) bool { return r == ';' || r == ':' }) {
		if strings.TrimLeft(param, "0") != "" {
			return string(final[0]), false
		}
	}
	return string(final[0]), true
}

func TestSteepNotesScopedSaveFlow(t *testing.T) {
	h, client := newSteepRuntime(t, steep.WithWindowSize(100, 32))

	const docPath = "/users/guest/documents/test-note.txt"
	const docBody = "hello steep note"

	// Reproduce Issue #292 step-by-step in a real 100x32 terminal:
	// Open Apps, filter for notes, launch it, then F4 to view it.
	h.Key("f2").Wait("SEARCH: type to filter apps...")
	h.Type("com.gostalgia.notes").Wait("Enter launches Notes")
	h.Key("enter").Wait("Notes is LIVE")
	steepAppRunning(t, client, "com.gostalgia.notes", true)
	h.Key("f4").Wait("Notes - [Untitled]")

	// Enter path, tab to content, type content, tab to Save, press Enter.
	h.Type(docPath).Key("tab").Type(docBody).Key("tab").Key("enter")
	h.Wait([]string{"Allow Notes file access?", docPath, "Only this path is granted."})

	// Declining with Esc keeps document modified and shows full cause + next step without truncation.
	h.KeyEsc().WaitNot("Allow Notes file access?")
	h.Wait([]string{"no write grant", "/apps/data/com.gostalgia.notes/."})

	// Selecting Save again and approving (Tab -> Enter) grants single-path session access and saves.
	h.Key("enter").Wait("Allow Notes file access?")
	h.Key("tab").Key("enter").Wait("Saved " + docPath)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var readOut struct {
		DataBase64 string `json:"data_base64"`
	}
	if err := client.Call(ctx, "fs/read", map[string]string{"path": docPath}, &readOut); err != nil {
		t.Fatalf("fs/read saved note: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(readOut.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != docBody {
		t.Fatalf("saved note = %q, want %q", string(got), docBody)
	}

	var grantsOut struct {
		Grants []struct {
			Path         string `json:"path"`
			Access       string `json:"access"`
			Recursive    bool   `json:"recursive"`
			SessionBound bool   `json:"session_bound"`
		} `json:"grants"`
	}
	if err := client.Call(ctx, "fs/grant/list", map[string]string{"app_id": "com.gostalgia.notes"}, &grantsOut); err != nil {
		t.Fatalf("fs/grant/list: %v", err)
	}
	if len(grantsOut.Grants) != 1 || grantsOut.Grants[0].Path != docPath || grantsOut.Grants[0].Access != "read-write" ||
		grantsOut.Grants[0].Recursive || !grantsOut.Grants[0].SessionBound {
		t.Fatalf("expected single minimal session-bound grant, got %+v", grantsOut.Grants)
	}
}
