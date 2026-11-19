package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
)

func TestMonochromeChromeFallbacksAreBounded(t *testing.T) {
	kit := New(theme.Monochrome(), Plain)
	if got, want := kit.Hint("Tab focus · ↑↓ item · ←→ grid · Loading… — type help"), "Tab focus | Up/Down item | Left/Right grid | Loading... - type help"; got != want {
		t.Fatalf("Hint = %q, want %q", got, want)
	}
	if got := kit.Hint("界 e\u0301 👩‍💻"); got != "界 e\u0301 👩‍💻" {
		t.Fatalf("Hint changed Unicode content: %q", got)
	}
	if got, want := kit.EmptyMarker(), "[EMPTY]"; got != want {
		t.Fatalf("EmptyMarker = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		width int
		want  string
	}{{0, ""}, {1, "."}, {2, ".."}, {3, "..."}, {4, "h..."}, {5, "hello"}} {
		got := kit.Truncate("hello", tc.width)
		if got != tc.want {
			t.Errorf("Truncate(width %d) = %q, want %q", tc.width, got, tc.want)
		}
		if ansi.StringWidth(got) > max(0, tc.width) {
			t.Errorf("Truncate(width %d) overflows: %q", tc.width, got)
		}
	}
	if got := kit.Truncate("界e\u0301x", 3); got != "..." {
		t.Fatalf("wide graphemes were split or retained past the suffix: %q", got)
	}
	if got := kit.Rule(4); got != "----" {
		t.Fatalf("Rule = %q", got)
	}
}

func TestHelpBarUsesOnlyThemeOwnedASCIIFallbacks(t *testing.T) {
	bindings := []Binding{{Key: "↑↓", Help: "NAVIGATE"}, {Key: "Enter", Help: "OPEN"}}
	mono := strings.TrimRight(New(theme.Monochrome(), Plain).HelpBar(bindings, 80), " ")
	if got, want := mono, "Up/Down NAVIGATE | Enter OPEN"; got != want {
		t.Fatalf("Monochrome HelpBar = %q, want %q", got, want)
	}
	plain := strings.TrimRight(New(theme.Nostalgia(), Plain).HelpBar(bindings, 80), " ")
	if got, want := plain, "↑↓ NAVIGATE · Enter OPEN"; got != want {
		t.Fatalf("Nostalgia Plain HelpBar = %q, want %q", got, want)
	}
}

func TestPlainNonMonochromeKeepsUnicodeChrome(t *testing.T) {
	kit := New(theme.Nostalgia(), Plain)
	owned := "↑↓ · … ◇ ● ─ – —"
	if got := kit.Hint(owned); got != owned {
		t.Fatalf("Plain changed non-Monochrome chrome: %q", got)
	}
	if got := kit.Truncate("hello", 4); got != "hel…" {
		t.Fatalf("Plain non-Monochrome Truncate = %q", got)
	}
	if got := kit.EmptyMarker(); got != "◇" {
		t.Fatalf("Plain non-Monochrome EmptyMarker = %q", got)
	}
	if strings.Contains(kit.Hint(owned), "|") {
		t.Fatal("Plain non-Monochrome separator became ASCII")
	}
}
