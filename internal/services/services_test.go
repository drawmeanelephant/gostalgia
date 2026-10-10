package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gostalgia/apps"
	"gostalgia/internal/app"
	"gostalgia/internal/config"
	"gostalgia/internal/events"
	"gostalgia/internal/ipc"
	"gostalgia/internal/process"
	"gostalgia/internal/profile"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/session"
	"gostalgia/internal/vfs"
	"gostalgia/platform"
)

// testEnv is a full core-service stack without sockets or a real boot.
type testEnv struct {
	t      *testing.T
	ctx    *service.Context
	router *ipc.Router
	sm     *service.Manager
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	bus := events.NewBus()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := ipc.NewRouter()

	// The real runtime creates the layout in InitRoot before opening
	// the host-backed root; mirror that here.
	must(t, os.MkdirAll(filepath.Join(root, "vfs"), 0o755))
	hostFS, err := vfs.NewHost(filepath.Join(root, "vfs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hostFS.Close() })
	env := vfs.New(hostFS)
	must(t, env.Mount("/tmp", vfs.NewMem()))
	for _, dir := range []string{"/users/guest/documents", "/apps/manifests", "/apps/data"} {
		must(t, env.MkdirAll(dir))
	}

	cfg, err := config.Load(filepath.Join(root, "config", "system.json"))
	must(t, err)

	procs := process.NewManager(bus, log)
	sessions := session.NewManager(bus, log)
	sessions.SetVFS(env)
	profilesMgr, err := profile.NewManager(env, "", bus, log)
	must(t, err)
	registry := app.NewRegistry()
	must(t, apps.Register(registry))
	appMgr := app.NewManager(registry, procs, router, bus, log)
	appMgr.SetGrantStore(env.Grants())

	policyStore := security.NewPolicyStore(security.DefaultOperatorPolicy())
	sctx := &service.Context{
		Root:     root,
		Version:  "test",
		Config:   cfg,
		Events:   bus,
		Log:      log,
		Router:   router,
		VFS:      env,
		Procs:    procs,
		Apps:     appMgr,
		Sessions: sessions,
		Profiles: profilesMgr,
		Token:    "test-token",
		Policy:   policyStore,
		BootedAt: time.Now(),
		Shutdown: func(reason string) {},
	}
	sm := service.NewManager(sctx, bus, log)
	sctx.Services = sm
	must(t, sm.Register(NewSys()))
	must(t, sm.Register(NewProc()))
	must(t, sm.Register(NewFS()))
	must(t, sm.Register(NewDocument()))
	must(t, sm.Register(NewClipboard()))
	must(t, sm.Register(NewNet()))
	must(t, sm.Register(NewSession()))
	must(t, sm.Register(NewProfile()))
	must(t, sm.Register(NewRecovery()))
	must(t, sm.Register(NewSound()))
	must(t, sm.StartAll(context.Background()))
	t.Cleanup(func() { _ = sm.StopAll(context.Background()) })
	// Mirror the runtime: a default user session exists.
	_, err = sessions.Create(security.User{ID: "u-guest", Name: "guest"})
	must(t, err)
	return &testEnv{t: t, ctx: sctx, router: router, sm: sm}
}

func (e *testEnv) call(ctx context.Context, caps *security.Capabilities, method string, params any) ipc.Response {
	e.t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			e.t.Fatal(err)
		}
		raw = b
	}
	if caps != nil {
		ctx = ipc.WithCapabilities(ctx, caps)
	}
	return e.router.Dispatch(ctx, ipc.Request{ID: 1, Method: method, Params: raw})
}

func (e *testEnv) callAs(ctx context.Context, principal security.Principal, caps *security.Capabilities, method string, params any) ipc.Response {
	e.t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			e.t.Fatal(err)
		}
		raw = b
	}
	if caps != nil {
		ctx = ipc.WithCapabilities(ctx, caps)
	}
	ctx = ipc.WithPrincipal(ctx, principal)
	return e.router.Dispatch(ctx, ipc.Request{ID: 1, Method: method, Params: raw})
}

func TestSysStatus(t *testing.T) {
	env := newTestEnv(t)
	resp := env.call(context.Background(), security.AdminCapabilities(), "sys/status", nil)
	if !resp.OK {
		t.Fatalf("sys/status failed: %s", resp.Error)
	}
	var status struct {
		Version  string `json:"version"`
		Services []struct {
			Name  string        `json:"name"`
			State service.State `json:"state"`
		} `json:"services"`
		Apps     []app.Status                      `json:"apps"`
		Security platform.HostSecurityCapabilities `json:"security"`
	}
	must(t, json.Unmarshal(resp.Data, &status))
	if status.Security.Platform == "" {
		t.Errorf("expected security platform to be populated, got %+v", status.Security)
	}
	if status.Version != "test" {
		t.Errorf("version = %q, want test", status.Version)
	}
	if len(status.Services) != 10 {
		t.Errorf("services = %d, want 10", len(status.Services))
	}
	if len(status.Apps) != len(apps.Manifests()) {
		t.Errorf("apps count = %d, want %d", len(status.Apps), len(apps.Manifests()))
	}
	appIDs := make(map[string]bool)
	for _, a := range status.Apps {
		appIDs[a.Manifest.ID] = true
	}
	for _, id := range []string{"com.gostalgia.echo", "com.gostalgia.notes", "com.gostalgia.files", "com.gostalgia.settings"} {
		if !appIDs[id] {
			t.Errorf("expected app %s in status.Apps, got %+v", id, status.Apps)
		}
	}
}

func TestSysShutdownRequiresCapability(t *testing.T) {
	env := newTestEnv(t)
	resp := env.call(context.Background(), security.NewCapabilities(security.CapIPC), "sys/shutdown", nil)
	if resp.OK {
		t.Fatal("shutdown without the shutdown capability succeeded")
	}
	if resp.Error == "" {
		t.Fatal("shutdown denial has no error message")
	}
}

// The version-3 push channel: only an app principal may invalidate, and the
// published app.view envelope attributes the signal to that app's own id.
func TestAppInvalidatePublishesOwnViewEvent(t *testing.T) {
	env := newTestEnv(t)
	got := make(chan events.Envelope, 1)
	cancel := env.ctx.Events.Subscribe("app.view", func(e events.Envelope) { got <- e })
	defer cancel()

	appP := security.AppPrincipal("com.gostalgia.pomodoro", 42, "sess-1", security.User{Name: "guest"})
	resp := env.callAs(context.Background(), appP, security.NewCapabilities(security.CapIPC), "app/invalidate", nil)
	if !resp.OK {
		t.Fatalf("app/invalidate rejected: %s", resp.Error)
	}
	select {
	case e := <-got:
		ve, ok := e.Payload.(app.ViewEvent)
		if !ok || e.Type != "app.view" || e.Source != "app/com.gostalgia.pomodoro" ||
			ve.ID != "com.gostalgia.pomodoro" || ve.PID != 42 {
			t.Fatalf("unexpected view event envelope: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no app.view event published")
	}

	// An operator (or any non-app principal) cannot fake an app push.
	op := security.OperatorPrincipal(security.User{Name: "operator"})
	resp = env.callAs(context.Background(), op, security.AdminCapabilities(), "app/invalidate", nil)
	if resp.OK || !strings.Contains(resp.Error, "only applications") {
		t.Fatalf("operator invalidate = %+v, want app-only denial", resp)
	}
	// The route still requires the ipc capability.
	resp = env.callAs(context.Background(), appP, security.NewCapabilities(), "app/invalidate", nil)
	if resp.OK || !strings.Contains(resp.Error, "missing capability") {
		t.Fatalf("capability-less invalidate = %+v, want denial", resp)
	}
}

func TestWhoamiReportsCallerCapabilities(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()
	resp := env.call(context.Background(), admin, "session/whoami", nil)
	if !resp.OK {
		t.Fatalf("whoami failed: %s", resp.Error)
	}
	var out struct {
		User         string   `json:"user"`
		Capabilities []string `json:"capabilities"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	if out.User != "guest" {
		t.Errorf("user = %q, want guest", out.User)
	}
	if len(out.Capabilities) == 0 {
		t.Error("capabilities list is empty")
	}
}

func TestFSWriteReadRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	payload := []byte("hello from the fs service")
	encoded := base64.StdEncoding.EncodeToString(payload)
	resp := env.call(context.Background(), admin, "fs/write", map[string]string{
		"path":        "/users/guest/documents/note.txt",
		"data_base64": encoded,
	})
	if !resp.OK {
		t.Fatalf("fs/write failed: %s", resp.Error)
	}

	resp = env.call(context.Background(), admin, "fs/read", map[string]string{"path": "/users/guest/documents/note.txt"})
	if !resp.OK {
		t.Fatalf("fs/read failed: %s", resp.Error)
	}
	var out struct {
		Data string `json:"data_base64"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	decoded, err := base64.StdEncoding.DecodeString(out.Data)
	must(t, err)
	if string(decoded) != string(payload) {
		t.Fatalf("round trip mismatch: %q != %q", decoded, payload)
	}

	// The write landed in the host-backed root.
	host, err := os.ReadFile(filepath.Join(env.ctx.Root, "vfs", "users/guest/documents/note.txt"))
	must(t, err)
	if string(host) != string(payload) {
		t.Fatalf("host copy = %q, want %q", host, payload)
	}
}

func TestFSWriteDeniedWithoutCapability(t *testing.T) {
	env := newTestEnv(t)
	readOnly := security.NewCapabilities(security.CapFileRead)
	resp := env.call(context.Background(), readOnly, "fs/write", map[string]string{
		"path":        "/users/guest/documents/nope.txt",
		"data_base64": "",
	})
	if resp.OK {
		t.Fatal("fs/write succeeded without the fs.write capability")
	}
}

func TestFSEscapeRejected(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()
	resp := env.call(context.Background(), admin, "fs/write", map[string]string{
		"path":        "/../escaped.txt",
		"data_base64": "",
	})
	if resp.OK {
		t.Fatal("fs/write escaped the environment root")
	}
}

func TestProcListAndStop(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	proc, err := env.ctx.Apps.Launch(ctx, "com.gostalgia.echo")
	must(t, err)

	resp := env.call(context.Background(), admin, "proc/list", nil)
	if !resp.OK {
		t.Fatalf("proc/list failed: %s", resp.Error)
	}
	var procs []struct {
		ID    int32  `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	}
	must(t, json.Unmarshal(resp.Data, &procs))
	found := false
	for _, p := range procs {
		if p.ID == proc.ID() && p.Name == "com.gostalgia.echo" && p.State == "running" {
			found = true
		}
	}
	if !found {
		t.Fatalf("echo process missing from proc/list: %+v", procs)
	}

	resp = env.call(context.Background(), admin, "proc/stop", map[string]int{"id": int(proc.ID())})
	if !resp.OK {
		t.Fatalf("proc/stop failed: %s", resp.Error)
	}
	select {
	case <-proc.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("echo process did not exit after proc/stop")
	}
}

func TestAppLaunchViaIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	// The runtime already launches echo at boot; in the test env it is
	// not launched, so launching here must succeed once, then fail.
	resp := env.call(context.Background(), admin, "app/launch", map[string]string{"id": "com.gostalgia.echo"})
	if !resp.OK {
		t.Fatalf("app/launch failed: %s", resp.Error)
	}
	resp = env.call(context.Background(), admin, "app/launch", map[string]string{"id": "com.gostalgia.echo"})
	if resp.OK {
		t.Fatal("second launch succeeded, single-instance policy broken")
	}
	if err := env.ctx.Apps.Stop("com.gostalgia.echo", 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestAppControlPermissionsAndRequestLifetime(t *testing.T) {
	env := newTestEnv(t)
	for _, method := range []string{"app/launch", "app/stop"} {
		resp := env.call(context.Background(), security.NewCapabilities(security.CapIPC), method, map[string]string{"id": "com.gostalgia.echo"})
		if resp.OK {
			t.Fatalf("%s without capability succeeded", method)
		}
	}
	request, cancel := context.WithCancel(context.Background())
	resp := env.call(request, security.AdminCapabilities(), "app/launch", map[string]string{"id": "com.gostalgia.echo"})
	if !resp.OK {
		t.Fatal(resp.Error)
	}
	cancel() // A client's request/disconnection cannot own the app lifetime.
	resp = env.call(context.Background(), security.AdminCapabilities(), "app/com.gostalgia.echo/identity", nil)
	if !resp.OK {
		t.Fatal(resp.Error)
	}
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	if len(out.Capabilities) != 1 || out.Capabilities[0] != security.CapIPC {
		t.Fatalf("caps = %v", out.Capabilities)
	}
	resp = env.call(context.Background(), security.AdminCapabilities(), "app/stop", map[string]string{"id": "com.gostalgia.echo"})
	if !resp.OK || env.ctx.Apps.IsRunning("com.gostalgia.echo") {
		t.Fatalf("stop = %+v", resp)
	}
}

func TestFSMkdirAndRemoveOverIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	resp := env.call(context.Background(), admin, "fs/mkdir", map[string]string{"path": "/users/guest/documents/newdir"})
	if !resp.OK {
		t.Fatalf("fs/mkdir failed: %s", resp.Error)
	}
	info, err := env.ctx.VFS.Stat("/users/guest/documents/newdir")
	if err != nil || !info.IsDir() {
		t.Fatalf("mkdir result visible in VFS: info=%v err=%v", info, err)
	}

	// A non-empty directory cannot be removed.
	resp = env.call(context.Background(), admin, "fs/write", map[string]string{
		"path": "/users/guest/documents/newdir/keep.txt",
	})
	if !resp.OK {
		t.Fatalf("fs/write into new dir failed: %s", resp.Error)
	}
	resp = env.call(context.Background(), admin, "fs/remove", map[string]string{"path": "/users/guest/documents/newdir"})
	if resp.OK {
		t.Fatal("fs/remove succeeded on a non-empty directory, want error")
	}

	resp = env.call(context.Background(), admin, "fs/remove", map[string]string{"path": "/users/guest/documents/newdir/keep.txt"})
	if !resp.OK {
		t.Fatalf("fs/remove of file failed: %s", resp.Error)
	}
	resp = env.call(context.Background(), admin, "fs/remove", map[string]string{"path": "/users/guest/documents/newdir"})
	if !resp.OK {
		t.Fatalf("fs/remove of emptied dir failed: %s", resp.Error)
	}
	if _, err := env.ctx.VFS.Stat("/users/guest/documents/newdir"); err == nil {
		t.Fatal("removed directory still visible in VFS")
	}

	// Missing path is an error, not a silent success.
	resp = env.call(context.Background(), admin, "fs/remove", map[string]string{"path": "/users/guest/documents/ghost"})
	if resp.OK {
		t.Fatal("fs/remove of missing path succeeded, want error")
	}
}

func TestFSMkdirAndRemoveDeniedWithoutCapability(t *testing.T) {
	env := newTestEnv(t)
	readOnly := security.NewCapabilities(security.CapFileRead)

	resp := env.call(context.Background(), readOnly, "fs/mkdir", map[string]string{"path": "/users/guest/documents/denied"})
	if resp.OK {
		t.Fatal("fs/mkdir succeeded without the fs.write capability")
	}
	if resp := env.call(context.Background(), readOnly, "fs/remove", map[string]string{"path": "/users/guest/documents"}); resp.OK {
		t.Fatal("fs/remove succeeded without the fs.write capability")
	}
	// And neither call had any effect.
	if _, err := env.ctx.VFS.Stat("/users/guest/documents/denied"); err == nil {
		t.Fatal("denied mkdir still created the directory")
	}
}

func TestProcLogsViaIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	proc, err := env.ctx.Apps.Launch(context.Background(), "com.gostalgia.echo")
	must(t, err)
	defer func() { _ = env.ctx.Apps.Stop("com.gostalgia.echo", time.Second) }()

	// 1. Full logs query with admin cap
	resp := env.call(context.Background(), admin, "proc/logs", map[string]any{"id": proc.ID()})
	if !resp.OK {
		t.Fatalf("proc/logs failed: %s", resp.Error)
	}
	var logs process.Logs
	must(t, json.Unmarshal(resp.Data, &logs))
	if logs.ID != proc.ID() {
		t.Fatalf("logs.ID = %d, want %d", logs.ID, proc.ID())
	}
	if logs.Name != "com.gostalgia.echo" {
		t.Fatalf("logs.Name = %q, want com.gostalgia.echo", logs.Name)
	}
	if logs.State != process.StateRunning {
		t.Fatalf("logs.State = %q, want running", logs.State)
	}
	if logs.Duration == "" {
		t.Fatal("logs.Duration is empty")
	}

	// 2. Stream filtering
	resp = env.call(context.Background(), admin, "proc/logs", map[string]any{
		"id":     proc.ID(),
		"stream": "stdout",
	})
	if !resp.OK {
		t.Fatalf("proc/logs with stream failed: %s", resp.Error)
	}
	var stdoutResp struct {
		ID     int32                      `json:"id"`
		Stdout process.StreamDiagnostics  `json:"stdout"`
		Stderr *process.StreamDiagnostics `json:"stderr"`
	}
	must(t, json.Unmarshal(resp.Data, &stdoutResp))
	if stdoutResp.ID != proc.ID() || stdoutResp.Stderr != nil {
		t.Fatalf("expected only stdout stream, got: %+v", stdoutResp)
	}

	// 3. Tail parameter
	resp = env.call(context.Background(), admin, "proc/logs", map[string]any{
		"id":   proc.ID(),
		"tail": 5,
	})
	if !resp.OK {
		t.Fatalf("proc/logs with tail failed: %s", resp.Error)
	}

	// 4. Missing capability check
	noProcList := security.NewCapabilities(security.CapIPC)
	resp = env.call(context.Background(), noProcList, "proc/logs", map[string]any{"id": proc.ID()})
	if resp.OK {
		t.Fatal("proc/logs succeeded without CapProcList capability")
	}

	// 5. Unknown PID
	resp = env.call(context.Background(), admin, "proc/logs", map[string]any{"id": 99999})
	if resp.OK {
		t.Fatal("proc/logs succeeded for unknown PID, want error")
	}

	// 6. Missing PID parameter
	resp = env.call(context.Background(), admin, "proc/logs", map[string]any{})
	if resp.OK {
		t.Fatal("proc/logs succeeded without PID, want error")
	}
}

func TestProcInfoViaIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	proc, err := env.ctx.Apps.Launch(context.Background(), "com.gostalgia.echo")
	must(t, err)
	defer func() { _ = env.ctx.Apps.Stop("com.gostalgia.echo", time.Second) }()

	resp := env.call(context.Background(), admin, "proc/info", map[string]any{"id": proc.ID()})
	if !resp.OK {
		t.Fatalf("proc/info failed: %s", resp.Error)
	}
	var info process.Info
	must(t, json.Unmarshal(resp.Data, &info))
	if info.ID != proc.ID() {
		t.Fatalf("info.ID = %d, want %d", info.ID, proc.ID())
	}
	if info.Name != "com.gostalgia.echo" {
		t.Fatalf("info.Name = %s, want com.gostalgia.echo", info.Name)
	}

	noProcList := security.NewCapabilities(security.CapIPC)
	resp = env.call(context.Background(), noProcList, "proc/info", map[string]any{"id": proc.ID()})
	if resp.OK {
		t.Fatal("proc/info succeeded without CapProcList capability")
	}

	resp = env.call(context.Background(), admin, "proc/info", map[string]any{"id": 99999})
	if resp.OK {
		t.Fatal("proc/info succeeded for unknown PID, want error")
	}
}

func TestProcHistoryAndReapViaIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	proc, err := env.ctx.Apps.Launch(context.Background(), "com.gostalgia.echo")
	must(t, err)
	pid := proc.ID()

	// Stop the app
	must(t, env.ctx.Apps.Stop("com.gostalgia.echo", time.Second))

	// 1. proc/history with CapProcList
	resp := env.call(context.Background(), admin, "proc/history", nil)
	if !resp.OK {
		t.Fatalf("proc/history failed: %s", resp.Error)
	}
	var history []process.HistoryEntry
	must(t, json.Unmarshal(resp.Data, &history))
	if len(history) == 0 {
		t.Fatal("expected at least 1 history entry")
	}
	found := false
	for _, h := range history {
		if h.ID == pid {
			found = true
			if h.Name != "com.gostalgia.echo" {
				t.Fatalf("history entry name = %s, want com.gostalgia.echo", h.Name)
			}
			if h.State != process.StateStopped {
				t.Fatalf("history entry state = %s, want stopped", h.State)
			}
			break
		}
	}
	if !found {
		t.Fatalf("pid %d not found in proc/history", pid)
	}

	// 2. proc/history permission denial
	noCap := security.NewCapabilities(security.CapIPC)
	resp = env.call(context.Background(), noCap, "proc/history", nil)
	if resp.OK {
		t.Fatal("proc/history succeeded without CapProcList")
	}

	// 3. proc/reap permission denial
	resp = env.call(context.Background(), noCap, "proc/reap", nil)
	if resp.OK {
		t.Fatal("proc/reap succeeded without CapProcStop")
	}

	// 4. proc/reap with admin
	resp = env.call(context.Background(), admin, "proc/reap", nil)
	if !resp.OK {
		t.Fatalf("proc/reap failed: %s", resp.Error)
	}
	var reapRes struct {
		Reaped int `json:"reaped"`
	}
	must(t, json.Unmarshal(resp.Data, &reapRes))
	if reapRes.Reaped < 1 {
		t.Fatalf("expected at least 1 process reaped, got %d", reapRes.Reaped)
	}

	// 5. proc/info for reaped process should return from history
	resp = env.call(context.Background(), admin, "proc/info", map[string]any{"id": pid})
	if !resp.OK {
		t.Fatalf("proc/info for reaped process failed: %s", resp.Error)
	}
	var info process.HistoryEntry
	must(t, json.Unmarshal(resp.Data, &info))
	if info.ID != pid {
		t.Fatalf("info.ID = %d, want %d", info.ID, pid)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
