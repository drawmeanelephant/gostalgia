# Gostalgia

**Gostalgia** — *Go* + *nostalgia*: a computing environment with the soul of
the machines we grew up on, built in Go.

A cross-platform pseudo-operating system / portable computing environment.
It runs hosted on Windows, macOS, or Linux but presents its own
abstractions for applications, processes, files, services, IPC, configuration,
sessions, and permissions. The host OS is substrate, not the application model.

**Status: M1 (runtime vertical slice) complete — live tracker in
[status.md](status.md).** The environment boots, starts its core services,
opens a user session, launches and supervises an application, serves a local
IPC endpoint, and shuts down cleanly — with tests at every layer.

## Try it

```sh
go run ./cmd/gostalgia shell --root /tmp/gs     # gorgeous DOS-style Charm shell
# Or run headless with the existing control client:
go run ./cmd/gostalgia boot --root /tmp/gs      # terminal 1 — boot the environment
go run ./cmd/gctl --root /tmp/gs status      # terminal 2 — inspect it
go run ./cmd/gctl --root /tmp/gs echo hello  # talk to the first application
go run ./cmd/gctl --root /tmp/gs ps
go run ./cmd/gctl --root /tmp/gs ls /users/guest
go run ./cmd/gctl --root /tmp/gs shutdown    # clean shutdown
```

## What is here

- **Runtime** (`internal/runtime`) — boot, wiring, graceful shutdown.
- **Service framework** (`internal/service`) — dependency-ordered lifecycle,
  failure rollback, reverse-order stop.
- **Core services** (`internal/services`) — `sys`, `process`, `fs`, `ipc`.
- **Process model** (`internal/process`) — explicit in-proc vs child kinds,
  states, events, stop timeouts.
- **Packages** (`internal/pkg`) — bounded archive verification, publisher
  provenance, atomic install/update/rollback, and permission inspection through
  `gctl pkg` and shell `pkg`/`package`. See [docs/packages.md](docs/packages.md).
- **IPC** (`internal/ipc`) — one router, in-proc + NDJSON socket transports,
  token handshake, capability-carrying call contexts.
- **VFS** (`internal/vfs`) — environment-rooted filesystem with mounts,
  host backend confined via `os.Root`, memfs `/tmp`; `testing/fstest`-verified.
- **Charm shell** (`internal/experience/shell`) — Bubble Tea + Lip Gloss,
  DOS-style prompt, app shelf, command history, completion, scrollback; IPC only.
- **App SDK** (`sdk`, `internal/app`, `apps/`) — embedded JSON manifests,
  Init/Run/Stop, capability-scoped service calls and routes, single-instance
  launch, failure cleanup. Echo is the single copyable demo.
- **Events, config, sessions, capabilities** — the small primitives the rest
  builds on.

The runtime core and app SDK are standard-library-only. The terminal experience
uses the allowed Charm family (and its transitive dependencies), without cgo.
See [docs/applications.md](docs/applications.md) for the complete app-writing
spec, [docs/shell.md](docs/shell.md) for the terminal experience, and
[docs/architecture.md](docs/architecture.md)
for the design, decisions, milestone arc, and risks; subsystem details in
[docs/](docs/); the itemized milestone tracker and current state in
[status.md](status.md).

## Checks

```sh
go build ./... && go vet ./... && go test -race ./...
gofmt -l .  # empty output = clean
```

Requires Go ≥ 1.25.

## License

MIT — see [LICENSE](LICENSE), which also credits the Charm ecosystem and other
dependencies that power the terminal experience.
