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

The duplicate-boot guard is an exclusive file lock at `<root>.lock` —
beside the root, so a refused boot never creates or mutates the root. It
is held for the process lifetime and released by close or process death,
so a crashed instance never wedges its root.

## Boot sequence

`runtime.Boot(ctx, Options)` performs, in order:

1. Resolve the root (flag → `$GOSTALGIA_ROOT` → `~/.gostalgia`) and take the
   exclusive `<root>.lock` — before any root mutation. A locked root fails
   fast. Then refuse to boot if the recorded `runtime.json` still points at a
   live instance: a stale file whose endpoint does not answer is ignored, a
   bound endpoint that cannot complete the auth handshake counts as occupied
   (it is how a live instance with a dead accept loop presents), and only a
   responsive listener that rejects the recorded token is foreign.
2. Create the layout, load `config/system.json` (missing file = empty store;
   defaults are in code), and open the log (stdout + `logs/gostalgia.log`;
   `--verbose` for debug level).
3. Construct the event bus, IPC router, VFS (host-backed, `/tmp` mounted as memfs),
   process manager, session manager, app registry (+ builtin factories, + manifests
   loaded from `/apps/manifests`), and the app manager.
4. Generate the IPC token.
5. Register and start core services (`sys`, `process`, `fs`, `ipc`) in dependency
   order; a failed start rolls back everything already started.
6. Create user `guest` and open `session-1`.
7. Launch `com.gostalgia.echo` (the first application).
8. Self-test: one in-process IPC round trip (`sys/ping`); only then is the
   runtime "ready" and serving.

## Shutdown

`Runtime.Shutdown(reason)` runs exactly once and blocks until finished:

1. Stop all applications (5 s stop timeout each).
2. Stop any remaining processes (child procs included).
3. Close all sessions.
4. Stop services in reverse start order (10 s stop timeout each; the ipc service
   closes the listener and drops idle connections — shutdown never hangs on a
   connected client).
5. Remove `runtime.json` — only if its pid and token still match this instance,
   so an exiting instance never unlinks a live survivor's record — then flush
   and close the log file, and release the `<root>.lock` claim last.

Shutdown is triggered by SIGINT/SIGTERM (handled by `cmd/gostalgia`) or by the
`sys/shutdown` IPC method.

## SSH attach (`--ssh`)

`gostalgia boot --ssh ADDR` additionally serves the shell over SSH
(`internal/experience/sshx`). A bare port or empty host binds loopback; an
explicit host may bind other interfaces. Authentication is public-key only
against `<root>/ssh/authorized_keys` (required — the listener refuses to
start without it); a key's `gostalgia:user=<profile>` comment tag scopes it
to that profile's session. The host key is generated once at
`<root>/ssh/ssh_host_ed25519_key`. Each accepted session is a normal
`session/attach` client with `client_type: "ssh"` — equivalent to running
`gostalgia attach` remotely. See the SSH section of
[docs/security.md](security.md) for the threat model.

## runtime.json

Written atomically (unique staging file + rename) when the IPC service
starts, removed at shutdown only while it still records this instance:

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
