# Gostalgia

**Gostalgia** — *Go* + *nostalgia*: a computing environment with the soul of
the machines we grew up on, built in Go.

A cross-platform pseudo-operating-system / portable computing environment.
It runs hosted on Windows, macOS, or Linux but presents its own
abstractions for applications, processes, files, services, IPC, configuration,
sessions, and permissions. The host OS is substrate, not the application model.

**Status: M1 (runtime vertical slice) complete — live tracker in
[status.md](status.md).** The environment boots, starts its core services,
opens a user session, launches and supervises applications, serves a local IPC
endpoint, and shuts down cleanly, with tests at every layer.

## Requirements and setup

Install **Go 1.27.3 or newer**. The floor comes from the exactly pinned,
pre-1.0 `bloomfx.dev/bloom v0.0.0-alpha.6` experience dependency. Download the
module dependencies and build every package:

```sh
go mod download
go build ./...
```

The runtime, core services, app SDK, built-in apps, and `gctl` are
standard-library-only. The terminal experience uses the approved Charm family
and bloom, without cgo. All rendering and runtime code is pure Go and builds on
Windows, macOS, and Linux; host-specific socket and signal code stays under
`platform/`.

## Run it

Use a real interactive terminal, preferably at least 80×24 (30×10 minimum).
These relative roots work on all three host operating systems:

```sh
go run ./cmd/gostalgia shell --root ./.gostalgia-demo
```

Or boot headless and use the control client from another terminal:

```sh
go run ./cmd/gostalgia boot --root ./.gostalgia-demo  # terminal 1
go run ./cmd/gctl --root ./.gostalgia-demo status    # terminal 2
go run ./cmd/gctl --root ./.gostalgia-demo echo hello
go run ./cmd/gctl --root ./.gostalgia-demo ps
go run ./cmd/gctl --root ./.gostalgia-demo ls /users/guest
go run ./cmd/gctl --root ./.gostalgia-demo shutdown
```

The shell provides a DOS-style prompt, Home dashboard and FIGlet wordmark,
searchable launcher, command palette and confirmations, Task Manager and log
tails, notifications and toasts, bounded searchable transcript history,
presentation-data app views, themes and accessible color modes, keyboard and
additive mouse input, policy-controlled host clipboard writes, detachable SSH
sessions, and bounded per-session motion. Every mouse action retains a keyboard
route. See [docs/shell.md](docs/shell.md) for commands and keybindings.

## What is here

- **Runtime** (`internal/runtime`) — boot, wiring, graceful shutdown.
- **Service framework** (`internal/service`) — dependency-ordered lifecycle,
  failure rollback, reverse-order stop.
- **Core services** (`internal/services`) — system, process, filesystem, IPC,
  notifications, clipboard, configuration, sessions, profiles, packages, and
  recovery.
- **Process model** (`internal/process`) — explicit in-proc vs child kinds,
  states, events, stop timeouts, output capture, and supervision.
- **Packages** (`internal/pkg`) — bounded archive verification, publisher
  provenance, atomic install/update/rollback, and permission inspection through
  `gctl pkg` and shell `pkg`/`package`. See [docs/packages.md](docs/packages.md).
- **IPC** (`internal/ipc`) — one router, in-proc + NDJSON socket transports,
  token handshake, capability-carrying call contexts.
- **VFS** (`internal/vfs`) — environment-rooted filesystem with mounts,
  host backend confined via `os.Root`, memfs `/tmp`; `testing/fstest`-verified.
- **Experience** (`internal/experience`) — the shell and its theme/component
  vocabulary. The exactly pinned bloom engine provides solved layout, layers,
  buffers, additive mouse routing, finite per-session motion, and the Home
  FIGlet wordmark. Its bundled `big` and `mini` fonts are ASCII and
  BSD-3-Clause; attribution and fallbacks are documented in
  [docs/shell.md](docs/shell.md#home-wordmark-and-ascii-chrome-issue-214).
- **App SDK** (`sdk`, `internal/app`, `apps/`) — embedded JSON manifests,
  Init/Run/Stop, capability-scoped calls and routes, single-instance launch,
  failure cleanup, and data/action presentation contracts. Apps supply data,
  never terminal or desktop toolkit views.
- **Events, config, sessions, capabilities** — the small primitives the rest
  builds on.

See [docs/applications.md](docs/applications.md) for the app-writing spec,
[docs/architecture.md](docs/architecture.md) for design decisions and risks,
and [docs/](docs/) for subsystem guides.

## Experimental, nonproduction window spike

A deliberately unwired draft window service with bloom and Gio adapters lives
under `internal/experience/experimental/windows`. Run their standalone
demonstrations:

```sh
go run ./internal/experience/experimental/windows/cmd/windowdemo
go build -tags giodemo ./internal/experience/experimental/windows/cmd/giowindowdemo
```

It hosts the existing Screen and Pad SDK presentation fixtures in two titled
windows with border `[X]` close controls. Drag a title or use **Alt-arrows** to
move one window, click a window to raise/focus it, use **F6** to switch focus,
**Tab/Shift-Tab** to move
between that window's controls, type in its field, **Enter** to activate an
action or grid cell, and **Ctrl-W** to close the focused window. **Ctrl-C**
exits and restores the terminal. The two drafts and semantic action requests
remain independent, and lifecycle events use the existing typed event bus.

This is not production multiwindow support and is not registered with the
normal shell or runtime. The Gio demo's `giodemo` build tag keeps the
cgo-bound `gioui.org/app` host out of default builds; building it verifies
compile only, not a running window. Demos keep fixture actions local rather
than sending app IPC, implement only the presentation elements the fixtures
use, and the terminal demo is most useful at 80×24 or larger. The
toolkit-neutral draft and the observed side-by-side bloom/Gio leak inventory
are in
[docs/architecture.md §4.6](docs/architecture.md#46-desktop-toolkit--experimental-bloom-and-gio-adapters-decision-deferred).

## Checks

Run focused experience and experimental tests during development:

```sh
go test -count=1 ./internal/experience/... ./test/e2e
go test -count=1 ./internal/experience/experimental/windows/...
go test -count=1 ./internal/experience/shell -run '^(TestSteepRuntimeScenarios|TestSteepMotionScenarios)$'
```

The steep scenarios boot a real runtime in an emulated terminal and cover
navigation, launch/stop, palette, echo, resize, bracketed paste, mouse, toast
geometry, finite motion, and terminal restoration. Snapshot updates are always
explicit developer actions, never automatic in CI. Real local PTY and OpenSSH
checks remain separate release validation; see
[docs/development.md](docs/development.md).

Before submitting a change, run the complete build, vet, race, formatting,
pure-Go, and Windows/Linux cross-build gate:

```sh
go build ./... && go vet ./... && go test -race ./... && test -z "$(gofmt -l .)" && CGO_ENABLED=0 go build ./...
GOOS=windows go build ./... && GOOS=linux go build ./...
```

CI executes the tests on Ubuntu, macOS, and Windows. Cross-compilation alone is
not treated as proof of Windows runtime behavior.

## Material limitations and approved deviations

- bloom and steep are pre-1.0, exactly pinned dependencies with expected
  breaking-change risk; upgrades require deliberate review and three-OS tests.
- bloom alpha.6 clipboard helpers emit OSC 52 even when their callback denies a
  copy. Gostalgia therefore uses public selection data plus authenticated,
  asynchronous `clipboard/write` IPC instead. Host writes obey operator policy,
  including over SSH, and no OSC 52 fallback is used. The upstream issue remains
  an unsubmitted draft.
- The experimental window comparison now has observed adapters for both bloom
  and Gio (`gioui.org v0.10.3`, experimental-only). Its leak inventory is in
  [docs/architecture.md §4.6](docs/architecture.md#46-desktop-toolkit--experimental-bloom-and-gio-adapters-decision-deferred);
  it does not cover `gioui.org/app`'s native windowing, which needs cgo on
  macOS and display headers on Linux.
- `status.md` remains the canonical milestone tracker. Its stale Charm baseline
  row is separate tracker maintenance in
  [#217](https://github.com/drawmeanelephant/gostalgia/issues/217), not part of
  the experience feature changes.

## License

MIT — see [LICENSE](LICENSE), which also credits the Charm ecosystem and other
dependencies that power the terminal experience. The bundled FIGlet `big` and
`mini` fonts used by Home are BSD-3-Clause; see the
[font attribution](docs/shell.md#home-wordmark-and-ascii-chrome-issue-214).
