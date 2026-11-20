package notifications

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

var updateSeveritySnapshots = flag.Bool("update-notification-severity", false,
	"update notification severity snapshots")

// TestSeverityUnseenSnapshot pins the equal-severity seen/unseen pair with
// each row selected in turn, in both the full and compact Notification
// Center. It is Plain so the snapshot records layout and acknowledgement,
// not terminal colors.
func TestSeverityUnseenSnapshot(t *testing.T) {
	mgr := NewManager(10, 3, 5)
	mgr.Record(Notification{ID: "seen", Timestamp: time.Date(2025, 3, 14, 15, 9, 26, 0, time.UTC),
		Title: "older crash", Message: "exit 1", Level: LevelError})
	mgr.Record(Notification{ID: "unseen", Timestamp: time.Date(2025, 3, 14, 15, 10, 26, 0, time.UTC),
		Title: "newer crash", Message: "exit 2", Level: LevelError})
	mgr.MarkSeen("seen")

	kit := ui.New(theme.Monochrome(), ui.Plain)
	var b strings.Builder
	b.WriteString("# Notification severity: equal severity, distinct acknowledgement\n")
	for _, selected := range []string{"unseen", "seen"} {
		if !mgr.Select(selected) {
			t.Fatalf("could not select %s", selected)
		}
		b.WriteString("\n## selected " + selected + "\n")
		for _, section := range []struct {
			name   string
			bounds ui.Bounds
		}{
			{"full 80x24", ui.Bounds{Width: 80, Height: 24}},
			{"compact 30x10", ui.Bounds{Width: 30, Height: 10}},
		} {
			b.WriteString("### " + section.name + "\n")
			lines := strings.Split(mgr.RenderPanel(kit, section.bounds), "\n")
			for i := range lines {
				lines[i] = strings.TrimRight(lines[i], " ")
			}
			b.WriteString(strings.Join(lines, "\n") + "\n")
		}
	}
	got := b.String()

	path := filepath.Join("testdata", "notification-severity-unseen.snapshot")
	if *updateSeveritySnapshots {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("notification severity snapshot changed:\n%s", got)
	}
}
