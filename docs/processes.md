# Processes

The environment's process model lives in `internal/process`.

## The two kinds

| | `inproc` | `child` |
|---|---|---|
| What it is | Application/service logic running inside the runtime | A real host child process |
| Mechanism | goroutine + cancellable context | `os/exec` + `CommandContext` |
| Isolation | logical only (a panic can affect the runtime) | host OS process |
| Exit status | error returned by `Run` | exit code, signal status |
| Stop semantics | context cancellation + timeout | graceful signal + stop deadline, then tree kill |

The distinction is explicit in the API (`process.Kind`), in `Spec`, in `Info`,
in IPC payloads, and in `gctl ps` output. Nothing treats a goroutine as an OS
process.

## Lifecycle

States: `starting → running → stopping → stopped` (or `failed`).

- `Manager.StartInProc(ctx, spec, run)` — starts in-process logic; `run` must
  honor `p.Context()`. Each process runs once under the manager's supervision.
- `Manager.StartChild(ctx, spec)` — spawns `spec.Args` in an isolated process
  group, inheriting or replacing the environment (`spec.Env`), optional working
  directory.
- `Manager.Stop(id, timeout)` — sets `stopping`, then delivers the graceful
  stop signal for the process kind and waits up to the timeout for the process
  to exit on its own before escalating to a tree kill.
- Exited processes record exit diagnostics in bounded history buffer; inactive
  in-memory processes are reaped manually via `Reap()` or automatically when
  inactive counts exceed limits.

## Supervision and the stop contract

The process manager supervises every process for its whole lifetime. Each
process runs exactly once — there is no automatic restart policy: an exited
process transitions to `stopped` or `failed`, and restarting an application is
an app-manager concern that re-runs the full launch path.

- **Single run:** Failure transitions to `failed`, a normal exit to `stopped`,
  and an operator stop to `stopped`. Nothing re-runs a finished process.
- **Graceful stop phase:** `Stop` first delivers the graceful signal for the
  process kind and waits up to the timeout for a voluntary exit:
  - `inproc`: the process context is canceled; `run` must honor it.
  - external app children: the child's IPC connection is closed, so the SDK's
    read loop sees EOF, cancels the app context, returns from `Run`, and calls
    `Instance.Stop(ctx)` — state persisted in `Stop` is not lost.
  - other children: `platform.TerminateProcessTree` sends `SIGTERM` to the
    child's process group on Unix. (Windows has no graceful group signal for
    detached children; external apps still stop gracefully via the conn close.)
- **Kill escalation:** If the stop deadline passes without an exit,
  `KillProcessTree` ends the whole tree — `SIGKILL` to the process group on
  Unix, the child's Job Object on Windows.
- **Shutdown gate:** Once `BeginShutdown`/`StopAll` marks the manager as
  draining, `StartInProc` and `StartChild` refuse to register new processes
  (callers undo the refused start), so no process can be created during or
  after the shutdown sequence and slip past `StopAll` as an orphan.

## Process tree isolation and cleanup

For real host child processes (`KindChild`), runaway child processes and their
descendants are managed via platform process group adapters (`platform.SetupProcessTree`
and `platform.KillProcessTree`):

- **Process group isolation:** Child processes are launched in their own process
  group (`Setpgid: true` on Unix). On Windows each started child is enrolled in
  a dedicated Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, so it and
  every descendant it spawns die together; the assignment happens right after
  `cmd.Start`, so a descendant spawned in the first instructions can outlive
  the job — that bounded startup window is the only gap.
- **Descendant termination:** On `Stop()` or shutdown, `KillProcessTree` sends
  `SIGKILL` to `-pid` (the process group) on Unix, or terminates the child's
  Job Object on Windows, killing any background workers or grandchildren that
  remained in the tree. The group signal
  is only sent while the group provably still belongs to the child (alive or
  unreaped zombie, or a group outliving a freed leader pid), so a pgid
  recycled after `Wait` cannot redirect the kill to an unrelated group.
  A descendant that escaped the group (`setpgid`/`setsid`) is only reachable
  while still linked by ppid — `KillDescendants` (used for `strict` children
  with `DenyDescendants`) sweeps both sets on Linux, re-verifying each
  candidate's `/proc` start time before signaling so a pid or pgid recycled
  mid-sweep is never signaled. On macOS, `strict` children cannot create
  descendants at all (`deny process-fork`), and on Windows sandboxed
  execution fails closed, so no confined descendant can exist.

## Bounded exit history and process reaping

- **Bounded history buffer:** A thread-safe, bounded ring buffer (`HistoryEntry`)
  records terminal states, exit codes, durations, and error
  diagnostics for terminated processes. Default capacity is 100 entries
  (`DefaultMaxHistory`), evicting the oldest entries when full.
- **Process reaping:** Live process structs can be pruned from memory via
  `Manager.Reap()` or automatic background reaping (`autoReap()`), freeing
  active process table slots while retaining diagnostic logs and exit history.

## Process inspection and IPC (`proc/list`, `proc/info`, `proc/logs`, `proc/history`, `proc/reap`)

The `proc/list` IPC endpoint returns complete `process.Info` snapshots,
including:
- `id`, `name`, `kind`, and `state`
- `session` and `user` ownership
- `caps` (granted capabilities snapshot)
- `started_at` and `exited_at` timestamps
- `error` (failure description)
- `exit_code` (for both child processes and exited in-proc runs)

The `proc/info` IPC endpoint returns this snapshot for a single process ID,
falling back to the bounded history buffer if the process was already reaped.

The `proc/history` IPC endpoint returns recent terminated process entries from the
bounded history buffer.

The `proc/reap` IPC endpoint prunes inactive process entries from the active
process table (requires `proc:stop` capability).

The `proc/logs` IPC endpoint returns captured process logs and stream diagnostics:
- Process identification (`id`, `name`, `kind`, `state`, `exit_code`)
- Lifecycle timings (`started_at`, `exited_at`, `duration`)
- `stdout`, `stderr`, and `combined` stream diagnostics containing:
  - `total_bytes`: total cumulative bytes written to the stream
  - `buffered_bytes`: current un-dropped bytes retained in the ring buffer
  - `dropped_bytes`: count of older bytes dropped due to capacity limits
  - `truncated`: boolean flag indicating if buffer overflow occurred
  - `content`: buffered stream text snapshot
- Parameters:
  - `id` (required): target process ID
  - `stream` (optional): filter by stream (`stdout`, `stderr`, or combined default)
  - `tail` (optional): limit returned content to the last N lines

### Child output capture and bounding

Child process (`KindChild`) standard output and standard error are captured via
bounded memory ring buffers (`process.RingBuffer`) defaulting to 64KB per stream
(configurable via `spec.LogLimit`).

- **Non-blocking FIFO drops:** When incoming output exceeds capacity, the oldest
  bytes are dropped, tracking exact byte counts and truncation flags without
  blocking child execution or causing unbounded memory growth.
- **WaitDelay protection:** `cmd.WaitDelay = 2 * time.Second` prevents child
  processes from leaking background I/O handles or stalling manager shutdown.
- **Child environment sanitization:** `DefaultChildEnv` deliberately whitelists
  safe system variables (`PATH`, `TMPDIR`, `HOME`, etc.) and sets `GOSTALGIA_*`
  runtime variables, explicitly filtering out sensitive host credentials,
  tokens, and private keys.

### Diagnostics and presentation

Exit codes, timings, and logs are integrated across developer and interactive tools:
- `gctl ps`: displays `PID`, `NAME`, `KIND`, `STATE`, `EXIT` code, and runtime `TIME` duration.
- `gctl history`: displays bounded exit history, terminal states, run durations, and exit errors.
- `gctl reap`: manually reaps inactive process records from the runtime.
- `gctl logs <pid> [tail]`: displays process diagnostics, stream byte metrics, drop counters, and captured output with terminal control-character sanitization.
- Charm shell `ps`: displays process names, states with exit codes, run durations, and capability grants.
- Charm shell `logs <pid> [tail]`: displays process diagnostics, byte counts, and sanitized stdout/stderr output.
- Live Task Manager (`F5` or `tasks`/`taskmanager`/`top`): full-screen interactive process table with real-time resource usage, log inspection modal, termination (`x`), and inactive process reaping (`r`).
- Crash Receipts (`receipt [pid]`): post-mortem exit snapshots for failed processes containing exit codes, failure causes, and bounded, sanitized log excerpts.
- Notification Center (`F6` or `notifications`/`alerts`): captures process crash and completion events with transient toast overlays above the prompt and Do-Not-Disturb (`dnd`) toggle.

## Events

Every state transition publishes `proc.state`
(`process.Event{ID, Name, Kind, State, Err}`) on the event bus. The transition
to a terminal state is guaranteed to be published before `Process.Done()`
closes, so an observer that sees `Done()` can rely on the final event having
been delivered.

## Ownership

A `Spec` carries the owning session and user and the capability list granted to
the process. `Process.Context()` replaces inherited caller capabilities with
that spec grant, and `Info.Caps` exposes a defensive snapshot over IPC.
Applications get their capabilities from their manifest. SDK lifecycle panics
are converted to errors and cleanup always runs; the process manager itself
still relies on direct non-app callbacks to behave. See
[applications.md](applications.md).
