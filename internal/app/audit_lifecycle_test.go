package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gostalgia/internal/events"
	"gostalgia/internal/ipc"
	"gostalgia/internal/process"
	"gostalgia/internal/security"
	"gostalgia/sdk"
)

// TestHungHandlerDoesNotWedgeManager is the regression for a permanently hung
// in-proc route handler wedging the app manager: route retraction waits are
// bounded, so Stop unblocks and the app is forgotten.
func TestHungHandlerDoesNotWedgeManager(t *testing.T) {
	entered := make(chan struct{})
	p := &probe{init: func(c *sdk.Context) error {
		return c.Handle("hang", func(ctx context.Context, _ json.RawMessage) (any, error) {
			close(entered)
			select {} // permanently hung, ignores ctx
		})
	}}
	m, r := probeManager(t, p, sdk.CapIPC)
	_, err := m.Launch(context.Background(), fakeManifest.ID)
	must(t, err)

	go func() {
		_ = r.Dispatch(ipc.WithCapabilities(context.Background(), security.AdminCapabilities()),
			ipc.Request{Method: "app/com.test.fake/hang"})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("hung handler never ran")
	}

	// Stop is allowed to report an error, but must not block past its timeout.
	stopErr := m.Stop(fakeManifest.ID, 500*time.Millisecond)
	t.Logf("Stop returned: %v", stopErr)

	// Once the bounded retraction elapses, the wedged app is forgotten and
	// the manager is usable again.
	deadline := time.Now().Add(10 * time.Second)
	for m.IsRunning(fakeManifest.ID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if m.IsRunning(fakeManifest.ID) {
		t.Fatal("hung handler permanently wedged the app manager")
	}
	proc, err := m.Launch(context.Background(), fakeManifest.ID)
	if err != nil {
		t.Fatalf("relaunch after unwedging failed: %v", err)
	}
	must(t, m.Stop(fakeManifest.ID, 2*time.Second))
	<-proc.Done()
}

var (
	buildHsOnce sync.Once
	hsBinPath   string
	buildHsErr  error
)

func getHandshakeBinary(t *testing.T) string {
	t.Helper()
	buildHsOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "hs-bin-*")
		if err != nil {
			buildHsErr = err
			return
		}
		bin := filepath.Join(tmpDir, "handshake-app")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "gostalgia/test/testapps/handshake")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildHsErr = fmt.Errorf("build handshake-app: %w\n%s", err, string(out))
			return
		}
		hsBinPath = bin
	})
	if buildHsErr != nil {
		t.Fatalf("failed to build handshake test binary: %v", buildHsErr)
	}
	return hsBinPath
}

// handshakeManager builds an app Manager with one registered external app
// driving the raw handshake test binary with the given args.
func handshakeManager(t *testing.T, args ...string) (*Manager, *ipc.Router, *process.Manager, Manifest) {
	t.Helper()
	bus := events.NewBus()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := ipc.NewRouter()
	procs := process.NewManager(bus, log)
	reg := NewRegistry()
	man := Manifest{
		ID:              "com.test.handshake",
		Name:            "Handshake App",
		Version:         "1.0.0",
		Mode:            sdk.ModeExternal,
		Executable:      getHandshakeBinary(t),
		Args:            args,
		Permissions:     []string{sdk.CapIPC},
		ProtocolVersion: 1,
	}
	must(t, reg.RegisterExternal(man))
	return NewManager(reg, procs, router, bus, log), router, procs, man
}

// TestExternalReadyRouteStormRejected: a ready handshake declaring far more
// routes than maxAppRoutes is rejected and the app is stopped.
func TestExternalReadyRouteStormRejected(t *testing.T) {
	m, _, procs, man := handshakeManager(t, "routes", fmt.Sprint(maxAppRoutes*4))
	_, err := m.Launch(context.Background(), man.ID)
	if err == nil {
		t.Fatal("route storm handshake accepted")
	}
	if !strings.Contains(err.Error(), "too many routes") {
		t.Fatalf("route storm error = %v", err)
	}
	if m.IsRunning(man.ID) {
		t.Fatal("route storm app still marked running")
	}
	deadline := time.Now().Add(5 * time.Second)
	for procs.Count() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procs.Count() > 0 {
		t.Fatal("route storm child still running as process")
	}
}

// TestExternalReadyInvalidRouteNameRejected: route names must be identifiers.
func TestExternalReadyInvalidRouteNameRejected(t *testing.T) {
	m, _, _, man := handshakeManager(t, "badname")
	_, err := m.Launch(context.Background(), man.ID)
	if err == nil {
		t.Fatal("invalid route names accepted")
	}
	if !strings.Contains(err.Error(), "invalid route name") {
		t.Fatalf("invalid route name error = %v", err)
	}
	if m.IsRunning(man.ID) {
		t.Fatal("bad-name app still marked running")
	}
}

// TestExternalReadyRouteBoundAllowsLegit: a bounded number of valid routes
// launches and serves normally, including at the cap boundary.
func TestExternalReadyRouteBoundAllowsLegit(t *testing.T) {
	m, router, _, man := handshakeManager(t, "ok", fmt.Sprint(maxAppRoutes))
	proc, err := m.Launch(context.Background(), man.ID)
	must(t, err)
	resp := router.Dispatch(ipc.WithCapabilities(context.Background(), security.AdminCapabilities()),
		ipc.Request{Method: "app/com.test.handshake/r0"})
	if !resp.OK {
		t.Fatalf("registered route not callable: %s", resp.Error)
	}
	must(t, m.Stop(man.ID, 5*time.Second))
	<-proc.Done()
}

// TestExternalAuthTokenEdgeCases covers the constant-time token check: wrong
// and empty tokens are rejected (ConstantTimeCompare returns 1 for two empty
// inputs, so empty must be guarded explicitly).
func TestExternalAuthTokenEdgeCases(t *testing.T) {
	for _, mode := range []string{"badtoken", "emptytoken"} {
		t.Run(mode, func(t *testing.T) {
			m, _, _, man := handshakeManager(t, mode)
			_, err := m.Launch(context.Background(), man.ID)
			if err == nil {
				t.Fatalf("%s token accepted", mode)
			}
			if m.IsRunning(man.ID) {
				t.Fatalf("%s app still marked running", mode)
			}
		})
	}
}

// TestLaunchFailureStopRace: a Stop racing a failing launch must return
// promptly — the failure path closes ra.done — never with a spurious
// cleanup timeout for an app that no longer exists.
func TestLaunchFailureStopRace(t *testing.T) {
	m, _, _, man := handshakeManager(t, "badtoken", "250")
	const iterations = 15
	for i := 0; i < iterations; i++ {
		launchErr := make(chan error, 1)
		go func() {
			_, err := m.Launch(context.Background(), man.ID)
			launchErr <- err
		}()
		// Hammer Stops through the handshake window (pid assigned, launch
		// still in flight) looking for the race.
		var lerr error
		finished := false
		for j := 0; j < 40 && !finished; j++ {
			err := m.Stop(man.ID, 2*time.Second)
			if err != nil && strings.Contains(err.Error(), "cleanup timed out") {
				t.Fatalf("iteration %d: racing Stop hit spurious cleanup timeout: %v", i, err)
			}
			select {
			case lerr = <-launchErr:
				finished = true
			default:
				time.Sleep(10 * time.Millisecond)
			}
		}
		if !finished {
			lerr = <-launchErr
		}
		if lerr == nil {
			t.Fatalf("iteration %d: bad-token launch succeeded", i)
		}
		if err := m.Stop(man.ID, 2*time.Second); err != nil &&
			strings.Contains(err.Error(), "cleanup timed out") {
			t.Fatalf("iteration %d: post-failure Stop hit spurious cleanup timeout: %v", i, err)
		}
	}
}

// TestLaunchRejectedDuringShutdown: once the process manager begins
// shutdown, app launches are refused instead of being orphaned.
func TestLaunchRejectedDuringShutdown(t *testing.T) {
	bus := events.NewBus()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := ipc.NewRouter()
	procs := process.NewManager(bus, log)
	reg := NewRegistry()
	must(t, reg.RegisterBuiltin(fakeManifest, func() (Instance, error) {
		return &fakeInstance{}, nil
	}))
	m := NewManager(reg, procs, router, bus, log)

	procs.BeginShutdown()
	if _, err := m.Launch(context.Background(), fakeManifest.ID); err == nil ||
		!strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("launch during shutdown = %v, want shutting down error", err)
	}
	if m.IsRunning(fakeManifest.ID) {
		t.Fatal("app marked running after rejected shutdown launch")
	}
}

// TestLaunchShutdownRace hammers launches concurrent with the shutdown
// transition: every launch must be rejected or reliably stopped — never
// orphaned and still running after the dust settles.
func TestLaunchShutdownRace(t *testing.T) {
	for i := 0; i < 30; i++ {
		bus := events.NewBus()
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		procs := process.NewManager(bus, log)
		reg := NewRegistry()
		must(t, reg.RegisterBuiltin(fakeManifest, func() (Instance, error) {
			return &fakeInstance{}, nil
		}))
		m := NewManager(reg, procs, ipc.NewRouter(), bus, log)

		launchErr := make(chan error, 1)
		go func() {
			_, err := m.Launch(context.Background(), fakeManifest.ID)
			launchErr <- err
		}()
		go procs.BeginShutdown()
		err := <-launchErr
		procs.StopAll(2 * time.Second)
		if err == nil {
			// The launch slipped in before the flag: StopAll must own it now.
			deadline := time.Now().Add(5 * time.Second)
			for m.IsRunning(fakeManifest.ID) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if m.IsRunning(fakeManifest.ID) {
				t.Fatalf("iteration %d: app orphaned by shutdown race", i)
			}
			continue
		}
		// Rejected either at the manager gate or the process-manager
		// registration gate; either way the error mentions shutdown.
		if !strings.Contains(err.Error(), "shut") {
			t.Fatalf("iteration %d: launch racing shutdown failed oddly: %v", i, err)
		}
		if m.IsRunning(fakeManifest.ID) {
			t.Fatalf("iteration %d: rejected launch left app running", i)
		}
	}
}
