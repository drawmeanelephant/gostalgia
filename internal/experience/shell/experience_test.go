package shell

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/platform"
)

type experienceCaller struct {
	mu      syncMutex
	procs   []taskmanager.ProcessRow
	stopped []int32
	reaped  int
	logs    map[int32]string
}

type syncMutex struct{}

func (e *experienceCaller) Call(ctx context.Context, method string, params, out any) error {
	switch method {
	case "app/list":
		var apps []appStatus
		data, _ := json.Marshal(apps)
		return json.Unmarshal(data, out)
	case "proc/list":
		data, _ := json.Marshal(e.procs)
		return json.Unmarshal(data, out)
	case "proc/stop":
		var p struct {
			ID int32 `json:"id"`
		}
		data, _ := json.Marshal(params)
		_ = json.Unmarshal(data, &p)
		e.stopped = append(e.stopped, p.ID)
		for i := range e.procs {
			if e.procs[i].ID == p.ID {
				e.procs[i].State = "stopped"
				e.procs[i].ExitCode = 137
			}
		}
		return nil
	case "proc/reap":
		count := 0
		var remaining []taskmanager.ProcessRow
		for _, p := range e.procs {
			if p.State == "stopped" || p.State == "failed" {
				count++
			} else {
				remaining = append(remaining, p)
			}
		}
		e.procs = remaining
		e.reaped += count
		resp := map[string]any{"reaped": count}
		data, _ := json.Marshal(resp)
		return json.Unmarshal(data, out)
	case "proc/logs":
		var p struct {
			ID int32 `json:"id"`
		}
		data, _ := json.Marshal(params)
		_ = json.Unmarshal(data, &p)
		logText := e.logs[p.ID]
		resp := map[string]any{
			"combined": map[string]any{"content": logText},
		}
		resData, _ := json.Marshal(resp)
		return json.Unmarshal(resData, out)
	}
	return nil
}

func TestTaskManagerToggleAndNavigation(t *testing.T) {
	client := &experienceCaller{
		procs: []taskmanager.ProcessRow{
			{ID: 1, Name: "init", State: "running", Resources: platform.ResourceUsage{Supported: true, MemoryBytes: 1024 * 1024}},
			{ID: 2, Name: "worker", State: "running", Resources: platform.ResourceUsage{Supported: true, MemoryBytes: 2 * 1024 * 1024}},
		},
		logs: map[int32]string{
			2: "worker started\nprocessing batch\n",
		},
	}
	m := New(context.Background(), client, nil)

	// Initially in home view
	if m.taskView || m.notifView || m.shelf {
		t.Fatal("expected home view initially")
	}

	// Press F5 to open Task Manager
	m.Update(tea.KeyPressMsg{Code: tea.KeyF5})
	if !m.taskView {
		t.Fatal("expected taskView active after F5")
	}

	// Feed process update
	m.Update(procUpdateMsg{procs: client.procs})

	view := m.View().Content
	if !strings.Contains(view, "LIVE TASK MANAGER") {
		t.Fatal("expected LIVE TASK MANAGER header in view")
	}
	if !strings.Contains(view, "init") || !strings.Contains(view, "worker") {
		t.Fatal("expected process names in table view")
	}

	// Navigate down to worker (index 1)
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.tasks.SelectedIndex() != 1 {
		t.Fatalf("expected selected index 1, got %d", m.tasks.SelectedIndex())
	}

	// View logs with 'l'
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "l"})
	// Simulate log arrival
	m.Update(procLogsMsg{pid: 2, name: "worker", content: client.logs[2]})

	logView := m.View().Content
	if !strings.Contains(logView, "CHILD LOG TAIL: worker (PID 2)") {
		t.Fatalf("expected child log tail header, got:\n%s", logView)
	}
	if !strings.Contains(logView, "processing batch") {
		t.Fatalf("expected log content in view, got:\n%s", logView)
	}

	// Close logs modal with Esc
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.tasks.Mode() != taskmanager.ModeTable {
		t.Fatal("expected return to ModeTable after Esc")
	}

	// Stop worker with 'x'
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "x"})
	m.Update(procActionMsg{status: "Stopped process 2"})
	status, _ := m.tasks.Status()
	if !strings.Contains(status, "Stopped process 2") {
		t.Fatalf("unexpected task status: %q", status)
	}

	// Esc returns to Home view
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.taskView {
		t.Fatal("expected taskView=false after Esc")
	}
}

func TestNotificationCenterAndToasts(t *testing.T) {
	client := &experienceCaller{}
	m := New(context.Background(), client, nil)

	// Record notification (crashed app)
	r := receipts.New(10, "broken-daemon", "failed", 1, "nil pointer", 1, "panic: nil pointer dereference")
	m.receipts.Add(r)
	m.notifs.Record(notifications.Notification{
		ID:        "ntf-1",
		Title:     "broken-daemon Crashed",
		Message:   "Exit code 1",
		Level:     notifications.LevelError,
		PID:       10,
		Receipt:   &r,
		Timestamp: time.Now(),
	})

	// Toasts should be visible in home view
	view := m.View().Content
	if !strings.Contains(view, "broken-daemon Crashed") {
		t.Fatalf("expected toast in home view, got:\n%s", view)
	}
	if !strings.Contains(view, "CRASH") {
		t.Fatalf("expected CRASH badge in toast, got:\n%s", view)
	}

	// Notification distinction: accepted/recorded != seen
	if m.notifs.UnseenCount() != 1 {
		t.Fatalf("expected 1 unseen notification, got %d", m.notifs.UnseenCount())
	}
	if m.notifs.History()[0].Seen {
		t.Fatal("recorded notification should have Seen=false initially")
	}

	// Open Notification Center with F6
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if !m.notifView {
		t.Fatal("expected notifView active after F6")
	}

	panelView := m.View().Content
	if !strings.Contains(panelView, "NOTIFICATION CENTER") {
		t.Fatalf("expected NOTIFICATION CENTER header, got:\n%s", panelView)
	}
	if !strings.Contains(panelView, "broken-daemon Crashed") {
		t.Fatalf("expected broken-daemon Crashed in panel, got:\n%s", panelView)
	}

	// Mark seen / inspect receipt with Enter
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.notifs.History()[0].Seen != true {
		t.Fatal("expected notification to be marked seen after viewing")
	}
	// Enter on notification with receipt opens crash receipt view in task manager
	if !m.taskView || m.tasks.Mode() != taskmanager.ModeReceipt {
		t.Fatal("expected receipt view to open upon Enter")
	}
	receiptView := m.View().Content
	if !strings.Contains(receiptView, "CRASH RECEIPT") || !strings.Contains(receiptView, "panic: nil pointer dereference") {
		t.Fatalf("expected crash receipt with panic log excerpt, got:\n%s", receiptView)
	}
}

func TestDoNotDisturbSuppressesToasts(t *testing.T) {
	client := &experienceCaller{}
	m := New(context.Background(), client, nil)

	// Enable DND via command
	res := execute(context.Background(), client, "/users/guest", "dnd on")
	m.Update(res)

	if !m.notifs.DND() {
		t.Fatal("expected DND to be true")
	}

	// Record notification while DND is active
	m.notifs.Record(notifications.Notification{
		Title:   "Background Sync Alert",
		Message: "Sync delayed",
		Level:   notifications.LevelWarning,
	})

	// Active toasts must be suppressed
	if len(m.notifs.ActiveToasts()) != 0 {
		t.Fatalf("expected 0 active toasts under DND, got %d", len(m.notifs.ActiveToasts()))
	}
	view := m.View().Content
	if strings.Contains(view, "Background Sync Alert") {
		t.Fatal("toast should not appear in home view under DND")
	}

	// History is NOT destroyed!
	if len(m.notifs.History()) != 1 {
		t.Fatalf("expected history to preserve event under DND, got %d", len(m.notifs.History()))
	}

	// Disable DND via command
	resOff := execute(context.Background(), client, "/users/guest", "dnd off")
	m.Update(resOff)
	if m.notifs.DND() {
		t.Fatal("expected DND to be false")
	}
}

func TestProcessLifecycleCrashDiagnostics(t *testing.T) {
	client := &experienceCaller{
		procs: []taskmanager.ProcessRow{
			{ID: 5, Name: "service-x", State: "running"},
		},
		logs: map[int32]string{
			5: "fatal error: stack overflow\ngoroutine 1:\n",
		},
	}
	m := New(context.Background(), client, nil)

	// Step 1: Initial running process update
	m.Update(procUpdateMsg{procs: client.procs})

	if len(m.receipts.List()) != 0 {
		t.Fatal("expected 0 crash receipts for running process")
	}

	// Step 2: Process crashes (fails with exit code 2)
	client.procs[0].State = "failed"
	client.procs[0].ExitCode = 2
	client.procs[0].Error = "terminated by signal"
	client.procs[0].RestartCount = 1

	cmd := m.handleProcessUpdates(client.procs)
	if cmd == nil {
		t.Fatal("expected cmd to fetch logs for crash receipt")
	}

	// Verify receipt generated
	receiptsList := m.receipts.List()
	if len(receiptsList) != 1 {
		t.Fatalf("expected 1 crash receipt, got %d", len(receiptsList))
	}
	if receiptsList[0].PID != 5 || receiptsList[0].ExitCode != 2 {
		t.Fatalf("unexpected receipt: %+v", receiptsList[0])
	}

	// Verify notification recorded
	if m.notifs.UnseenCount() != 1 {
		t.Fatalf("expected 1 unseen notification, got %d", m.notifs.UnseenCount())
	}
	notif := m.notifs.History()[0]
	if notif.PID != 5 || notif.Level != notifications.LevelError {
		t.Fatalf("unexpected notification: %+v", notif)
	}

	// Simulate receipt log arrival
	m.Update(receiptLogsMsg{pid: 5, receiptID: receiptsList[0].ID, logs: client.logs[5]})
	updatedReceipt, _ := m.receipts.Get(receiptsList[0].ID)
	if !strings.Contains(updatedReceipt.LogExcerpt, "fatal error: stack overflow") {
		t.Fatalf("receipt log excerpt was not updated: %q", updatedReceipt.LogExcerpt)
	}
}

func TestCommandsTasksNotificationsReceiptsReap(t *testing.T) {
	client := &experienceCaller{
		procs: []taskmanager.ProcessRow{
			{ID: 10, Name: "dead-proc", State: "stopped", ExitCode: 0},
		},
	}
	m := New(context.Background(), client, nil)

	// Test 'tasks' command switches view
	res := execute(context.Background(), client, "/users/guest", "tasks")
	m.Update(res)
	if !m.taskView {
		t.Fatal("expected taskView after 'tasks' command")
	}

	// Test 'notifications' command switches view
	resNotif := execute(context.Background(), client, "/users/guest", "notifications")
	m.Update(resNotif)
	if !m.notifView || m.taskView {
		t.Fatal("expected notifView after 'notifications' command")
	}

	// Test 'reap' command
	resReap := execute(context.Background(), client, "/users/guest", "reap")
	if !strings.Contains(resReap.text, "Reaped 1 terminated processes") {
		t.Fatalf("expected reap output, got %q", resReap.text)
	}

	// Test 'receipt' commands
	r := receipts.New(20, "job", "failed", 1, "oom", 0, "out of memory")
	m.receipts.Add(r)

	resList := execute(context.Background(), client, "/users/guest", "receipt")
	m.Update(resList)

	foundList := false
	for _, entry := range m.transcript {
		if strings.Contains(entry.text, "CRASH RECEIPTS") {
			foundList = true
			break
		}
	}
	if !foundList {
		t.Fatal("expected CRASH RECEIPTS in transcript after receipt command")
	}

	resShow := execute(context.Background(), client, "/users/guest", "receipt 20")
	m.Update(resShow)
	if !m.taskView || m.tasks.Mode() != taskmanager.ModeReceipt {
		t.Fatal("expected receipt view to open after 'receipt 20'")
	}
}
