package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/ipc"
	"gostalgia/internal/runtime"
	"gostalgia/internal/security"
	"gostalgia/platform"
)

// hostClipboard is a host clipboard that records what is written to it.
type hostClipboard struct {
	mu        sync.Mutex
	text      string
	writes    int
	available bool
}

func (c *hostClipboard) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.available
}

func (c *hostClipboard) ReadText(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text, nil
}

func (c *hostClipboard) WriteText(_ context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text, c.writes = text, c.writes+1
	return nil
}

func (c *hostClipboard) state() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text, c.writes
}

// probeMsg runs fn on the event loop, where it may read the model.
type probeMsg struct{ fn func(*Model) }

// copyObserver is the shell under a real Bubble Tea program, reporting the
// answers to its copies and running probes.
type copyObserver struct {
	*Model
	results chan resultMsg
	copies  chan copyMsg
	probed  chan struct{}
	events  chan string
}

func (m *copyObserver) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseMsg:
		m.events <- fmt.Sprintf("%T: %+v", msg, msg)
	case bloom.SelectionMsg:
		m.events <- fmt.Sprintf("selection: %+v", msg)
	}
	if p, ok := msg.(probeMsg); ok {
		p.fn(m.Model)
		m.probed <- struct{}{}
		return m, nil
	}
	_, cmd := m.Model.Update(msg)
	switch msg := msg.(type) {
	case resultMsg:
		m.results <- msg
	case copyMsg:
		m.copies <- msg
	}
	return m, cmd
}

// A selection made with the mouse in a real terminal program copies through
// the runtime's authenticated clipboard/write under its operator policy:
// denied by default and with writes off, leaving both clipboards as they
// were, and written once both are on. The terminal never gets OSC 52.
func TestMouseSelectionCopiesThroughTheRuntimesPolicy(t *testing.T) {
	clip := &hostClipboard{text: "host-before", available: true}
	orig := platform.GetHostClipboard()
	platform.SetHostClipboard(clip)
	t.Cleanup(func() { platform.SetHostClipboard(orig) })

	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("shell test cleanup") })
	data, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	conn, err := platform.DialIPC(rt.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.NewClient(conn, info.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	call := func(method string, params, out any) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := client.Call(ctx, method, params, out); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	internal := func() string {
		t.Helper()
		var out struct{ Text string }
		call("clipboard/read", map[string]string{"target": "internal"}, &out)
		return out.Text
	}
	call("clipboard/write", map[string]string{"text": "internal-before", "target": "internal"}, nil)

	ctx := t.Context()
	m := &copyObserver{Model: New(ctx, client, rt.Done()), results: make(chan resultMsg, 20),
		copies: make(chan copyMsg, 4), probed: make(chan struct{}), events: make(chan string, 100)}
	output := &lockedBuffer{}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(output),
		tea.WithoutSignalHandler(), tea.WithWindowSize(80, 24))
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	t.Cleanup(func() { p.Kill(); <-done })
	const perStep = 30 * time.Second
	wait := func(step string, ch <-chan copyMsg) copyMsg {
		t.Helper()
		select {
		case msg := <-ch:
			return msg
		case <-time.After(perStep):
			for {
				select {
				case event := <-m.events:
					t.Logf("event before timeout: %s", event)
				default:
					t.Logf("last terminal output: %q", output.String())
					t.Fatalf("timed out at %s", step)
					return copyMsg{}
				}
			}
		}
	}
	probe := func(fn func(*Model)) {
		t.Helper()
		p.Send(probeMsg{fn})
		select {
		case <-m.probed:
		case <-time.After(perStep):
			t.Fatal("the probe timed out")
		}
	}
	select {
	case <-m.results: // Init's app list.
	case <-time.After(perStep):
		t.Fatal("init timed out")
	}
	p.Send(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "echo copy-me-please"})
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case r := <-m.results:
		if r.err != nil {
			t.Fatal(r.err)
		}
	case <-time.After(perStep):
		t.Fatal("echo timed out")
	}

	const word = "copy-me-please"
	selectWord := func(step string) (copyMsg, string) {
		t.Helper()
		var at image.Point
		probe(func(m *Model) { at, _ = findText(m, rectOf(t, m, slotBody), word) })
		if at == (image.Point{}) {
			t.Fatalf("%s: %q is not on screen", step, word)
		}
		end := at.Add(image.Pt(len(word)-1, 0))
		p.Send(tea.MouseClickMsg{X: at.X, Y: at.Y, Button: tea.MouseLeft})
		p.Send(tea.MouseMotionMsg{X: end.X, Y: end.Y, Button: tea.MouseLeft})
		p.Send(tea.MouseReleaseMsg{X: end.X, Y: end.Y, Button: tea.MouseLeft})
		msg := wait(step, m.copies)
		var toast string
		probe(func(m *Model) {
			// The newest toast is the copy's, and it is on screen.
			if ts := m.notifs.ActiveToasts(); len(ts) > 0 && strings.Contains(toastText(m), ts[len(ts)-1].Title) {
				toast = ts[len(ts)-1].Title + " · " + ts[len(ts)-1].Message
			}
		})
		return msg, toast
	}

	// The default policy keeps the host clipboard off.
	msg, toast := selectWord("default policy")
	if msg.err == nil || !strings.Contains(toast, "Copy blocked") || !strings.Contains(toast, "clipboard.enabled") ||
		!strings.Contains(toast, "sys/policy/update") {
		t.Fatalf("default policy: %v, toast %q", msg.err, toast)
	}
	if text, n := clip.state(); text != "host-before" || n != 0 || internal() != "internal-before" {
		t.Fatalf("a denied copy changed a clipboard: host %q (%d writes), internal %q", text, n, internal())
	}

	// The integration on, but writes off.
	call("sys/policy/update", security.OperatorPolicy{Clipboard: security.ClipboardPolicy{Enabled: true, AllowHostRead: true}}, nil)
	msg, toast = selectWord("writes off")
	if msg.err == nil || !strings.Contains(toast, "Copy blocked") || !strings.Contains(toast, "clipboard.allow_host_write") ||
		strings.Contains(toast, "clipboard.enabled") {
		t.Fatalf("writes off: %v, toast %q", msg.err, toast)
	}
	if text, n := clip.state(); text != "host-before" || n != 0 || internal() != "internal-before" {
		t.Fatalf("a denied copy changed a clipboard: host %q (%d writes), internal %q", text, n, internal())
	}

	// Both on: the host clipboard gets the selection.
	call("sys/policy/update", security.OperatorPolicy{Clipboard: security.ClipboardPolicy{Enabled: true, AllowHostWrite: true}}, nil)
	msg, toast = selectWord("allowed")
	if msg.err != nil || !strings.Contains(toast, "Copied") || !strings.Contains(toast, "host clipboard") {
		t.Fatalf("allowed: %v, toast %q", msg.err, toast)
	}
	if text, n := clip.state(); text != word || n != 1 {
		t.Fatalf("the host clipboard has %q after %d writes, want %q once", text, n, word)
	}

	// A host with no clipboard says so.
	clip.mu.Lock()
	clip.available = false
	clip.mu.Unlock()
	msg, toast = selectWord("no host clipboard")
	if msg.err == nil || !strings.Contains(toast, "Copy failed") {
		t.Fatalf("no host clipboard: %v, toast %q", msg.err, toast)
	}

	p.Quit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	done <- nil // for the cleanup
	stream := output.String()
	if !strings.Contains(stream, "\x1b[?1006h") {
		t.Fatal("the program never asked for mouse reports")
	}
	if strings.Contains(stream, "\x1b]52") {
		t.Fatal("the terminal stream holds OSC 52")
	}
}
