# Runtime

The runtime (`internal/runtime`, booted by `cmd/gostalgia`) owns the
environment's lifecycle. The default CLI entrypoint owns boot plus the
[Charm shell](shell.md); `gostalgia boot` remains headless. The runtime package
itself remains standard-library-only and does not import the experience layer.

## Environment root

Everything an environment owns lives under one root directory:

```text
~/.gostalgia/                  (default; override with --root or $GOSTALGIA_ROOT)
├── config/
│   └── system.json          persisted configuration
├── logs/
│   └── gostalgia.log          append-only runtime log
├── runtime.json             how to reach the live environment (endpoint+token)
└── vfs/                     backing store for the environment filesystem "/"
    ├── users/guest/{documents,downloads,desktop,config}
    ├── apps/manifests/      application manifests
    ├── data/
    └── mounts/
```

`gostalgia init` creates the layout; `Boot` creates it idempotently.

## Boot sequence

`runtime.Boot(ctx, Options)` performs, in order:

1. Resolve the root (flag → `$GOSTALGIA_ROOT` → `~/.gostalgia`) and create the layout.
2. Load `config/system.json` (missing file = empty store; defaults are in code).
3. Open the log (stdout + `logs/gostalgia.log`; `--verbose` for debug level).
4. Construct the event bus, IPC router, VFS (host-backed, `/tmp` mounted as memfs),
   process manager, session manager, app registry (+ builtin factories, + manifests
   loaded from `/apps/manifests`), and the app manager.
5. Generate the IPC token; refuse to boot if another live environment already owns
   the root (stale `runtime.json` whose endpoint does not answer is ignored).
6. Register and start core services (`sys`, `process`, `fs`, `ipc`) in dependency
   order; a failed start rolls back everything already started.
7. Create user `guest` and open `session-1`.
8. Launch `com.gostalgia.echo` (the first application).
9. Self-test: one in-process IPC round trip (`sys/ping`); only then is the
   runtime "ready" and serving.

## Shutdown

`Runtime.Shutdown(reason)` runs exactly once and blocks until finished:

1. Stop all applications (5 s stop timeout each).
2. Stop any remaining processes (child procs included).
3. Close all sessions.
4. Stop services in reverse start order (10 s stop timeout each; the ipc service
   closes the listener and drops idle connections — shutdown never hangs on a
   connected client).
5. Remove `runtime.json`, flush and close the log file.

Shutdown is triggered by SIGINT/SIGTERM (handled by `cmd/gostalgia`) or by the
`sys/shutdown` IPC method.

## runtime.json

Written when the IPC service starts, removed at shutdown:

```json
{"pid": 123, "version": "0.1.0", "endpoint": "unix:///var/folders/.../gostalgia-ipc-XXXX/gostalgia-1a2b3c.sock",
 "token": "...", "started_at": "2026-10-04T..."}
```

`gctl` reads it to find and authenticate to the environment.

## Logging

`slog` text format to stdout and the log file. Boot, service state changes,
process lifecycle, sessions, and applications log at info; every event on the
bus is mirrored at debug. The log file is opened append-only; rotation is
future work.

`Options.LogOutput` lets an experience-layer host replace console output.
The shell passes `io.Discard`, retaining file logs without corrupting terminal
rendering. This boundary uses a standard-library interface, not Charm types.
