package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goRuntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gostalgia/internal/app"
	"gostalgia/internal/process"
	"gostalgia/internal/runtime"
	"gostalgia/platform"
	"gostalgia/sdk"
)

var (
	buildAdvOnce sync.Once
	advBinPath   string
	buildAdvErr  error
)

func getAdversarialBinary(t *testing.T) string {
	t.Helper()
	buildAdvOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "adv-bin-*")
		if err != nil {
			buildAdvErr = err
			return
		}
		bin := filepath.Join(tmpDir, "adversarial-app")
		if goRuntime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "gostalgia/test/testapps/adversarial")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildAdvErr = fmt.Errorf("build adversarial-app: %w\n%s", err, string(out))
			return
		}
		advBinPath = bin
	})
	if buildAdvErr != nil {
		t.Fatalf("failed to build adversarial test binary: %v", buildAdvErr)
	}
	return advBinPath
}

func TestSandboxIsolationE2E(t *testing.T) {
	caps := platform.GetHostSecurityCapabilities()
	if !caps.Supported {
		t.Skipf("sandbox execution not supported on this host: %s", caps.Reason)
	}

	bin := getAdversarialBinary(t)

	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root})
	must(t, err)
	t.Cleanup(func() { rt.Shutdown("sandbox e2e test") })

	sandboxedMan := app.Manifest{
		ID:              "com.test.sandboxed",
		Name:            "Sandboxed App",
		Version:         "1.0.0",
		Mode:            sdk.ModeExternal,
		Executable:      bin,
		Isolation:       sdk.IsolationSandbox,
		Permissions:     []string{sdk.CapIPC},
		ProtocolVersion: 1,
	}
	must(t, rt.Apps.Registry().RegisterExternal(sandboxedMan))

	strictMan := app.Manifest{
		ID:              "com.test.strict",
		Name:            "Strict App",
		Version:         "1.0.0",
		Mode:            sdk.ModeExternal,
		Executable:      bin,
		Isolation:       sdk.IsolationStrict,
		Permissions:     []string{sdk.CapIPC},
		ProtocolVersion: 1,
	}
	must(t, rt.Apps.Registry().RegisterExternal(strictMan))

	trustedMan := app.Manifest{
		ID:              "com.test.trusted",
		Name:            "Trusted App",
		Version:         "1.0.0",
		Mode:            sdk.ModeExternal,
		Executable:      bin,
		Isolation:       sdk.IsolationTrusted,
		Permissions:     []string{sdk.CapIPC},
		ProtocolVersion: 1,
	}
	must(t, rt.Apps.Registry().RegisterExternal(trustedMan))

	client := dialRunning(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. Verify sys/status reports HostSecurityCapabilities
	var status struct {
		Security platform.HostSecurityCapabilities `json:"security"`
	}
	must(t, client.Call(ctx, "sys/status", nil, &status))
	if !status.Security.Supported {
		t.Fatalf("expected host security supported = true in sys/status")
	}

	// 2. Launch sandboxed app
	var launchRes struct {
		ID  string `json:"id"`
		PID int32  `json:"pid"`
	}
	must(t, client.Call(ctx, "app/launch", map[string]string{"id": sandboxedMan.ID}, &launchRes))
	if launchRes.PID <= 0 {
		t.Fatalf("unexpected launch PID %d", launchRes.PID)
	}

	// Verify isolation in proc/info
	var procInfo process.Info
	must(t, client.Call(ctx, "proc/info", map[string]any{"id": launchRes.PID}, &procInfo))
	if procInfo.Isolation != sdk.IsolationSandbox {
		t.Fatalf("proc/info isolation = %q, want %q", procInfo.Isolation, sdk.IsolationSandbox)
	}
	if procInfo.Policy == nil || !procInfo.Policy.DenyNetwork {
		t.Fatalf("proc/info policy expected DenyNetwork=true, got %+v", procInfo.Policy)
	}

	// Verify network access is denied for sandboxed app
	var netRes struct {
		Connected bool   `json:"connected"`
		Error     string `json:"error"`
	}
	must(t, client.Call(ctx, "app/com.test.sandboxed/probe_network", map[string]string{"target": "1.1.1.1:80"}, &netRes))
	if netRes.Connected {
		t.Fatalf("sandboxed app unexpectedly connected to host network!")
	}
	if !strings.Contains(netRes.Error, "network is unreachable") && !strings.Contains(netRes.Error, "unreachable") && !strings.Contains(netRes.Error, "operation not permitted") {
		t.Logf("network error detail: %s", netRes.Error)
	}

	// Sandboxed children must also be unable to reach remote unix sockets
	// (#83): their IPC channel is an inherited descriptor, so the sandbox
	// grants no connect() capability at all. Probe the environment's own
	// IPC socket — reachable by path from a trusted process.
	if goRuntime.GOOS != "windows" {
		var envInfo struct {
			Endpoint string `json:"endpoint"`
		}
		if raw, err := os.ReadFile(filepath.Join(root, "runtime.json")); err == nil {
			_ = json.Unmarshal(raw, &envInfo)
		}
		if strings.HasPrefix(envInfo.Endpoint, "unix://") {
			var unixRes struct {
				Connected bool   `json:"connected"`
				Error     string `json:"error"`
			}
			must(t, client.Call(ctx, "app/com.test.sandboxed/probe_network", map[string]string{
				"network": "unix",
				"target":  strings.TrimPrefix(envInfo.Endpoint, "unix://"),
			}, &unixRes))
			if unixRes.Connected {
				t.Fatal("sandboxed app connected to the runtime's unix socket (remote unix-socket was allowed)")
			}
			t.Logf("unix socket connect denied: %s", unixRes.Error)

			// The socket lives in the runtime's private IPC directory, which
			// is masked for sandboxed children: a same-uid app must not be
			// able to write into it (e.g. to replace or unlink the socket).
			var writeSockRes struct {
				Written bool   `json:"written"`
				Error   string `json:"error"`
			}
			target := filepath.Join(filepath.Dir(strings.TrimPrefix(envInfo.Endpoint, "unix://")), "probe")
			must(t, client.Call(ctx, "app/com.test.sandboxed/probe_write", map[string]string{"path": target}, &writeSockRes))
			if writeSockRes.Written {
				t.Fatalf("sandboxed app wrote into the IPC socket directory %s", target)
			}
			t.Logf("IPC socket dir write denied: %s", writeSockRes.Error)
		}
	}

	// Verify filesystem write to host root is rejected
	var writeRes struct {
		Written bool   `json:"written"`
		Error   string `json:"error"`
	}
	must(t, client.Call(ctx, "app/com.test.sandboxed/probe_write", map[string]string{"path": "/etc/gostalgia_forbidden.txt"}, &writeRes))
	if writeRes.Written {
		t.Fatalf("sandboxed app unexpectedly wrote to host /etc!")
	}

	must(t, client.Call(ctx, "app/stop", map[string]string{"id": sandboxedMan.ID}, nil))

	// 3. Launch strict app
	var strictLaunch struct {
		ID  string `json:"id"`
		PID int32  `json:"pid"`
	}
	must(t, client.Call(ctx, "app/launch", map[string]string{"id": strictMan.ID}, &strictLaunch))

	var strictProcInfo process.Info
	must(t, client.Call(ctx, "proc/info", map[string]any{"id": strictLaunch.PID}, &strictProcInfo))
	if strictProcInfo.Isolation != sdk.IsolationStrict {
		t.Fatalf("strict proc/info isolation = %q, want %q", strictProcInfo.Isolation, sdk.IsolationStrict)
	}

	// Verify network access is denied for strict app
	must(t, client.Call(ctx, "app/com.test.strict/probe_network", map[string]string{"target": "1.1.1.1:80"}, &netRes))
	if netRes.Connected {
		t.Fatalf("strict app unexpectedly connected to host network!")
	}

	// Verify descendant creation fails under strict mode
	var spawnRes struct {
		Spawned bool   `json:"spawned"`
		Error   string `json:"error"`
	}
	if err := client.Call(ctx, "app/com.test.strict/spawn_descendant", nil, &spawnRes); err != nil {
		var logs process.Logs
		_ = client.Call(ctx, "proc/logs", map[string]any{"id": strictLaunch.PID}, &logs)
		t.Fatalf("spawn_descendant call failed: %v (stderr: %q)", err, logs.Stderr.Content)
	}
	if spawnRes.Spawned {
		t.Fatalf("strict app unexpectedly spawned a descendant process!")
	}
	t.Logf("strict descendant spawn error: %s", spawnRes.Error)

	must(t, client.Call(ctx, "app/stop", map[string]string{"id": strictMan.ID}, nil))

	// 4. Launch trusted app and verify it CAN spawn descendant
	var trustedLaunch struct {
		ID  string `json:"id"`
		PID int32  `json:"pid"`
	}
	must(t, client.Call(ctx, "app/launch", map[string]string{"id": trustedMan.ID}, &trustedLaunch))
	must(t, client.Call(ctx, "app/com.test.trusted/spawn_descendant", nil, &spawnRes))
	if !spawnRes.Spawned {
		t.Fatalf("trusted app failed to spawn descendant: %s", spawnRes.Error)
	}
	must(t, client.Call(ctx, "app/stop", map[string]string{"id": trustedMan.ID}, nil))
}

func TestSandboxFailClosedUnsupported(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root})
	must(t, err)
	t.Cleanup(func() { rt.Shutdown("sandbox fail closed test") })

	// Register an external app with an unresolvable isolation level
	badMan := app.Manifest{
		ID:              "com.test.badiso",
		Name:            "Bad Isolation App",
		Version:         "1.0.0",
		Mode:            sdk.ModeExternal,
		Executable:      "/bin/true",
		Permissions:     []string{sdk.CapIPC},
		ProtocolVersion: 1,
	}
	// Manifest validation will reject unknown isolation levels at parse/register time
	badMan.Isolation = "completely-unknown"
	if err := rt.Apps.Registry().RegisterExternal(badMan); err == nil {
		t.Fatal("expected RegisterExternal with unknown isolation to fail")
	}

	caps := platform.GetHostSecurityCapabilities()
	if !caps.Supported {
		// On an unsupported platform/host, declaring sandbox or strict fails closed at launch time
		bin := "/bin/true"
		if goRuntime.GOOS == "windows" {
			bin = "cmd.exe"
		}
		unsupportedMan := app.Manifest{
			ID:              "com.test.unsupported.sandbox",
			Name:            "Unsupported Sandbox App",
			Version:         "1.0.0",
			Mode:            sdk.ModeExternal,
			Executable:      bin,
			Isolation:       sdk.IsolationSandbox,
			Permissions:     []string{sdk.CapIPC},
			ProtocolVersion: 1,
		}
		must(t, rt.Apps.Registry().RegisterExternal(unsupportedMan))
		client := dialRunning(t, root)
		var res any
		err := client.Call(context.Background(), "app/launch", map[string]string{"id": unsupportedMan.ID}, &res)
		if err == nil {
			t.Fatal("expected app/launch to fail closed when sandbox is unsupported on host")
		}
	}
}
