# Gostalgia Architecture

Gostalgia is a cross-platform, pseudo-operating-system / portable computing
environment written in Go. It runs as a hosted process on top of Windows,
macOS, or Linux, but presents its **own** abstractions for applications,
processes, files, services, IPC, configuration, permissions, sessions, and
system management. The host OS is substrate, not the application model.

This document is the authoritative architecture. When implementation and
document disagree, one of them is wrong; fix both.

---

## 1. Layered model

```text
┌──────────────────────────────────────────────────────────────┐
│  APPLICATIONS        echo · terminal · editor · file mgr …   │
├──────────────────────────────────────────────────────────────┤
│  EXPERIENCE  Charm shell now · desktop/windows later (M4+) │
├──────────────────────────────────────────────────────────────┤
│  SYSTEM SERVICES     sys · process · fs · ipc · session …    │
├──────────────────────────────────────────────────────────────┤
│  OS RUNTIME          app model · process model · events ·    │
│                      config · security · lifecycle           │
├──────────────────────────────────────────────────────────────┤
│  PLATFORM ABSTRACTION platform/ (unix, windows)              │
├──────────────────────────────────────────────────────────────┤
│  HOST OS             Windows / macOS / Linux                 │
└──────────────────────────────────────────────────────────────┘
```

Rules that hold the layers together:

- Applications and services talk to **each other through the environment**
  (IPC router, VFS, process manager, event bus), never through host APIs.
- Platform-specific code lives only under `platform/` (and, rarely, behind
  build tags next to the subsystem that owns the concern). Everything else
  must compile for all three targets with no `GOOS` conditionals.
- Lower layers never import higher layers. `internal/app` may not know about
  the desktop; `internal/ipc` may not know about apps.

## 2. Package map and dependency direction

```text
gostalgia
├── cmd/
│   ├── gostalgia/        # the environment runtime (boot/serve)
│   └── gctl/          # client CLI for a running environment
├── internal/
│   ├── config/         # layered JSON config store          (leaf)
│   ├── events/         # typed pub/sub event bus            (leaf)
│   ├── ipc/            # router + NDJSON socket server/client
│   ├── vfs/            # virtual filesystem, mounts, memfs  (leaf)
│   ├── process/        # environment process manager
│   ├── security/       # users, capabilities                (leaf)
│   ├── profile/        # workspace profiles and isolation   (leaf)
│   ├── recovery/       # portable backup and disaster restore (leaf)
│   ├── pkg/            # application package management     (leaf)
│   ├── session/        # user sessions
│   ├── service/        # service lifecycle framework
│   ├── services/       # concrete core services (sys, process, fs, ipc, profile, package, recovery)
│   ├── experience/     # Charm shell (imports IPC client contracts only)
│   ├── app/            # application model, manifests, launcher
│   └── runtime/        # boot, wiring, shutdown
├── sdk/                # public stdlib-only app contract
├── apps/               # builtin applications: echo/ + registration & manifest seeding
├── platform/           # host-specific endpoints (sockets, paths)
├── cmd/gostalgia         # the environment runtime binary
├── cmd/gctl           # client CLI for a running environment
├── test/e2e            # full-environment test over a real socket
├── docs/
├── status.md           # living status + the milestone list (canonical tracker)
└── go.mod
```

Dependency flow (arrows = "imports"):

```text
config, events, vfs, security  ←  ipc, process, session  ←  app  ←  apps
                                   ↑ all of the above                ↑
                                   service → services → runtime → cmd
```

No cycles. `service.Context` carries the wiring (config, bus, router, vfs,
procs, apps, sessions) down to concrete services so they never import
`runtime`.

## 3. Subsystems

### 3.1 Runtime (`internal/runtime`)

Owns boot and shutdown. Boot sequence:

1. Resolve environment root (`--root` > `$GOSTALGIA_ROOT` > `~/.gostalgia`), create layout.
2. Open configuration store (`config/system.json`).
3. Set up logging (stdout + `<root>/logs/gostalgia.log`).
4. Construct leaf managers: event bus, IPC router, VFS (host-backed, `/tmp` memfs mount), process manager, session manager, app registry.
5. Register core services; start them in dependency order.
6. IPC service listens (platform adapter) and writes `<root>/runtime.json`
   (endpoint + auth token); guard against a second live instance.
7. Create default user + session; launch the builtin `com.gostalgia.echo` app.
8. Runtime is "ready"; serves until shutdown request (signal or IPC).

Shutdown is the reverse: stop apps → stop services (reverse order) → close
IPC → remove `runtime.json` → flush logs. Idempotent, driven by
`Runtime.Shutdown(reason)`.

### 3.2 Service framework (`internal/service`) + core services (`internal/services`)

A service has explicit states: `created → starting → running → stopping →
stopped` (or `failed`), plus `Name()`, `Depends()`, `Init()`, `Start()`,
`Stop()`. The manager topologically sorts by dependencies, starts in order,
rolls back on failure, and stops in reverse. Lifecycle transitions publish
events. Concrete services in the slice: `sys` (status/ping/shutdown/app
endpoints), `process` (list/stop), `fs` (list/read/write/mkdir over the VFS),
`ipc` (socket listener + auth).

### 3.3 Process model (`internal/process`)

Environment processes are explicit objects with ID, name, kind, state, owner
session, and lifecycle — never anonymous goroutines. Two kinds, deliberately
distinct:

| Kind | Meaning | Mechanism | Isolation |
|---|---|---|---|
| `inproc` | App/service logic inside the runtime | goroutine + cancellable context | logical only |
| `child`  | Real host child process | `os/exec` + `CommandContext` | host process |

The distinction is part of the API and of every listing/IPC payload. We do
**not** pretend goroutines are OS processes. `proc/list` already returns
complete `process.Info` snapshots (including `exit_code`, timing, state, and
error) over IPC, though CLI/shell presentation and output capture into
bounded buffers are tracked in Milestone 3. Supervision (restart policies,
reaping, crash-loop protection) is tracked in Milestone 3, and platform-specific
execution policies in Milestone 4.

### 3.4 IPC (`internal/ipc`)

One router, two transports:

- **In-process**: direct `Router.Dispatch` calls. Same routing semantics as
  the wire — the wire is a transport detail, not a second API.
- **Local socket**: NDJSON request/response framed over a platform listener
  (unix domain socket on macOS/Linux, loopback TCP on Windows), with a token
  handshake. This is what `gctl` and out-of-process apps use.

Protocol: request `{id, method, params}`, response `{id, ok, data, error}`.
Methods are namespaced: `sys/status`, `proc/list`, `fs/read`,
`app/<app-id>/<method>`. Streaming and pub/sub over the socket are future
work; the framing allows it (one JSON value per line).

### 3.5 Filesystem (`internal/vfs`)

A virtual filesystem rooted at `/`, backed by a host directory
(`<root>/vfs`), with a mount table (the slice mounts an in-memory FS at
`/tmp`). Applications only ever see env paths like `/users/guest/documents`;
host paths never leak into the API. The host backend confines all paths
lexically inside the root and is `testing/fstest`-verified. Future mounts:
memory FS, remote FS, per-app private storage, host-dir mounts.

### 3.6 Application model (`internal/app`, `apps/`)

An application is declared by a **manifest** (id, name, version, entrypoint,
permissions, description; JSON — see 4.3) and implemented by a factory that
produces an `sdk.Instance` with `Init(*sdk.Context)`, `Run(ctx)`, and
`Stop(ctx)`. Factories/apps receive no raw router, VFS, or process manager.
Launch reserves the ID before factory/Init, stages routes atomically under
`app/<id>/…`, and runs a tracked process with manifest capabilities on its
context. SDK calls and handlers replace inherited caller permissions, including
operator permissions, with the app's grant. Cleanup drains handlers, calls Stop,
and retracts routes/state on failure or exit. Events use `app.state` with
`launched`/`exited`. Single instance per app ID; compiled builtin manifests win
over seeded disk copies. See [applications.md](applications.md) for the spec.

### 3.6a Terminal experience (`internal/experience/shell`)

Bubble Tea owns the event loop/terminal; Lip Gloss supplies the DOS-inspired
styling through explicit `internal/experience/theme` tokens and the reusable
`internal/experience/ui` component kit ([experience.md](experience.md)).
The shell talks through authenticated IPC, never via runtime managers.
`gostalgia` (no args) or `gostalgia shell` owns boot → shell → graceful shutdown;
`gostalgia boot` remains the headless entrypoint. Runtime logging accepts an
`io.Writer` so the interactive host suppresses console logs without importing
Charm into runtime. App shelf, launch/stop, VFS navigation, process inspection,
history, completion, and safe bounded scrollback are documented in
[shell.md](shell.md).

### 3.7 Events (`internal/events`)

Typed events (`Event` interface with `Type()`), published as envelopes (id,
type, source, time, payload) on a bus with per-topic and wildcard
subscribers. Handlers run synchronously in the publisher goroutine (ordered,
deterministic, no hidden buffering); slow consumers must use their own
goroutine — the bus is not a queue. Each subsystem defines its own event
types; nothing publishes untyped maps.

### 3.8 Sessions & security (`internal/session`, `internal/security`)

Users and sessions exist as first-class environment concepts from day one
(the slice creates user `guest` and one session at boot).

Sessions support detachable interactive clients and persistent workspace state:
- **Detachable shells & multi-client attachment**: Headless runtimes (`gostalgia boot`)
  run independently of interactive terminals. Interactive shell clients attach to a
  running session via `gostalgia attach` or `gostalgia shell --attach`, and can cleanly
  detach via `detach`, leaving the runtime, background services, and hosted applications
  running undisturbed. Multiple interactive clients can attach to the same session
  simultaneously, with attachment lifecycle events published on the `session` bus topic.
- **Persistent workspace state**: Sessions persist working directory (CWD), active
  view, and command history in the user's VFS configuration (`/users/guest/config/workspace.json`)
  using atomic writes (`vfs.FS.SaveAtomic`). If workspace state is corrupted or missing,
  safe fallback defaults are restored without failing boot.
- **Sensitive command & token redaction**: Command history automatically filters and
  redacts sensitive information before persistence. Commands starting with `auth`, `token`,
  or `login` are omitted from history entirely. Long hex tokens (>= 32 characters) and
  flags/parameters matching passwords, secrets, bearer tokens, and API keys are redacted
  with `[REDACTED]`, ensuring secrets never leak to disk.

Security model:
**capabilities**. A process's capabilities come from its manifest
permissions; they are attached to the IPC context and enforced at handler
boundaries (`security.Capabilities.Has`). Read/write VFS methods, process
listing/stopping, app listing/launching/stopping, session management (`session.read`, `session.write`),
profile management (`profile.read`, `profile.write`),
backup management (`backup.read`, `backup.write`),
and shutdown check their individual grants; app route publication/invocation checks `ipc`. App SDK
`Call` and `Handle` adapters replace caller capabilities with the app's manifest
grant, enforcing capability scoping and preventing confused-deputy attacks.
Calls attempting undeclared capabilities fail in production. Trusted operator
clients (`gctl`, shell) connect over the local socket with an admin token.
See the complete permission table in [applications.md](applications.md).

### 3.8a Personal workspace profiles (`internal/profile`)

Personal profiles provide multi-user directory isolation, live identity switching,
and personal workspace separation under `/users/<profile_id>/`:
- **Directory isolation**: Each profile seeds a standard directory tree upon creation:
  `/users/<profile_id>/{documents,downloads,desktop,config,.trash}`. Documents, recents,
  favorites, and workspace states are partitioned per user directory.
- **Profile registry**: Profiles are recorded in `/config/profiles.json` and managed
  via `profile.Manager`. A default `guest` profile is seeded at initial environment boot.
- **Dynamic profile switching**: Calling `Switch(profileID)` publishes transition
  events (`profile.switched`), dynamically reconfigures the layered configuration store
  (`LayeredStore.SetUserStore`), points document search, recents, and favorites to the
  new profile (`Store.SwitchProfile`), binds application launch tokens and process
  attribution to the active profile's `security.User`, ensures an active session exists,
  and updates operator IPC tokens.
- **IPC profile service**: Endpoints under `profile/*` (`list`, `get`, `active`, `create`,
  `update`, `switch`, `delete`) enable programmatic profile administration, guarded by
  `profile.read` and `profile.write` capabilities.
- **Profile deletion**: `Delete` removes the profile from the registry and deletes its
  entire `/users/<profile_id>/` data tree (documents, config including workspace history,
  downloads, desktop, trash), so private data does not outlive the profile. The active
  profile and the last remaining profile cannot be deleted.

### 3.8b Portable backup and disaster recovery (`internal/recovery`)

Disaster recovery and state migration provide bounded, verified archive portability:
- **Format**: ZIP-based `.gbar` containing `backup.json` manifest and `data/` file trees.
- **Resource bounds**: 100 MiB archive size, 200 MiB extracted limit, 5,000 files, 100x max compression ratio, and 32 MiB max single file limit.
- **Security filtering**: Restricts archives to `/config` and `/users/<id>/*`. Automatically excludes live session tokens, host endpoints, IPC sockets, logs, transient files, and private app mounts.
- **Conflict detection & rollback**: Interactive preview identifies `create`, `identical`, and `conflict` entries. Restores support `abort`, `overwrite`, and `skip` conflict strategies with atomic transactional staging and automated journaled rollback.
- See [recovery.md](recovery.md) for full specifications and command reference.

**Honesty clause:** this is *logical* isolation only while applications run
in-process. The auth token on the socket protects against accidental
cross-user access, not a determined local attacker. Process isolation and
OS-level sandboxing (job objects / sandbox profiles / landlock / seccomp) are
organized under Milestone 4 and are not claimed until implemented and verified.

### 3.9 Configuration (`internal/config`)

One JSON document per environment (`config/system.json`) addressed by dotted
paths, persisted atomically on write. Defaults live in code. Per-user, and
per-app layers are future milestones; the store API already supports overlay
by construction.

### 3.10 Platform layer (`platform/`)

Only what is genuinely host-specific, kept thin and behind build tags:

- IPC listener/dial: unix socket (`unix` build tag) vs loopback TCP
  (`windows`).
- Shutdown signal set: unix requests `os.Interrupt` + `SIGTERM`; Windows
  requests `os.Interrupt` only (see below).
- Environment-root resolution defaults.

Signals are the one item on the "host integration" list that the standard
library does not make portable: which terminating signals a host delivers
differs per platform, so the requested set lives behind build tags in
`platform/`. On Windows only Ctrl-C / console close reaches the process;
other termination paths (`taskkill /f`, job-object teardown) deliver no
signal at all, so a Windows host currently gets graceful shutdown via
Ctrl-C or IPC only — a known gap recorded in `docs/platform.md` and
scheduled for the platform-parity milestone. Everything else (filesystem,
exec, clocks) is already portable in the standard library. As desktop/audio
arrive, their host bits get adapters here.

## 4. Decisions and trade-offs

### 4.1 Dependency policy

**Core and SDK: standard library only. Experience: approved Charm family.**
Bubble Tea v1.3.10, Lip Gloss v1.1.0, and Bubbles v1.0.0 are direct dependencies
only in `internal/experience/` (Issues #21–#22). They provide terminal
lifecycle/event handling, styling, and reusable TUI components; no alternate UI
framework or unrelated direct dependency is added. Their required transitive
dependencies (terminal, ANSI, Unicode width, color/input helpers, and
`golang.org/x/*`) are pinned by go.mod/go.sum and are part of this explicit
exception, not hand-added runtime dependencies. Issue #22 promotes the already
pinned `github.com/charmbracelet/x/ansi v0.11.6` and
`github.com/muesli/termenv v0.16.0` to direct experience helpers for cell-aware
layout and explicit renderer profiles, without adding modules or changing
versions. The kit never probes the host's dark/light background.

`go list -deps gostalgia/internal/runtime gostalgia/sdk` contains only Gostalgia
and standard-library packages. Automated tests in `test/e2e/dependencies_test.go`
enforce that boundary, verify that Charm is confined to `internal/experience/`,
ensure no standalone executables (e.g. gum, glow, vhs) are used, and lock the
approved direct baseline. `gctl` and the SDK/demo dependency closures likewise
remain stdlib-only. The repository shares one module, so module downloads include
Charm, but headless runtime packages never import it. No cgo requirement; verify
with `CGO_ENABLED=0 go build ./...`. JSON manifests and stdlib command parsing
remain intentional. Early VirelaiOS bring-up (toolchain, guest runner, kernel
integration) is separately owned by the repository owner and tracked outside
this roadmap; no guest port is treated as implemented here.

### 4.2 JSON manifests, not YAML

Same schema as the brief's sketch, but `encoding/json` is stdlib and the
manifest is machine-facing (produced/consumed by tooling), not hand-edited
prose. Revisit if hand-authoring becomes common.

### 4.3 NDJSON IPC

Line-delimited JSON is trivially debuggable (`nc` + `tail`), unframes
correctly, and streams. Binary/shmem transports can be added behind the same
router if a workload demands it.

### 4.4 Synchronous event handlers

Determinism and ordering beat throughput at this stage. The bus documents
the contract; an async subscriber helper can be added without changing
publishers.

### 4.5 Host-backed VFS, not a real FS driver

The slice maps `/` onto `<root>/vfs` with lexical confinement. That is
enough to prove the abstraction boundary; real drivers/virtual backends
slide in behind the same `vfs.FS` interface later.

### 4.6 Desktop toolkit — deferred decision, leading candidate recorded

The desktop is Milestone M4. Leading candidate: **Gio** (pure Go, no cgo,
cross-platform); alternatives: Fyne (cgo/OpenGL, more batteries), Ebiten
(game-oriented). This decision is *not* made yet; the window service API
will be designed first so the toolkit sits behind `desktop/` adapters.

## 5. Milestones

The roadmap is structured into five sequential milestones:

- **01: A welcoming terminal workspace** ([#20](https://github.com/drawmeanelephant/gostalgia/issues/20)–[#24](https://github.com/drawmeanelephant/gostalgia/issues/24)) —
  evidence-based roadmap/security reconciliation, coordinated Charm dependency
  baseline, reusable visual language, home/launcher navigation, and accessibility
  fallbacks.
- **02: Everyday apps and documents** ([#25](https://github.com/drawmeanelephant/gostalgia/issues/25)–[#29](https://github.com/drawmeanelephant/gostalgia/issues/29)) —
  safe VFS document operations, decoupled UI presentation contract, Files browser,
  Notes editor with crash recovery, and interactive layered settings.
- **03: A live, observable computer** ([#30](https://github.com/drawmeanelephant/gostalgia/issues/30)–[#34](https://github.com/drawmeanelephant/gostalgia/issues/34)) —
  bounded child-process stdout/stderr capture, process supervision and crash-loop
  protection, non-blocking IPC event subscriptions, Task Manager, and detachable
  workspaces.
- **04: Application isolation and trust** ([#35](https://github.com/drawmeanelephant/gostalgia/issues/35)–[#39](https://github.com/drawmeanelephant/gostalgia/issues/39)) —
  distinct app identity and credentials, external Go app lifecycle, app-private
  storage and scoped grants, platform execution enforcement on macOS and Linux,
  and adversarial isolation tests.
- **05: A personal, extensible computer** ([#40](https://github.com/drawmeanelephant/gostalgia/issues/40)–[#44](https://github.com/drawmeanelephant/gostalgia/issues/44)) —
  safe application packages and rollback, personal workspace profiles, document
  search and open-with handoff, portable backup/restore, and explicit host bridges.

The foundational runtime vertical slice (boot, services, VFS, IPC, inproc/child
processes, initial Charm shell, Echo app SDK demo) is merged and verified.
The canonical tracker for all 25 issues lives in [status.md](../status.md).

## 6. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Windowing toolkit choice | rework of desktop layer | design window service API first; toolkit behind adapter; spike Gio early |
| Goroutine-vs-process confusion | wrong abstractions, false security | explicit `Kind` everywhere; docs state isolation level per kind |
| Windows divergence (named pipes vs AF_UNIX, path quirks, case-insensitivity) | broken parity | platform/ adapters + CI matrix + behavior tests, not just compilation |
| VFS symlink/TOCTOU escape | containment failure | confinement via `os.Root` now; per-app views in Milestone 4 (#37); honest docs |
| Scope creep (kernel/browser envy) | nothing actually works | milestone gates: each must boot, run, and be tested before the next |
| Single-runtime coupling (crash kills env) | reliability | supervisor/restart in Milestone 3 (#31); out-of-proc apps in Milestone 4 (#36) |
| Auth/token theater | false security claims | security doc states exactly what is and is not protected |

## 7. Testing strategy

- Unit tests per subsystem (bus, config, service ordering/rollback, process
  lifecycle incl. child procs via self-exec helper, VFS with
  `testing/fstest`, IPC router/server/client incl. auth, app manager).
- End-to-end: boot a real runtime in a temp root, talk to it over a real
  socket (`test/e2e`), verify clean shutdown.
- Manual: `gostalgia boot` + `gctl` walkthrough documented in
  `docs/development.md`.
- Gates: `go vet ./...`, `gofmt`, `go test ./...`, `go test -race ./...`
  (unix), CI matrix on all three OSes.
