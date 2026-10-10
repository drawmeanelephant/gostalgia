# Security & Threat Model

Gostalgia is a desktop-like local application runtime. It executes built-in and
third-party applications under capability-based authority, virtual filesystem
isolation, process supervision, and platform OS sandboxing.

This document defines the system's security architecture, trust boundaries,
threat model, credential lifecycle, isolation levels, and host limitations.

## Threat Model and Trust Boundaries

The runtime distinguishes three classes of actors:

```
+--------------------------------------------------------------------------+
|                              Host Machine                                |
|                                                                          |
|  +---------------------------+       +--------------------------------+  |
|  |     Operator Clients      |       |      External Applications     |  |
|  |  (Charm Shell, gctl CLI)  |       |   (Sandboxed / Strict / Child) |  |
|  +-------------+-------------+       +---------------+----------------+  |
|                |                                     |                   |
|       Operator Token (Admin)                 Scoped App Token            |
|                |                                     |                   |
|                v                                     v                   |
|  +--------------------------------------------------------------------+  |
|  |                      Gostalgia Runtime Core                        |  |
|  |                                                                    |  |
|  |  * IPC Router & Handshake Verifier    * Process Supervisor         |  |
|  |  * TokenStore & Capability Scoper     * VFS Grant & Mount Manager  |  |
|  +--------------------------------------------------------------------+  |
|                                                                          |
|  +--------------------------------------------------------------------+  |
|  |                       Local Attacker Boundary                      |  |
|  |  * Unauthenticated socket clients     * Rogue host processes       |  |
|  |  * Replayed / revoked tokens          * Malformed protocol frames  |  |
|  +--------------------------------------------------------------------+  |
+--------------------------------------------------------------------------+
```

### 1. Operator Trust (High Trust)

- **Definition:** The human user or administrator running Gostalgia, interacting
  via the interactive Charm terminal shell or the `gctl` CLI tool.
- **Authority:** Full system authority (`admin` capability set).
- **Capabilities:** Operators can launch and terminate processes, manage
  filesystem capability grants (`fs/grant`, `fs/grant/revoke`), mount shared host
  folders (`hostfs/mount`), configure system services and operator policies (`sys/policy`),
  inspect process listings and logs, and issue system shutdown.
- **Authentication:** At boot, the runtime creates a random 256-bit operator
  token written to `<root>/runtime.json`. Operator tools read this token to
  authenticate their IPC connection. Socket clients authenticating with the
  operator token receive the `admin` capability set and operator principal identity.

### 2. Application Trust (Untrusted to Semi-Trusted)

- **Definition:** Built-in or external third-party applications running within
  Gostalgia.
- **Authority:** Least privilege. Applications have **no ambient authority**.
  They cannot access the host filesystem, execute arbitrary processes, or
  invoke privileged system methods (`sys/shutdown`, `proc/stop`, `fs/grant`).
- **Capability Scoping:**
  - Manifest grants: Applications declare requested capabilities (`ipc`,
    `fs.read`, `fs.write`, `proc.list`, `proc.stop`, `app.list`, `app.launch`,
    `shutdown`, `config.read`, `config.write`, `clipboard.read`,
    `clipboard.write`, `hostfs.read`, `hostfs.write`, `net.egress`,
    `session.read`, `session.write`, `profile.read`, `profile.write`,
    `package.read`, `package.write`, `backup.read`, `backup.write`,
    `notify`, `sound`) in their JSON manifest. The runtime verifies that
    only permitted non-admin capabilities are requested.
  - Confused-deputy boundary: When an operator or another application invokes an
    application's exported handlers, the runtime executes that handler strictly
    under the application's declared grants. An operator calling an app does not
    lend it administrative capabilities.
  - App-private storage: Every app receives an automatic, isolated partition at
    `/apps/data/<app_id>`. Applications can only read, write, and list within
    their own partition. Other partitions are completely hidden and inaccessible.
  - Scoped VFS grants: Access outside an app's private partition requires an
    explicit, path-scoped grant issued by an operator (`fs/grant`) or by a
    document handoff (`doc/handoff`). Grants are checked per-operation and
    fail closed on revocation. Handoff grants are session-bound: they are
    revoked automatically when the receiving application's run ends.
    Grant scoping applies to every service an app can reach, not just `fs/*`:
    `backup/*` operations run entirely inside the caller's scoped view, and
    `doc/recents/add`/`doc/favorites/add` ignore paths the caller cannot read
    so the shared stores and search index never absorb ungranted entries.

### 3. Local-Attacker Threat Model (Adversarial)

The runtime defends against local threats operating on the host machine:

- **Unauthenticated socket connections:** An attacker connecting to the local
  IPC socket cannot invoke any RPC method without authenticating first. The
  first message must be an `auth` handshake request with a valid token.
  Non-auth requests, missing tokens, or invalid tokens are immediately rejected
  and the connection is dropped.
- **Connection and handshake budgets:** Connections are subjected to strict
  budgets. Handshake must complete within the handshake deadline (5 seconds).
  Exceeding the maximum connection budget (256 concurrent connections) or
  flooding the server with in-flight requests (> 32 per connection) terminates
  the offending connection immediately.
- **Socket path privacy:** On Unix, all IPC sockets — the operator listener
  and the per-app child listeners — live inside a per-boot private directory
  (`$TMPDIR/gostalgia-ipc-*`, mode `0700`), never at a predictable or shared
  temp path. No outside process can pre-plant an occupant to block boot or
  app launch, and on Linux the directory is additionally hidden under the
  child's fresh `/tmp` tmpfs while on macOS it is a masked path.
- **Malformed frames and oversized payloads:** The NDJSON framing scanner
  enforces a 4 MiB frame size limit (`maxLine`). Malformed JSON, non-object
  payloads, unterminated lines, duplicate request IDs, and negative IDs fail
  cleanly without crashing the server or exhausting memory.
- **Stale credential replay:** When an application process exits or is stopped,
  its scoped token is immediately revoked in `TokenStore`. In-flight requests
  fail, active connections are terminated, and subsequent handshake attempts
  using the stale token are rejected.
- **Cross-process credential leakage:** Environment variables of external
  processes are scrubbed (`process.CleanEnv`), stripping host environment
  variables and parent tokens. Only the app's dedicated `GOSTALGIA_APP_TOKEN`
  is passed to the child process.
- **Uncooperative or hostile application processes:** Applications that ignore
  cancellation, block shutdown, or attempt CPU/memory exhaustion are bounded by
  supervisory timeouts and OS resource limits. If an application fails to exit
  within its stop deadline, the process supervisor sends `SIGKILL` to the entire
  process group (`platform.KillProcessTree`), preventing zombie or orphan escape.

---

## Credential Handling & Lifecycle

### Token Generation
All tokens (operator tokens and application tokens) are generated using
cryptographically secure pseudo-random bytes (`crypto/rand`) encoded as 64-character
hex strings (256 bits of entropy).

### Operator Credential
- Generated once per runtime boot.
- Persisted in `<root>/runtime.json` with file mode `0600` on Unix systems.
- Never exposed in process listings (`proc/list`, `proc/info`) or debug logs.
- Grants full `admin` capabilities and operator principal identity.

### Application Credentials
- Generated dynamically per application launch by `internal/security.TokenStore`.
- Strictly bound to the tuple `(app_id, proc_id, session_id, user, capabilities)`.
- Delivered to external child processes via the `GOSTALGIA_APP_TOKEN` environment
  variable over a dedicated IPC socket.
- Never written to disk or `runtime.json`.
- Scrubbed from public process inspection endpoints.
- **Revocation:** Bound to the process lifecycle. As soon as the application
  stops, crashes, or is terminated by the supervisor, `TokenStore.RevokeProcess`
  invalidates the token. Any existing socket connections using that token are
  immediately disconnected, and subsequent calls fail with `unauthorized:
  credential revoked`.

### Workspace Profiles and Identity Attribution

Personal workspace profiles (`internal/profile`) provide multi-user isolation within Gostalgia:
- **Principal User Binding:** When an application is launched, its process specification (`process.Spec.User`), token credentials (`security.Credential.User`), and session ownership (`session.Session.User`) are strictly bound to the active profile's `security.User`.
- **Operator Identity Synchronization:** When an operator switches profiles via `profile/switch`, `TokenStore.SetOperatorUser` updates the operator credential's user identity in-place. Active IPC connections dynamically re-evaluate the connection principal on each dispatch, ensuring live profile switches update active operator sessions and context without requiring socket reconnection.
- **Profile Capabilities:** Access to profile management endpoints is guarded by capability checks:
  - `profile.read`: Required for `profile/list`, `profile/get`, and `profile/active`.
  - `profile.write`: Required for `profile/create`, `profile/update`, `profile/switch`, and `profile/delete`.
- **Workspace Directory Isolation:** Each profile receives a segregated directory hierarchy rooted at `/users/<profile_id>/` with subdirectories for `documents`, `downloads`, `desktop`, `config`, and `.trash`. Document search indexing and recent/favorite lists are partitioned per profile.

---

## Isolation Levels

Gostalgia supports four explicit isolation levels declared in application manifests:

| Level | Boundary | Network Egress | Descendant Fork | Host FS Writes | Resource Limits |
|---|---|---|---|---|---|
| `inproc` | Logical only (SDK capability filters) | Allowed (host) | N/A (same process) | Allowed (host) | None |
| `trusted` | Process boundary (sanitized env, process group) | Allowed (host) | Allowed (supervised) | Permitted (user perms) | None |
| `sandbox` | OS container / sandbox confinement | **Denied** (kernel/profile) | Allowed (supervised) | Restricted | Memory & FD caps |
| `strict` | Maximal containment | **Denied** (kernel/profile) | **Denied** (blocked/killed) | **Denied** (read-only) | Strict memory, FDs, CPU |

### Detailed Confinement Mechanics

#### 1. `inproc` (In-Process)
- Application code is compiled directly into the runtime and executed in a
  goroutine.
- SDK adapters (`sdk.Context`) enforce capability checks logically.
- **Limitation:** Shares the runtime's memory space and process address space.
  A rogue in-process component could theoretically use unsafe memory access or
  runtime manipulation. Untrusted applications must never run `inproc`.

#### 2. `trusted` (Supervised Child Process)
- Spawns out-of-process as a separate OS process via `os/exec`.
- Environment is sanitized via `process.CleanEnv`, stripping parent environment
  variables and secrets.
- Allocated a new process group (`Setpgid: true` on Unix; `CREATE_NEW_PROCESS_GROUP`
  on Windows) for clean teardown via `platform.KillProcessTree`.
- Operates under standard OS user privileges without sandbox restrictions.

#### 3. `sandbox` (OS Sandboxing)
- **Linux:**
  - Unprivileged user and network namespaces (`CLONE_NEWUSER | CLONE_NEWNET`)
    block all host network egress at the kernel level without requiring root or
    setuid helpers.
  - A mount namespace (`CLONE_NEWNS`) is set up by a re-exec init hook
    (`/proc/self/exe` + `GOSTALGIA_SANDBOX_INIT` payload): masked paths (the
    environment root holding `runtime.json`, operator token) are hidden under
    empty tmpfs/`/dev/null` bind mounts, and `AllowedPaths` are bind-mounted.
    Because `DenyNetwork` is in force, `/tmp`, `/var/tmp`, `/run`, and
    `/var/run` are also covered with fresh tmpfs so no host unix socket
    (docker.sock, ssh-agent, the runtime's IPC socket) is reachable by path.
  - Resource ceilings are enforced via `prlimit64`:
    - Virtual memory address space (`RLIMIT_AS`) capped at 2 GB.
    - Maximum open file descriptors (`RLIMIT_NOFILE`) capped at 1024.
- **macOS:**
  - Generates a custom Apple Seatbelt profile executed via `/usr/bin/sandbox-exec`.
  - Denies all network operations (`(deny network*)`) with **no** unix-socket
    exceptions: Seatbelt cannot path-scope `remote unix-socket` for connect(),
    so child IPC is a pre-connected socketpair handed down as an inherited
    file descriptor (`GOSTALGIA_IPC_FD`) instead of a filesystem socket the
    child dials. A confined child can neither reach `docker.sock`/`ssh-agent`
    nor open new connections to the runtime's own IPC endpoint.
  - Reads are confined to an execution allowlist (system libraries, frameworks,
    temp dirs, the executable itself, and `AllowedPaths`); `MaskedPaths`
    (including the environment root) are carved out of covering prefixes with
    `require-not` filters. Host filesystem writes are permitted except into
    masked paths. The runtime's per-boot IPC socket directory is masked the
    same way, so a confined app cannot discover or unlink live socket files.
  - The declared boundary is enforced, not merely documented: if the host
    lacks `sandbox-exec` the launch fails closed.

#### 4. `strict` (Maximal OS Confinement)
- Enforces all protections of `sandbox`, plus:
  - **Descendant Process Prevention:**
    - macOS: Enforces `(deny process-fork)` in Seatbelt profile. Any call to
      `fork()`/`posix_spawn()`/`execve()` immediately fails with `EPERM`.
      This is hard prevention — `KillDescendants` is intentionally a no-op
      because no confined descendant can exist.
    - Linux: the supervisor sweeps `/proc` on a poll and kills both members
      of the child's process group (descendants inherit the group, catching
      reparented grandchildren and fork+exit races) and transitive
      ppid-tree descendants (catching `setpgid`/`setsid` escapees).
      Residual: a descendant that both leaves the group and orphans itself
      within one poll interval can evade tracking until teardown, where
      `KillProcessTree` SIGKILLs the whole group.
  - **Read-Only Filesystem:**
    - macOS: Disallows filesystem write operations (`(deny file-write*)`)
      except `/dev/null`, `/dev/zero`, temp/scratch dirs, and `AllowedPaths`.
    - Linux: every real mount is bind-remounted read-only inside the mount
      namespace; `AllowedPaths` are re-mounted writable. `MaskedPaths` stay
      hidden.
  - **Tighter Resource Limits:**
    - Maximum file descriptors capped at 512 (`RLIMIT_NOFILE`).
    - Virtual memory capped at 2 GB (`RLIMIT_AS`).
    - CPU execution budget enforced via `RLIMIT_CPU`.

### Fail-Closed Execution Guarantee

If an application declares `sandbox` or `strict` isolation and the host system
cannot satisfy the required enforcement mechanisms:
- The runtime **fails closed** immediately, returning `ErrSandboxUnsupported`.
- The runtime **never silently degrades** to `trusted` or `inproc` execution.
- Host capability status is transparently reported by `platform.GetHostSecurityCapabilities()`
  and queryable via `sys/status`.
- `proc/info` and `proc/list` report the effective `isolation` level and active
  `policy` parameters for every process.
- Process termination (`platform.KillProcessTree`) kills the entire process group
  (`SIGKILL` to `-pid`), ensuring no orphan descendant processes can escape.

### Opt-in Platform Integration and Operator Policies

Host integration adapters (clipboard, shared folders, network egress) default to
strictly disabled/internal-only and require two distinct levels of authorization:
1. **Application capability declaration:** The app must declare the corresponding
   capability token (`clipboard.read`, `clipboard.write`, `hostfs.read`, `hostfs.write`,
   or `net.egress`) in its manifest.
2. **Explicit operator policy:** The operator must explicitly enable the integration
   in `OperatorPolicy` (`internal/security.PolicyStore`), inspected via `sys/policy`
   and updated by operator clients via `sys/policy/update`:
   - **Clipboard (`clipboard.*`):** When disabled, clipboard reads and writes operate
     purely within an internal, in-memory clipboard buffer. When enabled, incoming and
     outgoing data is strictly sanitized (`platform.SanitizeClipboard`), stripping
     terminal control sequences, DCS sequences, and OSC 52 sequences (which could hijack
     the host terminal or exfiltrate clipboard state) before interacting with the host clipboard.
   - **Shared host folders (`hostfs.*`):** Host paths cannot be accessed directly; they
     must be explicitly mounted by an operator via `hostfs/mount`. Backed by `SharedHostFS`,
     shared mounts use Go 1.24 `os.Root` confinement to guarantee that path traversals and
     symlinks cannot escape the designated host folder root. If mounted read-only, all write
     operations fail closed with `ErrReadOnly`.
   - **Network egress (`net.egress`):** Outbound network access is disabled by default. When
     enabled by policy, requests via `net/fetch` are checked against operator-configured host
     whitelists (`allowed_hosts`), blacklists (`blocked_hosts`), port filters (`allowed_ports`),
     and HTTPS-only transport requirements (`allow_insecure: false`). In addition, for external
     sandboxed apps, host network access at the kernel/sandbox level is denied unless both the app
     manifest grants `net.egress` and the operator policy permits network egress.

Audio output (`sound` capability, `sound/play`) is intentionally not part of
the opt-in policy set: it carries output-only, bounded, synthesized PCM that
the runtime renders in-process. Applications pass only a declarative tone
sequence — a preset name or a capped list of `{frequency_hz, duration_ms,
wave}` notes (≤64 notes, ≤10 s total, 20–8000 Hz, fixed 22050 Hz 16-bit mono)
— never file paths, host commands, or unbounded audio. The platform adapter
hands a finished WAV clip to a discovered host player (`afplay`,
`pw-play`/`paplay`/`aplay`, or PowerShell `System.Media.SoundPlayer`) spawned
detached and reaped under a hard timeout, so playback can never block IPC.
Hosts without a player get an explicit `ErrAudioUnsupported` denial, not a
silent drop.

---

## Host Limitations & Unprotected Vectors

Honesty about security boundaries is essential. The following scenarios are
explicitly outside Gostalgia's protection guarantees:

1. **Same-UID Local Attacker:**
   On multi-user systems, an attacker who obtains code execution under the
   **same OS user account** that launched Gostalgia can inspect runtime files
   in `<root>/runtime.json`, attach debuggers (`ptrace`), or read process memory.
   The operator token is an authentication mechanism for local clients, not a
   boundary against the owning OS account.

2. **In-Process Applications:**
   `inproc` mode provides no memory isolation. Applications requiring isolation
   must be packaged as external executables and run under `sandbox` or `strict`
   modes.

3. **Windows Host Confinement:**
   Windows lacks unprivileged native sandboxing equivalents to Linux user
   namespaces or macOS Seatbelt without hypervisor-backed container runtimes.
   Consequently, requesting `sandbox` or `strict` on Windows **fails closed**
   by design. External applications on Windows run under `trusted` isolation.

4. **macOS Seatbelt Deprecation:**
   `/usr/bin/sandbox-exec` and Apple Seatbelt (Sandbox.kext) are deprecated by
   Apple and unsupported in modern App Store apps, although they remain
   functional on standard macOS installations. Future macOS versions may alter
   or restrict Seatbelt profile behavior.

5. **Cgroups vs. Process Rlimits:**
   On Linux, resource limits are enforced per-process via `prlimit64` rather than
   systemd cgroups v2 slices (to avoid requiring root or cgroupfs delegation).
   Consequently, memory limits constrain virtual address space (`RLIMIT_AS`)
   rather than resident set size (RSS).

6. **Linux Descendant Sweep Residual:**
   `KillDescendants` kills the child's process group and its transitive
   ppid-tree descendants. A sufficiently adversarial descendant that calls
   `setpgid`/`setsid` *and* orphans itself (parent exits) inside a single
   supervisor poll interval can evade tracking until the app is stopped —
   at which point the process-group `SIGKILL` in `KillProcessTree` still
   catches anything that stayed in the group. Only a fully escaped daemon
   survives, and only until teardown of a group it left. Blocking `fork`
   outright would require seccomp interception, which is not implemented.
   `PR_SET_CHILD_SUBREAPER` was deliberately rejected: reparenting managed
   children to the runtime races `os/exec`'s `Wait` and strands zombies.

7. **macOS File Metadata Leakage:**
   To resolve whitelisted paths, the Seatbelt profile grants
   `file-read-metadata` on their ancestor directories (e.g. `/private/var`).
   A confined app can `stat()` those directories — learning names and
   attributes one level up the whitelist trees — but cannot list them or
   read contents. Directory listing (`file-read-data`) remains denied.

---

## Security Verification & Test Coverage

The security guarantees described here are verified by an extensive test suite:

- **IPC Fuzzing & Framing Tests (`internal/ipc`):** Fuzzing and adversarial
  testing covering malformed NDJSON frames, oversized payloads (> 4 MiB),
  handshake budgets, flooding clients, duplicate request IDs, and slow writers.
- **Protocol & Path Parsing Tests (`internal/ipc`, `internal/vfs`, `sdk`):**
  Adversarial testing of dot-segment escapes (`..`), NUL-byte injection,
  backslash path traversal, reverse-DNS manifest ID validation, and unknown
  manifest fields.
- **Adversarial Test App Fixtures (`test/testapps/adversarial`):** Dedicated
  adversarial test binary exercising:
  - Unauthorized host filesystem access (probing `/etc/passwd`, `runtime.json`,
    and unauthorized host paths).
  - Cross-process credential leakage (verifying environment scrubbing).
  - Permission borrowing and privilege escalation attempts.
  - CPU and memory resource exhaustion attempts.
  - Uncooperative shutdown (ignoring termination signals and stop contexts).
- **Sandbox Guarantee Tests (`test/e2e/sandbox_test.go`, `test/e2e/adversarial_test.go`):**
  Automated tests verifying that disallowed operations fail under `sandbox` and
  `strict` policies, while behaving as expected under unrestricted (`trusted`)
  launches.

