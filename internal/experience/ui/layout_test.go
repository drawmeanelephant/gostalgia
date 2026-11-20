package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestTruncateGraphemeCells(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  string
	}{
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"界界A", 4, "界…"},
		{"e\u0301clair", 2, "e\u0301…"},
		{"👩‍💻x", 2, "…"},
		{"👩‍💻x", 3, "👩‍💻x"},
		{"🇺🇸xy", 3, "🇺🇸…"},
		{"abc\nsecond", 8, "abc"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := Truncate(tc.text, tc.width); got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
			}
		})
	}
}

func TestANSILayoutPreservesStyles(t *testing.T) {
	text := "\x1b[31m界e\u0301👩‍💻x\x1b[0m"
	got := Truncate(text, 4)
	if ansi.Strip(got) != "界e\u0301…" || !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("styled truncation = %q", got)
	}
	assertRectangle(t, Fit(text+"\nsecond", Bounds{4, 3}), Bounds{4, 3})
	for _, width := range []int{1, 2, 3, 4, 80} {
		for _, line := range strings.Split(Wrap(text, width), "\n") {
			if ansi.StringWidth(line) > width || !utf8.ValidString(line) {
				t.Fatalf("wrapped line %q exceeds %d cells", line, width)
			}
			if strings.Contains(ansi.Strip(line), "👩") && !strings.Contains(ansi.Strip(line), "👩‍💻") {
				t.Fatalf("split emoji cluster: %q", line)
			}
		}
	}
	if got := ansi.Strip(Tail(text, 3)); got != "👩‍💻x" {
		t.Fatalf("Tail = %q", got)
	}
	// A cut that lands inside a wide cluster drops it instead of exceeding
	// the requested cell budget.
	if got := ansi.Strip(Tail("界abc", 4)); got != "abc" {
		t.Fatalf("Tail across a wide cluster = %q", got)
	}
}

func TestFitAndWrap(t *testing.T) {
	if got := Fit("界界A\nsecond", Bounds{3, 3}); got != "界 \nsec\n   " {
		t.Fatalf("Fit = %q", got)
	}
	if got := Wrap("e\u0301界👩‍💻Z", 3); got != "e\u0301界\n👩‍💻Z" {
		t.Fatalf("Wrap = %q", got)
	}
	for _, bounds := range []Bounds{{0, 3}, {3, 0}, {-1, 3}, {3, -1}} {
		if got := Fit("text", bounds); got != "" {
			t.Fatalf("nonpositive bounds %+v = %q", bounds, got)
		}
	}
	if Wrap("text", 0) != "" || Tail("text", 0) != "" {
		t.Fatal("zero-width layout should be empty")
	}
}

func TestSanitizeExternalText(t *testing.T) {
	text := "\x1b[31mred\x1b[0m\x1b]52;c;clipboard\a\x1bPpayload\x1b\\\r\nnext\t\x00\x7f"
	if got := Sanitize(text); got != "red\nnext    " {
		t.Fatalf("Sanitize = %q", got)
	}
	if got := Sanitize("e\u0301 👩‍💻 界"); got != "e\u0301 👩‍💻 界" {
		t.Fatalf("sanitizing changed printable clusters: %q", got)
	}
	if !utf8.ValidString(Sanitize("\xff")) {
		t.Fatal("invalid UTF-8 was not repaired")
	}
}

func TestFirstCluster(t *testing.T) {
	for text, want := range map[string]string{
		"":             "",
		"abc":          "a",
		"e\u0301clair": "e\u0301",
		"👩‍💻x":         "👩‍💻",
		"🇺🇸xy":         "🇺🇸",
		"\xffrest":     "\xff",
	} {
		if got := FirstCluster(text); got != want {
			t.Errorf("FirstCluster(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestGraphemeClusters(t *testing.T) {
	input := "e\u0301界👩‍💻🇺🇸"
	clusters := GraphemeClusters(input)
	want := []string{"e\u0301", "界", "👩‍💻", "🇺🇸"}
	if len(clusters) != len(want) {
		t.Fatalf("GraphemeClusters(%q) returned %d clusters, want %d: %v", input, len(clusters), len(want), clusters)
	}
	for i := range want {
		if clusters[i] != want[i] {
			t.Errorf("cluster[%d] = %q, want %q", i, clusters[i], want[i])
		}
	}
	expectedWidths := []int{1, 2, 2, 2}
	for i, c := range clusters {
		if got := GraphemeWidth(c); got != expectedWidths[i] {
			t.Errorf("GraphemeWidth(%q) = %d, want %d", c, got, expectedWidths[i])
		}
	}
}

func TestEscapeSequenceFiltering(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "CSI clear and cursor positioning",
			input: "\x1b[2J\x1b[H\x1b[10;20Hmalicious text",
			want:  "malicious text",
		},
		{
			name:  "CSI private mode switches",
			input: "\x1b[?1049h\x1b[?25l\x1b[?1000halt-screen-toggle\x1b[?1049l",
			want:  "alt-screen-toggle",
		},
		{
			name:  "CSI color and font formatting",
			input: "\x1b[31;1;4mRed Bold Underline\x1b[0m",
			want:  "Red Bold Underline",
		},
		{
			name:  "OSC 52 clipboard injection with BEL",
			input: "\x1b]52;c;Y29weXBhc3Rl\aSafe text",
			want:  "Safe text",
		},
		{
			name:  "OSC 52 clipboard injection with ST",
			input: "\x1b]52;c;Y29weXBhc3Rl\x1b\\Safe text",
			want:  "Safe text",
		},
		{
			name:  "OSC 0 window title hijack",
			input: "\x1b]0;Evil Title\x07Text",
			want:  "Text",
		},
		{
			name:  "OSC 8 hyperlink injection",
			input: "\x1b]8;;http://attacker.example.com\x1b\\Click Here\x1b]8;;\x1b\\",
			want:  "Click Here",
		},
		{
			name:  "DCS device control sequence",
			input: "\x1bP$q\"p\x1b\\DCS filtered",
			want:  "DCS filtered",
		},
		{
			name:  "APC and PM sequences",
			input: "\x1b_apc_payload\x1b\\\x1b^pm_payload\x1b\\Clean",
			want:  "Clean",
		},
		{
			name:  "C1 UTF-8 control character U+009B",
			input: "\xc2\x9b31mInjected\xc2\x9b0m",
			want:  "31mInjected0m",
		},
		{
			name:  "Low control characters",
			input: "Hello\x00\x07\x08\x0b\x0c\x0e\x0f\x1aWorld\x7f",
			want:  "HelloWorld",
		},
		{
			name:  "Truncated and broken escape sequences",
			input: "Prefix\x1b[2KClean\x1b",
			want:  "PrefixClean",
		},
		{
			name:  "Preserves newlines, expands tabs to tab stops, combining runes",
			input: "Line 1\tTabbed\r\nLine 2 with e\u0301 and 界 and 👩‍💻",
			want:  "Line 1  Tabbed\nLine 2 with e\u0301 and 界 and 👩‍💻",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitize(tc.input)
			if got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.input, got, tc.want)
			}
			if strings.Contains(got, "\x1b") {
				t.Fatalf("sanitized output contains raw ESC: %q", got)
			}
		})
	}
}

func assertRectangle(t *testing.T, view string, bounds Bounds) {
	t.Helper()
	if bounds.Width <= 0 || bounds.Height <= 0 {
		if view != "" {
			t.Fatalf("expected empty rectangle %+v, got %q", bounds, view)
		}
		return
	}
	lines := strings.Split(view, "\n")
	if len(lines) != bounds.Height {
		t.Fatalf("height = %d, want %d\n%q", len(lines), bounds.Height, view)
	}
	for _, line := range lines {
		if got := ansi.StringWidth(line); got != bounds.Width {
			t.Fatalf("line width = %d, want %d: %q", got, bounds.Width, line)
		}
		if !utf8.ValidString(line) {
			t.Fatalf("invalid UTF-8: %q", line)
		}
	}
}
