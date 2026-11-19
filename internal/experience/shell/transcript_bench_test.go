package shell

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// transcriptBenchKinds cycles through every transcript entry kind.
var transcriptBenchKinds = []string{"output", "muted", "accent", "error", "command"}

// transcriptBenchLine is fixture line i: uniquely numbered, of mixed length
// and script, so that some lines wrap at 80 columns and some hold wide or
// combining graphemes.
func transcriptBenchLine(i int) string {
	switch i % 4 {
	case 0:
		return fmt.Sprintf("%05d short output line", i)
	case 1:
		return fmt.Sprintf("%05d %s", i, strings.Repeat("a wrapped line of ordinary words ", 5))
	case 2:
		return fmt.Sprintf("%05d 界面 he\u0301llo wörld 日本語 テキスト", i)
	}
	return fmt.Sprintf("%05d C:\\users\\guest> echo %s", i, strings.Repeat("x", 40))
}

// transcriptBenchModel is a prompt screen of w×h cells whose transcript was
// given appended fixture lines, one entry each, at its tail.
func transcriptBenchModel(b *testing.B, w, h, appended int) *Model {
	b.Helper()
	m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), ui.ANSI256)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.setMode(modePrompt)
	for i := range appended {
		m.append(entry{transcriptBenchLine(i), transcriptBenchKinds[i%len(transcriptBenchKinds)]})
	}
	return m
}

// BenchmarkTranscriptRender times delivered frames, Model.View with the
// root's composition, of the prompt screen over a full transcript. Each
// case names the lines appended; the retained-lines metric is how many the
// transcript kept under its cap. steady draws unchanged frames, append
// appends one line before each frame, and restyle switches the theme
// before each frame.
func BenchmarkTranscriptRender(b *testing.B) {
	for _, size := range [][2]int{{80, 24}, {200, 60}} {
		for _, appended := range []int{400, 10000} {
			name := fmt.Sprintf("%dx%d/appended=%d", size[0], size[1], appended)
			b.Run(name+"/steady", func(b *testing.B) {
				m := transcriptBenchModel(b, size[0], size[1], appended)
				var out string
				b.ReportAllocs()
				for b.Loop() {
					out = m.View().Content
				}
				last := fmt.Sprintf("%05d", appended-1)
				if !strings.Contains(out, last) {
					b.Fatalf("frame does not show the last line %s:\n%s", last, out)
				}
				b.ReportMetric(float64(len(m.transcript)), "retained-lines")
			})
			b.Run(name+"/append", func(b *testing.B) {
				m := transcriptBenchModel(b, size[0], size[1], appended)
				var out string
				n := appended
				b.ReportAllocs()
				for b.Loop() {
					m.append(entry{transcriptBenchLine(n), transcriptBenchKinds[n%len(transcriptBenchKinds)]})
					n++
					out = m.View().Content
				}
				last := fmt.Sprintf("%05d", n-1)
				if !strings.Contains(out, last) {
					b.Fatalf("frame does not show the last line %s:\n%s", last, out)
				}
				b.ReportMetric(float64(len(m.transcript)), "retained-lines")
			})
			b.Run(name+"/restyle", func(b *testing.B) {
				m := transcriptBenchModel(b, size[0], size[1], appended)
				themes := []string{"midnight", "nostalgia"}
				var out string
				i := 0
				b.ReportAllocs()
				for b.Loop() {
					m.SetThemeByName(themes[i%2])
					i++
					out = m.View().Content
				}
				last := fmt.Sprintf("%05d", appended-1)
				if !strings.Contains(out, last) {
					b.Fatalf("frame does not show the last line %s:\n%s", last, out)
				}
				b.ReportMetric(float64(len(m.transcript)), "retained-lines")
			})
		}
	}
}
