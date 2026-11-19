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

**Core and SDK: standard library only. Experience: approved Charm family and
bloom.** The coordinated Charm v2 baseline (`charm.land/bubbletea/v2 v2.1.1`,
`charm.land/lipgloss/v2 v2.0.6`, and `charm.land/bubbles/v2 v2.2.1`) is a
direct dependency only in `internal/experience/` (Issues #21–#22, migrated to
v2 in #184). It provides terminal lifecycle/event handling, styling, and
reusable TUI components. `charm.land/ssh v0.4.3` and `golang.org/x/crypto
v0.54.0` serve only the opt-in SSH attach listener in `internal/experience/sshx`
(#190). Their required transitive dependencies (terminal, ANSI, Unicode width,
color/input helpers, and `golang.org/x/*`) are pinned by go.mod/go.sum and are
part of this explicit exception, not hand-added runtime dependencies. Issue #22
promotes `github.com/charmbracelet/x/ansi` (now v0.11.9) and
`github.com/muesli/termenv v0.16.0` to direct experience helpers for cell-aware
layout and explicit renderer profiles; `github.com/charmbracelet/colorprofile
v0.4.3` is passed to `tea.WithColorProfile` at program creation so the kit
stays the only downsampler. The kit never probes the host's dark/light
background.

**bloom (Issue #207).** `bloomfx.dev/bloom` (MIT) is approved as a direct
dependency of the experience layer only, pinned to exactly
`v0.0.0-alpha.6`. Its role is composition: layout solving
(`Row`/`Column`/`Grid` with fill, fixed, percent, and collapsible slots),
layers for modal dialogs and anchored toasts, viewports over append-only
buffers, the real terminal cursor, and additive mouse routing. It does not
replace the theme or the `ui` kit, which remain the visual vocabulary and the
authority for panel border, title, and padding geometry; any component with a
`View() string` method is a bloom leaf. Bubble Tea remains the only terminal
and event-loop owner, and `ui.Sanitize` remains the gate for external text
(bloom's own filter keeps SGR and OSC 8, which is looser than our policy).

bloom is **pre-1.0, has a single maintainer, and expects breaking changes**
between releases. It is therefore pinned to one exact version (no ranges, no
`@latest`, no vendoring or fork), and every upgrade is a deliberate change of
its own: read the upstream changes, update the pin in `go.mod` and
`TestApprovedCharmBaseline`, and rerun the profile proofs and fences below.
bloom's own `go.mod` sets the floors that minimal version selection applied
here: Go 1.27.3, Bubble Tea v2.1.1, x/ansi v0.11.9, and a newer
`charmbracelet/ultraviolet` pseudo-version (`clipperhouse/uax29/v2 v2.7.0` was
already pinned); the indirect `mattn/go-runewidth`, `xo/terminfo`,
`golang.org/x/sync`, and `golang.org/x/sys` moved up with them. Since #208
the shell frame is a bloom layout tree: each shell model owns a root built
with explicit theme colors, and kit output enters the tree through the
`internal/experience/ui` adapters (`Leaf`, `PanelNode`), so the kit still owns
panel geometry. The usage rules are in
[shell.md](shell.md#bloom-layout-and-compositing-issue-207) and the frame in
[shell.md](shell.md#shell-frame-layout-issue-208). Since #211 bloom also routes
the shell's additive mouse input; copies bypass bloom's clipboard, which
would also send OSC 52, and go through the runtime's `clipboard/write` under
operator policy ([shell.md](shell.md#mouse-and-clipboard-issue-211)). Since #213
finite motion is also model-local: the only attached effect is a 160 ms
failed-launch badge shake, with the same per-model clock injected into
`bloom.WithClock` and `anim.Clock`. ReducedMotion attaches no effect or copy
flash and schedules no animation frames; `anim.SetMotion` is never used, and
idle shells retain only their existing functional polling
([experience.md](experience.md#finite-per-session-motion-issue-213)). Since #214 the
Home dashboard also uses `bloom/fig`: the bundled `big` FIGlet font (with two
tracking cells) falls back to bundled `mini` and then the spaced title according
to solved content cells. Both fonts are ASCII and are licensed under
BSD-3-Clause, as recorded from the pinned `fig/fonts/LICENSES.md` in
[experience.md](experience.md#home-figlet-wordmark-issue-214). The wordmark is
static, uses the kit's heading tokens, and adds no animation or terminal-owner
responsibilities.

**steep (Issue #212).** `getsteep.dev/steep` and its separate
`getsteep.dev/steep/reel` module are direct **test-only** dependencies, both
pinned to exactly `v0.0.0-alpha.12`. steep supplies an emulated terminal and
input decoder; reel provides terminal recordings and protocol assertions. They
may be imported only by test files under `internal/experience/` or `test/`.
Production packages, the runtime, the SDK, applications, and `gctl` must not
import either module. steep is also pre-1.0: its harness, snapshot, and recording
APIs may change between releases. Upgrades therefore require an explicit pin
change, a review of upstream changes, inspection of every snapshot diff, and a
fresh three-OS run. Snapshots are updated only when a developer explicitly sets
`UPDATE_SNAPSHOTS=true`; CI always performs read-only comparisons.

`go list -deps gostalgia/internal/runtime gostalgia/sdk` contains only Gostalgia
and standard-library packages. Automated tests in `test/e2e/dependencies_test.go`
enforce that boundary, verify that Charm and bloom (every `bloomfx.dev/`
package, including from tests) are confined to `internal/experience/`,
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

### 4.6 Desktop toolkit — experimental bloom adapter; decision deferred

The desktop is Milestone M4. The documented candidate baseline is **Gio**
(pure Go, no cgo, cross-platform), with Fyne (cgo/OpenGL, more batteries) and
Ebiten (game-oriented) as alternatives. The toolkit decision remains open: a
window service API should keep the toolkit behind `desktop/` adapters.

Issue #215 adds a **nonproduction experimental spike**, not a desktop toolkit
decision or production multiwindow feature. Its neutral draft is
`internal/experience/experimental/windows`; the only implementation is the
adapter-local `bloomadapter`, and `cmd/windowdemo` beneath that experimental
subtree is its standalone entry point. The normal shell, runtime, and startup
paths do not import or register it. The SDK, app-facing presentation contract,
core packages, and event bus implementation are unchanged, and no Gio code or
dependency was added.

#### Draft and observed bloom behavior

The draft's `ID`, `Title`, `Bounds`, `Window`, and `Service` describe identity,
position/size, bottom-to-top z-order, focus, and lifecycle without bloom,
Bubble Tea, `Edge`, or drag-message types. A `Window` carries an unchanged
`sdk.View`: applications still provide data, not toolkit widgets. `Open`,
`Focus`, `Move`, and `Close` are the only mutations. They publish typed
`window.opened`, `window.focused`, `window.moved`, and `window.closed` payloads
with a nonempty source through the existing `events.Bus`. Opening focuses the
new window; focusing raises it; closing the focused window publishes its close
before the next focus transition. Constructing and drawing a frame publishes
nothing. A real `tea.WindowSizeMsg` can clamp bounds through `Move`, so a resize
that changes bounds intentionally publishes `window.moved`; repeated `View`
calls alone never do.

The demonstration opens the existing version-1 **Screen** item/form/action
fixture and version-2 **Pad** block/meter/grid fixture side by side. Focused Go
tests drive solved bloom frames and their actual mouse handlers: a title drag
changes only that window's bounds, clicking an exposed overlapped window raises
it, both border and keyboard close paths leave the survivor editable, and focus
scopes preserve independent field drafts. Enter emits unchanged
`sdk.ActionRequest` values with the fixture version, instance, item/cell IDs,
current fields, and a request ID. Real 80×24 PTY checks show both populated
views, editing/action feedback, focus switching, close, and a usable survivor.
The 30×10 path clamps both windows to the available canvas; the fixtures are
necessarily clipped there and this compact mode is not presented as a complete
small-screen presentation renderer.

The following toolkit leaks and limits were actually observed in the adapter;
none is hidden behind a claim of universal neutrality:

- **Geometry is bloom-specific at the boundary.** Neutral `Bounds` become
  terminal-cell `Layer.Anchor(Start, Start).Offset(...).Size(...)` rectangles.
  Alpha.6 clamps layer size but not offset, so the adapter clamps every drag and
  resize and reserves the help row. There is no independent z-index property:
  declaration order in `bloom.Overlay` is the service's bottom-to-top order.
- **Dragging leaks `on.Drag` mechanics.** The adapter translates `Start`,
  `Screen`, `Done`, and mouse messages into neutral `Move` calls. A stable,
  unique title ID preserves capture when raising a window reorders the layer
  tree. Only the title owns the drag handler: putting it on the box would also
  drag from the close control. The neutral API does not expose these events.
- **Focus needs adapter policy.** Nonmodal layers have no `Scope` method in
  alpha.6, so each window is an `Auto(...).ID(...).Scope()` slot with a retained
  `bloom.Focus`. The adapter pushes/pops scopes, remembers each window's
  control, and supplies F6/Ctrl-W routing. Alt-arrows provide the keyboard
  equivalent of title dragging through the same clamped `Move` transition.
  `Focus.Update` handles Tab and
  Press activation, not text editing or window switching. Click-focus is
  initially confined to the old scope, so control-local mouse handlers restore
  the clicked control while activating its window; otherwise the first click
  would only raise it.
- **Border close and removal have toolkit requirements.** `Box.Edge` displays
  the title and `[X]` only where the box has border cells and enough width.
  Ctrl-W is the keyboard equivalent. In the real PTY, removing a layer left
  stale cells during the terminal's differential repaint even though the new
  full frame was correct. Close therefore returns `tea.ClearScreen` for a full
  repaint; this workaround is adapter-local and adds no animation or idle work.
- **Window chrome is opaque with static depth.** Each layer fills its solved
  rectangle with the theme surface, including border glyphs and blank body
  cells, so canvas text cannot bleed through. A static two-column/one-row
  shadow tint behind the window gives it visible depth without animation or
  idle work; Plain and Monochrome omit the color-only effect. Titles reserve the
  corners, margins, and complete `[X]` control, then truncate to the remaining
  display cells for narrow, wide-glyph, or hostile text.
- **Presentation rendering is deliberately an adapter subset.** The adapter
  maps SDK items, single-line fields, actions, blocks, meters, and grid cells to
  bloom text controls and owns selection/draft state. It does not reuse the
  production shell's private renderer or implement version-3 regions and
  multiline editing. Demo action requests are recorded locally, not sent over
  app IPC, and there is no persistence, reopen UI, production routing, or second
  terminal owner.
- **Color and lifecycle checks found no additional leak in the tested scope.**
  The root uses kit `WithColors`, never `Root.Init`, ignores host color replies,
  and attaches no clipboard, animation, or global motion behavior. Effective
  Plain emitted no escapes and ANSI256 no truecolor in the adapter tests.
  Malicious title/item controls were removed by `ui.Sanitize`. Subscriber
  envelopes had the concrete experimental payload types and documented order;
  repeated renders emitted none. These are observations for this fixture and
  profile matrix, not a proof for every bloom composition or future toolkit.

Run the demonstration with
`go run ./internal/experience/experimental/windows/cmd/windowdemo` and its tests
with `go test -count=1 ./internal/experience/experimental/windows/...`.

#### Gio comparison status and approved deferral

There was **no existing Gio spike, adapter, or earlier draft API** in the
repository or issue history, and no Gio experiment ran in this mission. The only
Gio-side comparison is therefore with the documented candidate baseline above:
its pure-Go/no-cgo/cross-platform goals match this repository's constraints, and
its eventual adapter would need to satisfy the same neutral window and SDK-data
contracts. Whether Gio would impose different bounds, focus, drag, close, or
repaint mechanics is **unverified**. Cross-toolkit neutrality is also
**unverified**; one bloom implementation cannot establish it.

Under the **USER-APPROVED proposal** recorded on
[#206](https://github.com/drawmeanelephant/gostalgia/issues/206#issuecomment-6990850564),
this observed-bloom/documented-Gio comparison substitutes for the unavailable
side-by-side spike. It does not claim empirical Gio fulfillment.
[#216](https://github.com/drawmeanelephant/gostalgia/issues/216) is the linked
follow-up for approving a Gio dependency, implementing the same draft behind a
real Gio adapter, rerunning these behaviors, and replacing every unverified Gio
entry with observed results.

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
| Pre-1.0 bloom (single maintainer, breaking changes expected) | experience-layer rework on upgrade | exact pin, experience-only fence, deliberate upgrades that rerun the profile proofs (§4.1) |
| Pre-1.0 steep/reel test APIs | terminal-test rework on upgrade | exact alpha.12 pins, test-only import fence, explicit snapshot review, three-OS verification (§4.1) |
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
