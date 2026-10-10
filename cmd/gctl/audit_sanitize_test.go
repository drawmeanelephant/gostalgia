package main

import (
	"strings"
	"testing"
)

// Regression for #150: sanitizeTerminal feeds single-line tabwriter fields,
// so a server-supplied process name or error must not carry raw newlines or
// tabs into ps/history/logs output, where they forge rows and columns.
func TestAuditSanitizeTerminalKeepsNewlines(t *testing.T) {
	forged := "ok\n0\tkernel\tstopped\t0\t0\t-"
	got := sanitizeTerminal(forged)
	if strings.ContainsAny(got, "\n\t\r") {
		t.Fatalf("BUG: sanitizeTerminal kept a newline — a process name/error can forge rows in gctl ps/history/logs tables: %q", got)
	}
	if !strings.Contains(got, `\n`) || !strings.Contains(got, `\t`) {
		t.Fatalf("sanitizeTerminal should leave visible escapes for row/column breaks, got %q", got)
	}
	if got := sanitizeTerminal("na\x1b[31mme\x1b]52;c;eA==\x07\x1b[0m\x07done"); strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("sanitizeTerminal kept terminal sequences: %q", got)
	}
}

// Log content is legitimately multi-line; the block variant keeps real
// newlines and tabs while still stripping terminal sequences. It never
// feeds tabwriter fields.
func TestAuditSanitizeTerminalBlockKeepsShape(t *testing.T) {
	got := sanitizeTerminalBlock("line1\nline2\tcol\x1b[2J\x1b]0;t\x07end")
	if !strings.Contains(got, "line1\nline2\tcol") || !strings.HasSuffix(got, "end") {
		t.Fatalf("sanitizeTerminalBlock lost content shape: %q", got)
	}
	if strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("sanitizeTerminalBlock kept terminal sequences: %q", got)
	}
}
