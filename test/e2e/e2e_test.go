// Package e2e boots a complete Gostalgia environment in a temporary root
// and exercises it the way an external client would: over a real socket
// with real authentication, ending in a real clean shutdown.
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gostalgia/internal/events"
	"gostalgia/internal/ipc"
	"gostalgia/internal/runtime"
	"gostalgia/platform"
)

func TestEnvironmentSubscriptionsAndPrivateHistory(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root})
	must(t, err)
	t.Cleanup(func() { rt.Shutdown("subscription test") })
	client := dialRunning(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := client.Subscribe(ctx, ipc.SubscribeParams{Topic: "app.state"})
	must(t, err)
	private := "private-document-and-credential-value"
	must(t, client.Call(ctx, "fs/write", map[string]string{
		"path":        "/users/guest/documents/private.txt",
		"data_base64": base64.StdEncoding.EncodeToString([]byte(private)),
	}, nil))
	must(t, client.Call(ctx, "app/com.gostalgia.echo/echo", map[string]string{"msg": private}, nil))
	must(t, client.Call(ctx, "app/stop", map[string]string{"id": "com.gostalgia.echo"}, nil))
	select {
	case n := <-sub.Events:
		if n.Event.Type != "app.state" || n.Event.ID <= sub.Info.Cursor {
			t.Fatalf("live lifecycle event = %+v", n)
		}
	case <-ctx.Done():
		t.Fatal("no live app lifecycle event")
	}
	var history events.History
	must(t, client.Call(ctx, ipc.HistoryMethod, ipc.HistoryParams{Topic: "*"}, &history))
	if len(history.Events) == 0 || history.Epoch != sub.Info.Epoch {
		t.Fatal("runtime event trail not exposed")
	}
	raw, err := json.Marshal(history)
	must(t, err)
	runtimeData, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	must(t, err)
	var info struct {
		Token string `json:"token"`
	}
	must(t, json.Unmarshal(runtimeData, &info))
	for _, secret := range []string{private, base64.StdEncoding.EncodeToString([]byte(private)), info.Token, "private.txt"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("history leaked private content")
		}
	}
	must(t, sub.Close(ctx))
}

func dialRunning(t *testing.T, root string) *ipc.Client {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime.json: %v", err)
	}
	var info struct {
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
		PID      int    `json:"pid"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("parse runtime.json: %v", err)
	}
	if info.Endpoint == "" || info.Token == "" || info.PID == 0 {
		t.Fatalf("runtime.json incomplete: %+v", info)
	}
	conn, err := platform.DialIPC(info.Endpoint)
	if err != nil {
		t.Fatalf("dial %s: %v", info.Endpoint, err)
	}
	client, err := ipc.NewClient(conn, info.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestFullEnvironmentLifecycle(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root, Verbose: true})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}

	client := dialRunning(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Status.
	var status struct {
		Version  string `json:"version"`
		User     string `json:"user"`
		Session  string `json:"session"`
		Services []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"services"`
	}
	if err := client.Call(ctx, "sys/status", nil, &status); err != nil {
		t.Fatalf("sys/status: %v", err)
	}
	if status.Version != "0.1.0" {
		t.Errorf("version = %q", status.Version)
	}
	if status.User != "guest" || status.Session == "" {
		t.Errorf("session state = user:%q session:%q", status.User, status.Session)
	}
	if len(status.Services) != 15 {
		t.Errorf("services = %d, want 15", len(status.Services))
	}

	// 2. Talk to the launched application over the socket.
	var echoOut struct {
		Msg    string `json:"msg"`
		Echoes int64  `json:"echoes"`
	}
	if err := client.Call(ctx, "app/com.gostalgia.echo/echo", map[string]string{"msg": "end to end"}, &echoOut); err != nil {
		t.Fatalf("echo call: %v", err)
	}
	if echoOut.Msg != "end to end" || echoOut.Echoes != 1 {
		t.Fatalf("echo response = %+v", echoOut)
	}

	// 3. Filesystem round trip over the socket, verified on host disk.
	payload := "persisted through the environment VFS"
	must(t, client.Call(ctx, "fs/write", map[string]string{
		"path":        "/users/guest/documents/e2e.txt",
		"data_base64": base64.StdEncoding.EncodeToString([]byte(payload)),
	}, nil))
	var readOut struct {
		Data string `json:"data_base64"`
	}
	must(t, client.Call(ctx, "fs/read", map[string]string{"path": "/users/guest/documents/e2e.txt"}, &readOut))
	decoded, err := base64.StdEncoding.DecodeString(readOut.Data)
	must(t, err)
	if string(decoded) != payload {
		t.Fatalf("vfs round trip mismatch: %q", decoded)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "vfs", "users", "guest", "documents", "e2e.txt"))
	if err != nil {
		t.Fatalf("file missing on host: %v", err)
	}
	if string(onDisk) != payload {
		t.Fatalf("host file = %q, want %q", onDisk, payload)
	}

	// 4. Single-instance policy via IPC.
	if err := client.Call(ctx, "app/launch", map[string]string{"id": "com.gostalgia.echo"}, nil); err == nil {
		t.Fatal("duplicate launch over IPC succeeded, want error")
	}

	// 5. Process listing shows the running app.
	var procs []struct {
		ID    int32  `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	}
	must(t, client.Call(ctx, "proc/list", nil, &procs))
	running := false
	var echoPID int32
	for _, p := range procs {
		if p.Name == "com.gostalgia.echo" && p.State == "running" {
			running = true
			echoPID = p.ID
		}
	}
	if !running {
		t.Fatalf("echo not running per proc/list: %+v", procs)
	}

	// 5b. Query proc/logs and proc/info for the running app.
	var logs struct {
		ID       int32  `json:"id"`
		Name     string `json:"name"`
		State    string `json:"state"`
		Duration string `json:"duration"`
	}
	must(t, client.Call(ctx, "proc/logs", map[string]any{"id": echoPID}, &logs))
	if logs.ID != echoPID || logs.Name != "com.gostalgia.echo" || logs.Duration == "" {
		t.Fatalf("proc/logs response unexpected: %+v", logs)
	}

	var info struct {
		ID   int32  `json:"id"`
		Name string `json:"name"`
	}
	must(t, client.Call(ctx, "proc/info", map[string]any{"id": echoPID}, &info))
	if info.ID != echoPID || info.Name != "com.gostalgia.echo" {
		t.Fatalf("proc/info response unexpected: %+v", info)
	}

	// 6. Clean shutdown requested over IPC.
	shutdownDone := make(chan struct{})
	go func() { rt.Wait(); close(shutdownDone) }()
	var resp struct {
		Stopping bool `json:"stopping"`
	}
	if err := client.Call(ctx, "sys/shutdown", map[string]string{"reason": "e2e"}, &resp); err != nil {
		t.Logf("shutdown call transport error (acceptable during teardown): %v", err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(15 * time.Second):
		t.Fatal("runtime did not shut down")
	}

	// 7. Post-shutdown invariants.
	if _, err := os.Stat(filepath.Join(root, "runtime.json")); !os.IsNotExist(err) {
		t.Error("runtime.json was not removed at shutdown")
	}
	if len(rt.Sessions.Active()) != 0 {
		t.Error("session was not closed at shutdown")
	}
	if len(rt.Apps.Running()) != 0 {
		t.Error("application was not stopped at shutdown")
	}
}

func TestBootRejectsSecondInstance(t *testing.T) {
	root := t.TempDir()
	first, err := runtime.Boot(context.Background(), runtime.Options{Root: root})
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	defer first.Shutdown("test cleanup")

	if _, err := runtime.Boot(context.Background(), runtime.Options{Root: root}); err == nil {
		t.Fatal("second boot on a live root succeeded, want error")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
