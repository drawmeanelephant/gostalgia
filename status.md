# Gostalgia Status

The living status document: what works right now, how it is verified, what is
honestly missing, and the itemized milestone list. This is the **canonical
tracker** — `docs/architecture.md` §5 summarizes the milestone arc and points
here.

> **Workflow note:** Daily task dispatch, issue progress, and parallel agent
> workflows are tracked natively on GitHub (issues, milestones, and pull
> requests with `Fixes #XX`). To prevent git merge conflicts and serialization
> bottlenecks across parallel agents, **feature PRs do not edit `status.md`**.
> This document is reconciled on `main` at milestone completions or periodic
> project syncs.

Rules for updating this file (milestone syncs):

1. An item moves to ✅ only when it **boots and runs**, passes
   `go vet ./...` and `go test -race ./...`, and its documentation in `docs/`
   is updated to match reality.
2. The "Snapshot" section describes what a user can actually do today — not
   what exists in code but is unexercised.
3. Known gaps stay listed until they are fixed; nothing is deleted to make a
   milestone look complete.

---

## Snapshot: what works today

| Capability | State | Verified by |
|---|---|---|
| Boot → services → session → app launch → IPC → clean shutdown | ✅ working | `test/e2e` (real socket, real auth, real shutdown) |
| Service framework (dep-ordered start, rollback, reverse stop) | ✅ working | `internal/service` unit tests |
| IPC: in-proc + socket transports, token handshake, NDJSON | ✅ working | `internal/ipc` tests + `gctl` manual/e2e |
| IPC: concurrent calls, versioned subscriptions, bounded metadata-only audit history | ✅ working | `internal/ipc` race stress (slow clients, floods, malformed frames, reconnect/epochs, permissions); `test/e2e` real runtime lifecycle notifications and secret/document redaction |
| Process model: in-proc lifecycle, child spawn/kill/exit-status, bounded output capture, diagnostics | ✅ working | `internal/process` tests (ring buffer, drop accounting, env sanitization); `proc/list`, `proc/info`, `proc/logs` over IPC; `gctl ps`/`logs` and shell `ps`/`logs` |
| Process supervision: restart policies, crash-loop backoff, bounded exit history, reaping | ✅ working | `internal/process` supervision tests (`StateRestarting`/`StateCrashLoop`, sliding-window restarts, exponential backoff) |
| VFS: safe doc ops, atomic save + recovery fallback, copy/move, trash/restore, host/memfs backends, mounts | ✅ working | unit tests + e2e + failure injection + fstest |
| App SDK: embedded JSON manifest, Init/Run/Stop, scoped calls/routes, launch/stop | ✅ working | `sdk`, `internal/app`, services + shell socket lifecycle tests |
| External Go apps over the environment protocol | ✅ working | `internal/app` wire tests; bounded child-wire frames, in-flight dispatch, and redirect-hop limits |
| Isolation levels (`inproc`/`trusted`/`sandbox`/`strict`) with fail-closed execution | ✅ working | `platform` adapters + `test/e2e` sandbox/adversarial tests: Linux user/net/mount namespaces + `prlimit64`, macOS Seatbelt via `sandbox-exec` (inherited-FD IPC, no unix-socket egress), Windows fails closed |
| Charm shell: DOS-style prompt, app shelf, history/completion, VFS/process commands; bloom-laid-out frame with the real terminal cursor; transcript (10,000-line cap) and Task Manager log tails in bloom buffers | ✅ working | `internal/experience/shell` (real Bubble Tea + authenticated IPC); steep scenarios boot a real runtime in an emulated terminal (`TestSteepRuntimeScenarios`) |
| Shell experience: Home with FIGlet wordmark, searchable launcher; command palette, confirmations and toasts drawn as bloom layers | ✅ working | `internal/experience/shell` tests + golden files (`testdata/home-wordmark/`, `testdata/overlay/`) |
| Mouse input: click, wheel scroll, hover tooltips, select-to-copy; additive (every action keeps its key), `mouse on\|off` per shell, `accessibility.mouse` default | ✅ working | `internal/experience/shell` mouse tests (copies go through `clipboard/write` under operator policy, never OSC 52) + steep mouse scenario |
| Finite per-session motion: event-triggered effects ≤200 ms (the failed-launch badge shake); ReducedMotion attaches none; `motion on\|off` per shell | ✅ working | `internal/experience/shell` motion tests on injected clocks (`TestSteepMotionScenarios`) |
| Task Manager, notification center, crash receipts | ✅ working | `internal/experience/taskmanager`, `internal/experience/notifications`, `internal/experience/receipts` |
| Accessible terminal modes (monochrome, high-contrast) + layout regression coverage | ✅ working | `internal/experience/ui` golden tests |
| Visual language & component kit (tokens, panels, tabs, dialogs, badges) | ✅ working | `internal/experience/ui` + `internal/experience/theme` |
| Presentation contract: typed/versioned data+action elements, shell owns rendering (v2 adds text blocks, meters, grids) | ✅ working | `internal/app` presentation tests, shell presentation tests |
| Charm baseline & dependency fences: Go 1.27.3; Bubble Tea v2.1.1, Lip Gloss v2.0.6, Bubbles v2.2.1 (`charm.land/*/v2`); bloom `v0.0.0-alpha.6` (experience only); steep/reel `v0.0.0-alpha.12` (test only) | ✅ working | `test/e2e` dependency fences (`TestCoreDependencyBoundary`, `TestCharmRestrictedToExperience`, `TestSteepImportFence`, `TestBloomImportMatcher`, `TestAppsAndSDKUseOnlyPublicBoundary`, `TestApprovedCharmBaseline`, `TestNoStandaloneHostExecutables`) |
| Bundled apps: Echo, Files, Notes, Settings, Compendium, Calculator, Todo, Pomodoro, RSS, Weather, Sysmon, Markview, Adventure, Petwatch, Dogcalc, Musictoy | ✅ working | `apps/*` unit + acceptance tests (persistence, capability denial, corrupt-state healing, contract v1/v2 surfaces) |
| Event bus (typed synchronous handlers + bounded nonblocking metadata subscriptions/history) | ✅ working | `internal/events` tests, including ordered concurrent delivery, drop-oldest queues and 256-record retention |
| Config store: layered system/user/app precedence, live change events, atomic persist | ✅ working | `internal/config` layered + validation tests |
| Capability scoping: app Call/Handle adapters enforce manifest grants | ✅ working | SDK adapters replace caller grants; IPC methods reject unauthorized app calls; admin tokens remain trusted operator clients — see `docs/security.md` |
| Route-level capability enforcement (config, profile, session, document routes) | ✅ working | `config.read`/`profile.*`/`session.*`/`fs.write` enforced per-route; caller `app_id` pinned on config reads; stat oracle redacted |
| App identity & scoped grants: distinct per-app tokens, manifest capability grants, lifecycle revocation | ✅ working | `internal/security`, `internal/ipc`, `internal/app` unit + socket integration + `test/e2e` |
| App-private storage + scoped VFS grants | ✅ working | `internal/app` seed/scoped-grant tests; per-app private roots |
| Sessions: attach/detach shells, persistent workspace state, reaping on close | ✅ working | `internal/session` tests; bounded manager state |
| Personal workspace profiles + session ownership | ✅ working | `internal/profile` tests; `/users/<profile_id>/` isolation, per-profile recents/search |
| Documents: permission-aware search, recents, favorites, open-with handoff | ✅ working | `internal/document` tests; bounded handoff grants, special-file rejection |
| Packages: safe install, update, rollback, permission inspection | ✅ working | `internal/pkg` manager/archive tests; `gctl package` |
| Recovery: portable backup, export/import, verified restore | ✅ working | `internal/recovery` tests; export omission reporting, hard-fail unreadable targets, profile-scoped restores |
| Platform opt-ins: clipboard, shared-folder mounts, network egress | ✅ working | `internal/security.PolicyStore` operator policies; sanitized clipboard, `os.Root`-confined host mounts, host/port/HTTPS egress filters |
| Experimental window spike: draft window service + bloom adapter, standalone `windowdemo` (not registered with the shell) | ⚠️ nonproduction spike | `go test ./internal/experience/experimental/windows/...`; Gio comparison unverified ([#216](https://github.com/drawmeanelephant/gostalgia/issues/216)) |
| Cross-platform compile (darwin/linux/windows) | ✅ compiles | `GOOS=` builds in CI matrix |
| Cross-platform behavior: CI runs the full test suite on ubuntu, macOS, and Windows runners | ✅ CI-green (Windows: full fstest skipped — see `docs/filesystem.md` metadata caveat; structural checks run everywhere) | `.github/workflows/ci.yml` |

Quick check from a clean checkout:

```sh
go build ./... && go vet ./... && go test -race ./...
gofmt -l .  # empty output = clean
go run ./cmd/gostalgia shell --root /tmp/gs # interactive; exit shuts down
# Or headless:
go run ./cmd/gostalgia boot --root /tmp/gs   # terminal 1
go run ./cmd/gctl --root /tmp/gs status      # terminal 2
```

## Known gaps (honest list)

- **`inproc` apps share the runtime's memory.** In-process isolation is a
  logical capability boundary only — a rogue in-process component could bypass
  it via unsafe memory access. Untrusted applications must be packaged as
  external executables and run under `sandbox` or `strict` isolation
  (see `docs/security.md` §Isolation Levels).
- **Windows host confinement:** Windows lacks unprivileged native sandboxing
  equivalents to Linux user namespaces or macOS Seatbelt, so `sandbox`/`strict`
  requests **fail closed** by design; external apps on Windows run under
  `trusted` isolation only.
- **macOS Seatbelt is deprecated by Apple** (`sandbox-exec`/Sandbox.kext);
  it remains functional on standard macOS installs but future macOS versions
  may alter or remove it.
- **Network egress is kernel-enforced only for sandboxed external apps.**
  `inproc` and `trusted` apps use host networking; for them `net.egress` is a
  policy/capability gate, not a packet boundary.
- **Same-UID local attacker:** an attacker with code execution under the same
  OS user account can read `<root>/runtime.json`, attach debuggers, or inspect
  process memory. The operator token authenticates local clients; it is not a
  boundary against the owning OS account.
- **IPC observability is operator-only and metadata-only:** one subscription
  per connection; history retains 256 events in memory, not a durable security
  audit. Queue drops and runtime epochs explicitly signal resynchronization.
  Scoped app credentials do not grant access to the operator trail.
- **Linux `strict` descendant sweeping:** a descendant that both leaves the
  process group and orphans itself within one `/proc` poll interval can evade
  tracking until teardown, where `KillProcessTree` SIGKILLs the whole group
  (see `docs/security.md`).
- **Windows filesystem metadata:** directory mtimes are advisory on Windows
  (OS-level API inconsistency; see `docs/filesystem.md`).
- **VirelaiOS note:** Early VirelaiOS bring-up (toolchain, guest runner, kernel
  integration) is separately owned by the repository owner and is not treated as
  implemented; the shipped runtime remains standard-library Go on host platforms.

---

## The list

The original twenty-five tracked issues (#20–#44) across five GitHub milestones
are **all closed and merged**. A hardening audit in October 2026 filed sixteen
follow-up issues (#77–#92), all fixed by PRs #93–#103. Two follow-on milestones
are now complete — all fourteen follow-on issues (#104–#117) shipped via PRs #118–#131.

### Milestone 1: 01: A welcoming terminal workspace — ✅ complete

- [x] **[#20 Docs: reconcile the roadmap and security claims with the current implementation](https://github.com/drawmeanelephant/gostalgia/issues/20)** — reconcile status.md, security.md, processes.md, and guides against merged SDK and process services.
- [x] **[#21 Experience: establish a coordinated Charm version and dependency baseline](https://github.com/drawmeanelephant/gostalgia/issues/21)** — evaluate coordinated Bubble Tea/Lip Gloss/Bubbles baseline without cgo; pinned v1.3.10/v1.1.0/v1.0.0 with automated dependency fences.
- [x] **[#22 Experience: build a reusable Gostalgia visual language and component kit](https://github.com/drawmeanelephant/gostalgia/issues/22)** — tokens, panel borders, headers, tabs, dialogs, badges (`internal/experience/ui`, `internal/experience/theme`).
- [x] **[#23 Experience: add a home screen, searchable launcher, and command palette](https://github.com/drawmeanelephant/gostalgia/issues/23)** — home screen, searchable app cards, keyboard command palette.
- [x] **[#24 Experience: add accessible terminal modes and layout regression coverage](https://github.com/drawmeanelephant/gostalgia/issues/24)** — monochrome, high-contrast, reduced-motion, Unicode/layout regression tests.

### Milestone 2: 02: Everyday apps and documents — ✅ complete

- [x] **[#25 Filesystem: add safe VFS document operations and recoverable saves](https://github.com/drawmeanelephant/gostalgia/issues/25)** — atomic saves, copy/move, trash/restore, bounded reads/writes.
- [x] **[#26 Apps: define a data/action presentation contract with one terminal owner](https://github.com/drawmeanelephant/gostalgia/issues/26)** — typed/versioned data and action contracts; shell alone owns rendering.
- [x] **[#27 Apps: build a VFS-backed Files browser](https://github.com/drawmeanelephant/gostalgia/issues/27)** — keyboard browsing, sorting, previews, file operations.
- [x] **[#28 Apps: build Notes with safe saving, dirty state, and crash recovery](https://github.com/drawmeanelephant/gostalgia/issues/28)** — editor, save/save-as, dirty indicator, crash recovery.
- [x] **[#29 Settings: add layered preferences, live updates, and an interactive settings screen](https://github.com/drawmeanelephant/gostalgia/issues/29)** — layered system/user/app precedence, live change events.

### Milestone 3: 03: A live, observable computer — ✅ complete

- [x] **[#30 Processes: capture bounded child output and expose complete diagnostics](https://github.com/drawmeanelephant/gostalgia/issues/30)** — bounded stdout/stderr ring buffers with drop accounting, wait delay protection, sanitized child environments, IPC diagnostics (`proc/logs`, `proc/info`), `gctl ps`/`logs`, and shell `ps`/`logs`.
- [x] **[#31 Processes: add supervision, bounded history, and crash-loop protection](https://github.com/drawmeanelephant/gostalgia/issues/31)** — restart policies, crash-loop backoff, bounded exit history, reaping.
- [x] **[#32 IPC: add bounded event subscriptions, audit history, and responsive concurrent calls](https://github.com/drawmeanelephant/gostalgia/issues/32)** — versioned operator subscriptions, drop-oldest buffers, multiplexed calls, bounded metadata-only trail, retention/drop/epoch resynchronization; real-runtime and race stress coverage.
- [x] **[#33 Experience: add a live Task Manager, notification center, and crash receipts](https://github.com/drawmeanelephant/gostalgia/issues/33)** — live process screen, toasts, notifications, crash receipts.
- [x] **[#34 Sessions: support detachable shells and persistent workspace state](https://github.com/drawmeanelephant/gostalgia/issues/34)** — attach/detach, persistent workspace state, headless survival.

### Milestone 4: 04: Application isolation and trust — ✅ complete

- [x] **[#35 Security: give external apps distinct authenticated identities and scoped grants](https://github.com/drawmeanelephant/gostalgia/issues/35)** — separate operator authority from app principals; lifecycle-bound tokens.
- [x] **[#36 Apps: launch external Go applications over the environment protocol](https://github.com/drawmeanelephant/gostalgia/issues/36)** — external Go app execution, protocol compatibility, process tracking.
- [x] **[#37 Security: add app-private storage and scoped VFS grants](https://github.com/drawmeanelephant/gostalgia/issues/37)** — per-app private storage roots, scoped file-selection grants.
- [x] **[#38 Security: enforce platform-specific app execution and resource policies](https://github.com/drawmeanelephant/gostalgia/issues/38)** — host enforcement on macOS (Seatbelt) and Linux (namespaces + `prlimit64`); honest fail-closed reporting elsewhere.
- [x] **[#39 Security: add adversarial isolation tests, IPC fuzzing, and a threat model](https://github.com/drawmeanelephant/gostalgia/issues/39)** — adversarial test suite, protocol fuzzing, threat model documentation.

### Milestone 5: 05: A personal, extensible computer — ✅ complete

- [x] **[#40 Packages: add safe app installation, updates, rollback, and permission inspection](https://github.com/drawmeanelephant/gostalgia/issues/40)** — package format, verification, staging, rollback.
- [x] **[#41 Profiles: add personal workspace profiles and session ownership](https://github.com/drawmeanelephant/gostalgia/issues/41)** — personal profile identities, isolated preferences/workspaces.
- [x] **[#42 Documents: add permission-aware search, recents, favorites, and open-with handoff](https://github.com/drawmeanelephant/gostalgia/issues/42)** — bounded search, recent files, app handoff contracts.
- [x] **[#43 Recovery: add portable backup, export/import, and verified restore](https://github.com/drawmeanelephant/gostalgia/issues/43)** — versioned backup archive, verified staging and restore.
- [x] **[#44 Platform: add explicit opt-in clipboard, shared-folder, and network integration](https://github.com/drawmeanelephant/gostalgia/issues/44)** — capability-gated host adapters (clipboard, mounts, egress).

### Post-milestone work (merged)

- **[#71 Compendium](https://github.com/drawmeanelephant/gostalgia/pull/71)** — hardened local-first knowledge base app (vault storage, search, markdown, extensive hardening/acceptance coverage).
- **Audit fixes #77–#92 → PRs #93–#103** — sandbox capability enforcement, document handoff bounds + special-file rejection, child-wire frame/dispatch/redirect limits, recovery export/restore correctness, config/profile/session/document route capability enforcement, session reaping, clipboard write ordering, `KillDescendants` portability and reparented-grandchild coverage.
- **[#206 bloom adoption](https://github.com/drawmeanelephant/gostalgia/issues/206): issues #207–#215 → PRs #218–#226** — bloom approved as an exact-pinned, experience-only dependency (Go 1.27.3 and Bubble Tea v2.1.1 floors); shell frame as a bloom tree with a real cursor; palette, confirmation and toast layers; transcript and log tails in bloom buffers; additive mouse input; steep tests of the real shell; finite per-session motion; FIGlet Home wordmark; experimental window spike (empirical Gio comparison deferred to #216).

### Milestone 6: 06: The utilitarian nine — complete

- [x] **[#104 Apps: add a notify/post route surfaced in shell toasts and the notification center](https://github.com/drawmeanelephant/gostalgia/issues/104)**
- [x] **[#105 Platform: add a host audio adapter and a capability-gated sound/play route](https://github.com/drawmeanelephant/gostalgia/issues/105)**
- [x] **[#106 Apps: build a Calculator](https://github.com/drawmeanelephant/gostalgia/issues/106)**
- [x] **[#107 Apps: build a Todo list with persistent tasks](https://github.com/drawmeanelephant/gostalgia/issues/107)**
- [x] **[#108 Apps: build a Pomodoro timer](https://github.com/drawmeanelephant/gostalgia/issues/108)**
- [x] **[#109 Apps: build an RSS/Atom feed reader](https://github.com/drawmeanelephant/gostalgia/issues/109)**
- [x] **[#110 Apps: build a Markdown document viewer](https://github.com/drawmeanelephant/gostalgia/issues/110)**
- [x] **[#111 Apps: build a Weather app](https://github.com/drawmeanelephant/gostalgia/issues/111)**
- [x] **[#112 Apps: build a Sysmon system monitor](https://github.com/drawmeanelephant/gostalgia/issues/112)**
- [x] **[#113 Apps: build a text adventure](https://github.com/drawmeanelephant/gostalgia/issues/113)**
- [x] **[#114 Apps: build a music toy / step sequencer](https://github.com/drawmeanelephant/gostalgia/issues/114)**

### Milestone 7: 07: The cunty GUI two — complete

- [x] **[#115 Apps: extend the presentation contract with text-block, meter, and grid elements](https://github.com/drawmeanelephant/gostalgia/issues/115)**
- [x] **[#116 Apps: build Petwatch, a Tamagotchi task manager](https://github.com/drawmeanelephant/gostalgia/issues/116)**
- [x] **[#117 Apps: build Dogcalc, a calculator with dogs for buttons](https://github.com/drawmeanelephant/gostalgia/issues/117)**

---

## Verification log

| Date | Check | Result |
|---|---|---|
| 2026-11-19 | Tracker sync (#217): Charm baseline row matched to `go.mod` on `main` at `d40b54d` (Go 1.27.3, Charm v2, bloom alpha.6, steep/reel alpha.12); fence names matched to `test/e2e/dependencies_test.go`; snapshot rows updated for the merged bloom adoption (#206) | docs-only; the seven dependency fences pass, and `go test -race` passes for `internal/experience/shell/...` and `internal/experience/experimental/windows/...` on macOS |
| 2026-10-08 | M6+M7 complete: all fourteen follow-on issues (#104–#117) shipped via PRs #118–#131 — notify/post, host audio adapter (`sound` capability + `sound/play`), Calculator, Todo, Pomodoro, RSS, Markview (handoff-scoped grants), Weather (`net.egress` + Open-Meteo), Sysmon (`proc.list`/`proc.stop`), Adventure, Petwatch, Dogcalc, Musictoy, contract v2; known flake filed as #132 | `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test -race ./...`, `CGO_ENABLED=0 go build ./...` green on every merged PR across macOS/Linux/Windows CI |
| 2026-10-08 | Milestone sync: all M1–M5 issues (#20–#44) merged and closed; Compendium (#71) and audit-fix batch (issues #77–#92 → PRs #93–#103) landed; `status.md` reconciled against shipped reality; snapshot table expanded, stale gaps (OS sandbox, supervision, single-user, single-layer config) resolved into current honest-gap list | `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test -race ./...` all green on macOS (32 packages incl. `test/e2e`) |
| 2026-10-05 | Merge latest main into IPC #32: preserve Charm baseline/dependency fences and #35 scoped principals/revocation with concurrent replies and events; add scoped event-access, pending-response revocation and event-disconnect cleanup regressions | Full build/vet/gofmt/race/pure-Go gate passes on macOS; three repeated IPC/security/e2e race runs pass |
| 2026-10-06 | Security: distinct authenticated app identities and scoped grants (#35): Principal/TokenStore, operator vs app tokens, per-request validation, revocation on app exit/stop, CleanEnv secret scrubbing, socket e2e tests | Full gate green (vet, gofmt, -race, e2e); denied undeclared methods, stale token replay rejected, confused-deputy protection verified |
| 2026-10-05 | IPC subscriptions/history/concurrent calls (#32): five repeated focused race runs; real-runtime event + private-document history checks; deterministic blocked socket writer, floods, malformed frames, authorization, cursor replay/epoch and cleanup regressions | `gofmt -l .` clean; `go build ./...`, `go vet ./...`, `go test -race ./...`, `CGO_ENABLED=0 go build ./...` pass on macOS; no new dependencies; platform adapters/CI unchanged |
| 2026-10-05 | Processes: bounded child output capture & complete diagnostics (#30): RingBuffer with drop accounting, WaitDelay child exit drain, sanitized DefaultChildEnv, proc/logs and proc/info IPC routes, gctl ps/logs and Charm shell ps/logs with ANSI sanitization | Full gate green (build, vet, gofmt, race tests on internal/process, internal/services, shell, e2e; CGO_ENABLED=0 pure-Go build OK) |
| 2026-10-05 | Charm shell + public capability-scoped SDK; one embedded-manifest Echo demo; spec read against implementation | Build/vet/gofmt/race green; pure-Go and Windows/Linux builds OK; real Bubble Tea socket integration + compiled CLI PTY smoke (echo, scoped identity, stop/relaunch, F2 shelf, exit/terminal restoration/runtime cleanup); no second demo built |
| 2026-10-05 | FIFO regression (#16): `HostFS.Stat`/`ReadDir` no longer `os.Root.Open` special files — Lstat fallback for FIFOs, sockets, devices, symlinks; regression tests for `Stat`, `ReadDir`, and `ipc.Server.Close` with a `fs/list` handler in flight over a real socket | repro tests fail unfixed, pass fixed; full gate green (`vet`, `gofmt`, `-race`) |
| 2026-10-05 | Docs drift (#17): `status.md` "Last verified" no longer hardcodes test counts (CI is the source of truth); `security.md`/`ipc.md` scope the `runtime.json` 0600 mode to unix — Windows inherits directory ACLs | docs match the code |
| 2026-10-05 | Published private repo `drawmeanelephant/gostalgia`; CI matrix (ubuntu/macos/windows: vet, test, race-on-unix, gofmt) | **all three green** |
| 2026-10-05 | Windows CI hardening: HostFS handle lifecycle at shutdown, backslash-name rejection, single-source metadata; fstest metadata gate scoped to unix with structural checks everywhere (OS-level limitation, `docs/filesystem.md`) | Windows runner fully passing |
| 2026-10-05 | Rebrand FakeDOS → Gostalgia: module `gostalgia`, binaries `gostalgia`/`gctl`, app id `com.gostalgia.echo`, root `~/.gostalgia`, `$GOSTALGIA_ROOT`, socket `gostalgia-*.sock` | 171 references renamed, zero old names remain; full gate re-run green; live e2e clean |
| 2026-10-04 | `go test -race ./...` | 65 tests, 10 packages, all pass |
| 2026-10-04 | `go vet ./...`, `gofmt -l .` | clean |
| 2026-10-04 | Cross-compile windows/amd64, linux/amd64 | OK |
| 2026-10-04 | Manual e2e: boot → gctl status/ps/apps/echo/ls/cat/fs-write → shutdown | clean shutdown, `runtime.json` removed |
| 2026-10-04 | Duplicate-boot guard on a live root | refused with pid/endpoint |
