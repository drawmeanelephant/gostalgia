package notifications

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// severityLabels are the mandatory labels every notification surface shows
// for each level.
var severityLabels = map[Level]string{
	LevelInfo:    "INFO",
	LevelWarning: "WARN",
	LevelError:   "ERROR",
}

// severityCases covers the five built-in themes in a color profile and Plain
// as its own mode. Monochrome's chrome is ASCII in every profile, so Plain
// covers its raw output too.
type severityCase struct {
	name string
	kit  ui.Kit
}

func severityCases() []severityCase {
	cases := []severityCase{
		{"nostalgia/ansi256", ui.New(theme.Nostalgia(), ui.ANSI256)},
		{"midnight/ansi256", ui.New(theme.Midnight(), ui.ANSI256)},
		{"midnight/truecolor", ui.New(theme.Midnight(), ui.TrueColor)},
		{"monochrome/ansi256", ui.New(theme.Monochrome(), ui.ANSI256)},
		{"high-contrast/ansi256", ui.New(theme.HighContrast(), ui.ANSI256)},
		{"high-contrast-light/ansi256", ui.New(theme.HighContrastLight(), ui.ANSI256)},
		{"nostalgia/plain", ui.New(theme.Nostalgia(), ui.Plain)},
		{"monochrome/plain", ui.New(theme.Monochrome(), ui.Plain)},
	}
	return cases
}

func severityManager() *Manager {
	mgr := NewManager(10, 3, 5)
	for _, level := range []Level{LevelInfo, LevelWarning, LevelError} {
		mgr.Record(Notification{
			ID:        string(level),
			Timestamp: time.Date(2025, 3, 14, 15, 9, 26, 0, time.UTC),
			Title:     "event",
			Message:   "detail",
			Level:     level,
		})
	}
	return mgr
}

// Toast, full row and compact row share one INFO/WARN/ERROR treatment:
// color modes use a severity symbol and label, Monochrome and Plain use the
// literal bracketed label, and Plain emits no color or weight.
func TestSeveritySharedAcrossSurfaces(t *testing.T) {
	const (
		fullBounds    = 80
		compactWidth  = 30
		compactHeight = 10
	)
	for _, tc := range severityCases() {
		t.Run(tc.name, func(t *testing.T) {
			mgr := severityManager()
			surfaces := map[string]string{
				"toast":   mgr.RenderToasts(tc.kit, fullBounds),
				"full":    mgr.RenderPanel(tc.kit, ui.Bounds{Width: fullBounds, Height: 24}),
				"compact": mgr.RenderPanel(tc.kit, ui.Bounds{Width: compactWidth, Height: compactHeight}),
			}
			bracketed := tc.kit.ASCIIChrome() || tc.kit.Mode() == ui.Plain
			for surface, out := range surfaces {
				if out == "" {
					t.Fatalf("%s: %s surface rendered nothing", tc.name, surface)
				}
				for level, label := range severityLabels {
					if got := strings.Count(out, label); got != 1 {
						t.Fatalf("%s: %s surface counts %q %d times:\n%s", tc.name, surface, label, got, out)
					}
					if bracketed {
						if !strings.Contains(out, "["+label+"]") {
							t.Fatalf("%s: %s surface lacks bracketed [%s]:\n%s", tc.name, surface, label, out)
						}
					} else if !strings.Contains(out, severitySymbol(level)+" "+label) {
						t.Fatalf("%s: %s surface lacks %s %s:\n%s", tc.name, surface, severitySymbol(level), label, out)
					}
				}
				if bracketed {
					for _, level := range []Level{LevelInfo, LevelWarning, LevelError} {
						if strings.Contains(out, severitySymbol(level)) {
							t.Fatalf("%s: %s surface uses a glyph where chrome needs [%s]:\n%s", tc.name, surface, severityLabels[level], out)
						}
					}
				}
				if tc.kit.Mode() == ui.Plain && strings.Contains(out, "\x1b") {
					t.Fatalf("%s: Plain %s surface emitted escapes:\n%q", tc.name, surface, out)
				}
				if tc.kit.Mode() != ui.Plain && !strings.Contains(out, "\x1b[") {
					t.Fatalf("%s: %s surface has no color treatment:\n%s", tc.name, surface, out)
				}
			}
		})
	}
}

// severitySymbol is the color-mode glyph each level carries.
func severitySymbol(level Level) string {
	switch level {
	case LevelError:
		return "■"
	case LevelWarning:
		return "▲"
	default:
		return "◆"
	}
}

// Each level's cue is styled with its own semantic state, so color modes keep
// INFO, WARN and ERROR distinguishable without reading the label alone.
func TestSeverityColorsDifferByLevel(t *testing.T) {
	for _, tc := range severityCases() {
		if tc.kit.Mode() == ui.Plain {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			rendered := map[Level]string{}
			for _, level := range []Level{LevelInfo, LevelWarning, LevelError} {
				mgr := NewManager(10, 3, 5)
				mgr.Record(Notification{ID: "same", Timestamp: time.Date(2025, 3, 14, 15, 9, 26, 0, time.UTC),
					Title: "event", Message: "detail", Level: level})
				rendered[level] = mgr.RenderToasts(tc.kit, 48)
			}
			if rendered[LevelInfo] == rendered[LevelWarning] || rendered[LevelInfo] == rendered[LevelError] ||
				rendered[LevelWarning] == rendered[LevelError] {
				t.Fatalf("%s: levels share one styled treatment:\n%q\n%q\n%q", tc.name,
					rendered[LevelInfo], rendered[LevelWarning], rendered[LevelError])
			}
		})
	}
}

// A custom badge adds to the severity instead of replacing it.
func TestCustomBadgeSupplementsSeverity(t *testing.T) {
	mgr := NewManager(10, 3, 5)
	mgr.Record(Notification{Title: "Copy blocked", Message: "by policy", Level: LevelWarning, Badge: "COPY"})
	for _, tc := range []severityCase{
		{"colored", ui.New(theme.Nostalgia(), ui.ANSI256)},
		{"plain", ui.New(theme.Nostalgia(), ui.Plain)},
	} {
		toast := mgr.RenderToasts(tc.kit, 80)
		full := mgr.RenderPanel(tc.kit, ui.Bounds{Width: 80, Height: 24})
		compact := mgr.RenderPanel(tc.kit, ui.Bounds{Width: 30, Height: 10})
		for surface, out := range map[string]string{"toast": toast, "full": full, "compact": compact} {
			if !strings.Contains(out, "WARN") {
				t.Fatalf("%s %s lost its severity:\n%s", tc.name, surface, out)
			}
		}
		if !strings.Contains(toast, "COPY") || !strings.Contains(full, "COPY") {
			t.Fatalf("%s toast or row lost the custom badge:\ntoast %q\nfull %q", tc.name, toast, full)
		}
		if strings.Contains(toast, "BACKOFF") || strings.Contains(full, "BACKOFF") {
			t.Fatalf("%s still uses the level's old badge name:\ntoast %q\nfull %q", tc.name, toast, full)
		}
	}
}

// historySignature records the acknowledgement state rendering must not
// change.
func historySignature(mgr *Manager) string {
	var b strings.Builder
	for _, n := range mgr.History() {
		b.WriteString(n.ID)
		b.WriteByte(':')
		b.WriteString(string(n.Level))
		b.WriteByte(':')
		if n.Seen {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
		b.WriteByte(' ')
	}
	return b.String()
}

func lineContaining(t *testing.T, text, want string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no line contains %q:\n%s", want, text)
	return ""
}

// Seen and unseen rows of the same severity keep the same severity cue but
// distinct unseen markers, and focus moves between them without changing
// either. Rendering never acknowledges anything.
func TestSeverityUnseenAndFocusIndependent(t *testing.T) {
	for _, tc := range []severityCase{
		{"colored", ui.New(theme.Nostalgia(), ui.ANSI256)},
		{"plain", ui.New(theme.Nostalgia(), ui.Plain)},
		{"monochrome", ui.New(theme.Monochrome(), ui.ANSI256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(10, 3, 5)
			mgr.Record(Notification{ID: "seen", Timestamp: time.Date(2025, 3, 14, 15, 9, 26, 0, time.UTC),
				Title: "older crash", Message: "exit 1", Level: LevelError})
			mgr.Record(Notification{ID: "unseen", Timestamp: time.Date(2025, 3, 14, 15, 10, 26, 0, time.UTC),
				Title: "newer crash", Message: "exit 2", Level: LevelError})
			mgr.MarkSeen("seen")

			unseenMarker := strings.TrimSpace(ansi.Strip(seenMarker(tc.kit, Notification{Seen: false})))
			focused := tc.kit.Theme().Focus.Marker + " "
			before := historySignature(mgr)

			for _, selected := range []string{"unseen", "seen"} {
				if !mgr.Select(selected) {
					t.Fatalf("could not select %s", selected)
				}
				for surface, bounds := range map[string]ui.Bounds{
					"full":    {Width: 80, Height: 24},
					"compact": {Width: 30, Height: 10},
				} {
					out := ansi.Strip(mgr.RenderPanel(tc.kit, bounds))
					unseenLine := lineContaining(t, out, "newer crash")
					seenLine := lineContaining(t, out, "older crash")
					for _, line := range []string{unseenLine, seenLine} {
						if !strings.Contains(line, "ERROR") {
							t.Fatalf("%s %s row lost its severity with %s selected:\n%s", tc.name, surface, selected, out)
						}
					}
					if !strings.Contains(unseenLine, unseenMarker) {
						t.Fatalf("%s %s unseen row lacks %q:\n%s", tc.name, surface, unseenMarker, out)
					}
					if strings.Contains(seenLine, unseenMarker) {
						t.Fatalf("%s %s seen row keeps the unseen marker:\n%s", tc.name, surface, out)
					}
					selectedLine := unseenLine
					otherLine := seenLine
					if selected == "seen" {
						selectedLine, otherLine = seenLine, unseenLine
					}
					if !strings.Contains(selectedLine, focused) {
						t.Fatalf("%s %s selected row lacks the focus marker %q:\n%s", tc.name, surface, focused, out)
					}
					if strings.Contains(otherLine, focused) {
						t.Fatalf("%s %s moved focus to the other row:\n%s", tc.name, surface, out)
					}
				}
				if got := historySignature(mgr); got != before {
					t.Fatalf("%s: rendering changed acknowledgement from %q to %q", tc.name, before, got)
				}
			}

			// Moving selection is not acknowledgement, and explicit
			// inspection changes only the entry it names.
			mgr.SelectNext()
			mgr.SelectPrev()
			if got := historySignature(mgr); got != before {
				t.Fatalf("%s: selection changed acknowledgement from %q to %q", tc.name, before, got)
			}
			_ = mgr.RenderToasts(tc.kit, 80)
			if got := historySignature(mgr); got != before {
				t.Fatalf("%s: toast rendering changed acknowledgement from %q to %q", tc.name, before, got)
			}
			mgr.MarkSeen("unseen")
			if got := historySignature(mgr); got != "unseen:error:1 seen:error:1 " {
				t.Fatalf("%s: explicit inspection produced %q", tc.name, got)
			}
		})
	}
}
