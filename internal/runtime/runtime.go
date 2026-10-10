// Package runtime boots, wires, and shuts down the Gostalgia environment.
// It owns the boot sequence described in docs/architecture.md: root and
// layout, configuration, logging, leaf managers, core services, session,
// first application, and a self-test IPC round trip before declaring
// readiness.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	"gostalgia/internal/services"
	"gostalgia/internal/session"
	"gostalgia/internal/vfs"
	"gostalgia/platform"
)

// Version is the environment version.
const Version = "0.1.0"

// Options controls Boot.
type Options struct {
	// Root overrides the environment root directory. Empty means
	// $GOSTALGIA_ROOT, then ~/.gostalgia.
	Root string
	// Verbose enables debug logging.
	Verbose bool
	// LogOutput overrides console logging. Interactive hosts pass io.Discard
	// so service logs cannot corrupt a TUI; file logging remains enabled.
	LogOutput io.Writer
	// Notifications records user-facing notifications from services.
	// When nil, notify/post fails closed with "notification pipeline unavailable".
	Notifications service.NotificationSink
}

// Runtime is a booted Gostalgia environment.
type Runtime struct {
	Root       string
	Version    string
	Cfg        *config.Store
	LayeredCfg *config.LayeredStore
	Bus        *events.Bus
	Log        *slog.Logger
	Router     *ipc.Router
	VFS        *vfs.VFS
	Procs      *process.Manager
	Apps       *app.Manager
	//nolint:unused // reserved for multi-session support
	Sessions *session.Manager
	Profiles *profile.Manager
	Services *service.Manager
	Tokens   *security.TokenStore
	Policy   *security.PolicyStore

	svcCtx    *service.Context
	userMu    sync.RWMutex // guards user: the profile-switch callback writes it from IPC handler goroutines
	user      security.User
	logFile   *os.File
	hostFS    *vfs.HostFS   // closed at shutdown; Windows cannot delete an open directory tree
	done      chan struct{} // closed when shutdown begins
	completed chan struct{} // closed when shutdown finishes
	once      sync.Once
}

// ResolveRoot determines the environment root directory: explicit flag,
// then $GOSTALGIA_ROOT, then ~/.gostalgia.
func ResolveRoot(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if env := os.Getenv("GOSTALGIA_ROOT"); env != "" {
		return filepath.Abs(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("runtime: resolve environment root: %w", err)
	}
	return filepath.Join(home, ".gostalgia"), nil
}

// Boot starts a complete environment: root, config, logging, services,
// session, and the first application. It returns once the environment is
// serving IPC; shutdown is triggered by signal or Runtime.Shutdown.
func Boot(ctx context.Context, opts Options) (_ *Runtime, retErr error) {
	root, err := ResolveRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	if err := InitRoot(root); err != nil {
		return nil, err
	}

	layeredCfg, err := config.NewLayeredStore(
		filepath.Join(root, "config", "system.json"),
		filepath.Join(root, "vfs", "users", "guest", "config", "settings.json"),
	)
	if err != nil {
		return nil, err
	}
	cfg := layeredCfg.SystemStore()

	logFile, log := newLogger(root, opts.Verbose, opts.LogOutput)

	rt := &Runtime{
		Root:       root,
		Version:    Version,
		Cfg:        cfg,
		LayeredCfg: layeredCfg,
		Bus:        events.NewBusWithLogger(log),
		Router:     ipc.NewRouter(),
		logFile:    logFile,
		done:       make(chan struct{}),
		completed:  make(chan struct{}),
		user:       security.User{ID: "u-guest", Name: "guest"},
	}
	rt.Log = log

	// Event trail at debug level; subsystems log their own lifecycle
	// changes at info — this catches everything else.
	rt.Bus.Subscribe("*", func(env events.Envelope) {
		log.Debug("event", "id", env.ID, "type", env.Type, "source", env.Source)
	})

	hostFS, err := vfs.NewHost(filepath.Join(root, "vfs"))
	if err != nil {
		logFile.Close()
		return nil, err
	}
	rt.hostFS = hostFS
	// On any later boot failure, release the host root handle: on
	// Windows an open handle prevents the directory from being removed.
	defer func() {
		if retErr != nil {
			hostFS.Close()
		}
	}()
	rt.VFS = vfs.New(hostFS)
	if err := rt.VFS.Mount("/tmp", vfs.NewMem()); err != nil {
		logFile.Close()
		return nil, err
	}

	profilesMgr, err := profile.NewManager(rt.VFS, "", rt.Bus, log)
	if err != nil {
		logFile.Close()
		return nil, err
	}
	rt.Profiles = profilesMgr
	activeProfile := rt.Profiles.Active()
	rt.setUser(activeProfile.User())

	rt.Profiles.OnSwitch(func(prev, next profile.Profile) {
		rt.setUser(next.User())
		if rt.Apps != nil {
			rt.Apps.SetUser(next.User())
		}
		if rt.LayeredCfg != nil {
			_ = rt.LayeredCfg.SetUserStore(filepath.Join(root, "vfs", "users", next.ID, "config", "settings.json"))
		}
		if rt.Tokens != nil {
			rt.Tokens.SetOperatorUser(next.User())
		}
	})

	rt.Procs = process.NewManager(rt.Bus, log)
	rt.Sessions = session.NewManager(rt.Bus, log)
	rt.Sessions.SetVFS(rt.VFS)

	registry := app.NewRegistry()
	if err := apps.Register(registry); err != nil {
		logFile.Close()
		return nil, err
	}
	if err := app.SeedManifests(rt.VFS, apps.Manifests()); err != nil {
		logFile.Close()
		return nil, err
	}
	if _, err := registry.LoadManifests(rt.VFS, "apps/manifests"); err != nil {
		logFile.Close()
		return nil, err
	}
	rt.Apps = app.NewManager(registry, rt.Procs, rt.Router, rt.Bus, log)
	rt.Apps.SetUser(rt.CurrentUser())
	rt.Apps.SetRoot(root)

	token, err := newToken()
	if err != nil {
		logFile.Close()
		return nil, err
	}
	tokens := security.NewTokenStore()
	if err := tokens.RegisterOperator(token, rt.CurrentUser()); err != nil {
		logFile.Close()
		return nil, err
	}
	rt.Tokens = tokens
	rt.Apps.SetTokenStore(tokens)
	rt.Apps.SetGrantStore(rt.VFS.Grants())

	policyStore := security.NewPolicyStore(security.DefaultOperatorPolicy())
	rt.Policy = policyStore
	rt.Apps.SetPolicyStore(policyStore)

	svcCtx := &service.Context{
		Root:          root,
		Version:       Version,
		Config:        cfg,
		Layered:       layeredCfg,
		Events:        rt.Bus,
		Log:           log,
		Router:        rt.Router,
		VFS:           rt.VFS,
		Procs:         rt.Procs,
		Apps:          rt.Apps,
		Sessions:      rt.Sessions,
		Profiles:      rt.Profiles,
		Tokens:        tokens,
		Policy:        policyStore,
		Notifications: opts.Notifications,
		Token:         token,
		BootedAt:      time.Now(),
	}
	rt.svcCtx = svcCtx

	if err := checkNotRunning(root); err != nil {
		logFile.Close()
		return nil, err
	}

	sm := service.NewManager(svcCtx, rt.Bus, log)
	svcCtx.Services = sm
	for _, s := range []service.Service{
		services.NewSys(),
		services.NewProc(),
		services.NewFS(),
		services.NewDocument(),
		services.NewIPC(),
		services.NewConfig(),
		services.NewClipboard(),
		services.NewNet(),
		services.NewSession(),
		services.NewProfile(),
		services.NewPackage(),
		services.NewRecovery(),
		services.NewNotify(),
		services.NewSound(),
	} {
		if err := sm.Register(s); err != nil {
			logFile.Close()
			return nil, err
		}
	}
	rt.Services = sm
	svcCtx.Shutdown = rt.Shutdown

	if err := sm.StartAll(ctx); err != nil {
		// StartAll has already rolled back the services that reached
		// running; Shutdown finishes the boot-failure teardown (VFS
		// handle, log file, lifecycle channels) exactly once.
		rt.Shutdown("boot failure")
		return nil, err
	}

	// Session for the current user. An operator may have switched profiles
	// over IPC while services started, so read the identity under its lock.
	sess, err := rt.Sessions.Create(rt.CurrentUser())
	if err != nil {
		rt.Shutdown("boot failure")
		return nil, err
	}

	// First application, launched and managed by the environment.
	if _, err := rt.Apps.Launch(ctx, "com.gostalgia.echo"); err != nil {
		rt.Sessions.Close(sess.ID)
		rt.Shutdown("boot failure")
		return nil, err
	}

	// Self-test: one in-process IPC round trip before declaring ready.
	selfTest := ipc.WithCapabilities(context.Background(), security.AdminCapabilities())
	resp := rt.Router.Dispatch(selfTest, ipc.Request{ID: 1, Method: "sys/ping"})
	if !resp.OK {
		rt.Shutdown("boot failure")
		return nil, fmt.Errorf("runtime: self-test failed: %s", resp.Error)
	}

	log.Info("runtime ready",
		"version", Version,
		"root", root,
		"endpoint", svcCtx.Endpoint,
		"session", sess.ID,
	)
	return rt, nil
}

// InitRoot creates the environment directory layout and default
// configuration if they do not exist. It is idempotent.
func InitRoot(root string) error {
	for _, dir := range []string{
		"config",
		"logs",
		"vfs",
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return fmt.Errorf("runtime: create %s: %w", dir, err)
		}
	}
	hostFS, err := vfs.NewHost(filepath.Join(root, "vfs"))
	if err != nil {
		return err
	}
	defer hostFS.Close()
	env := vfs.New(hostFS)
	if err := env.Mount("/tmp", vfs.NewMem()); err != nil {
		return err
	}
	for _, dir := range []string{
		"/users/guest/documents",
		"/users/guest/downloads",
		"/users/guest/desktop",
		"/users/guest/config",
		"/users/guest/.trash",
		"/apps/manifests",
		"/apps/data",
		"/config",
		"/data",
		"/mounts",
	} {
		if err := env.MkdirAll(dir); err != nil {
			return fmt.Errorf("runtime: seed %s: %w", dir, err)
		}
	}
	return nil
}

// CurrentUser returns the active profile's user identity. It is safe for
// concurrent use: the profile-switch callback updates the identity from
// IPC handler goroutines while boot and services read it.
func (rt *Runtime) CurrentUser() security.User {
	rt.userMu.RLock()
	defer rt.userMu.RUnlock()
	return rt.user
}

func (rt *Runtime) setUser(u security.User) {
	rt.userMu.Lock()
	defer rt.userMu.Unlock()
	rt.user = u
}

// Done is closed when shutdown begins (via signal handling in the caller
// or a sys/shutdown request).
func (rt *Runtime) Done() <-chan struct{} { return rt.done }

// Wait blocks until shutdown has completed.
func (rt *Runtime) Wait() { <-rt.completed }

// Endpoint returns the IPC endpoint the environment is serving on.
func (rt *Runtime) Endpoint() string { return rt.svcCtx.Endpoint }

// Shutdown performs the graceful shutdown sequence exactly once and
// blocks until it completes: stop applications, close sessions, stop
// services in reverse order, then flush logs.
func (rt *Runtime) Shutdown(reason string) {
	rt.once.Do(func() {
		close(rt.done)
		if reason == "" {
			reason = "unspecified"
		}
		rt.Log.Info("shutting down", "reason", reason)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		for id := range rt.Apps.Running() {
			if err := rt.Apps.Stop(id, 5*time.Second); err != nil {
				rt.Log.Warn("application shutdown problem", "app", id, "err", err)
			}
		}
		rt.Procs.StopAll(5 * time.Second)
		for _, sess := range rt.Sessions.Active() {
			if err := rt.Sessions.Close(sess.ID); err != nil {
				rt.Log.Warn("session shutdown problem", "session", sess.ID, "err", err)
			}
		}
		if err := rt.Services.StopAll(ctx); err != nil {
			rt.Log.Warn("service shutdown problems", "err", err)
		}
		if rt.hostFS != nil {
			if err := rt.hostFS.Close(); err != nil {
				rt.Log.Warn("vfs close problem", "err", err)
			}
		}
		if rt.logFile != nil {
			rt.Log.Info("shutdown complete")
			rt.logFile.Close()
		}
		close(rt.completed)
	})
	<-rt.completed
}

func newLogger(root string, verbose bool, output io.Writer) (*os.File, *slog.Logger) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	file, err := os.OpenFile(filepath.Join(root, "logs", "gostalgia.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if output == nil {
		output = os.Stdout
	}
	var w io.Writer = output
	if err == nil {
		w = io.MultiWriter(output, file)
	}
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return file, slog.New(handler)
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("runtime: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// checkNotRunning refuses to boot if a live environment already owns this
// root. Anything that answers on the recorded endpoint and accepts the
// recorded token is a live instance — including one still mid-boot, whose
// sys routes may not be registered yet. A stale file from a dead instance
// is ignored.
func checkNotRunning(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		return nil // missing or unreadable: stale; boot overwrites it
	}
	var info struct {
		PID      int    `json:"pid"`
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(data, &info); err != nil || info.Endpoint == "" {
		return nil
	}
	conn, err := platform.DialIPC(info.Endpoint)
	if err != nil {
		return nil // nothing is listening: stale file from a dead instance
	}
	client, err := ipc.NewClient(conn, info.Token)
	if err != nil {
		return nil // the listener rejected the recorded token: not our instance
	}
	client.Close()
	return fmt.Errorf("runtime: environment is already running (pid %d, endpoint %s)", info.PID, info.Endpoint)
}
