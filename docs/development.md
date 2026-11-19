# Development

## Build and check

```sh
go build ./...        # compile everything
go vet ./...          # static analysis
gofmt -l .            # formatting check (empty output = clean)
go test ./...         # unit + integration tests
go test -race ./...   # race detector (run before merging anything)
```

Or `make build|vet|fmt|test|race`. Requires Go ≥ 1.27.3, the floor set by the
pinned `bloomfx.dev/bloom` (`os.Root.MkdirAll` alone needs 1.25).

## Run the environment

```sh
go run ./cmd/gostalgia shell --root /tmp/gs       # interactive Charm shell
# Or headless:
go run ./cmd/gostalgia boot --root /tmp/gs        # terminal 1
go run ./cmd/gctl --root /tmp/gs status        # terminal 2
```

Default root is `~/.gostalgia` (or `$GOSTALGIA_ROOT`). `gctl` commands:
`status`, `ps`, `apps`, `echo MSG`, `ls PATH`, `cat PATH`, `shutdown`, and
`call METHOD [JSON]` for any IPC method.

A full walkthrough:

```sh
gostalgia boot --root /tmp/gs &
gctl --root /tmp/gs echo hello           # → hello (echo #1)
gctl --root /tmp/gs apps                 # echo listed as running
gctl --root /tmp/gs ls /users/guest      # seeded user directories
gctl --root /tmp/gs shutdown             # clean shutdown
```

Shut down with Ctrl-C in the boot terminal instead, if you prefer; both paths
run the same graceful shutdown.

## Terminal end-to-end checks

Replace manual PTY smoke repetition for shell behavior with the steep scenario
suite:

```sh
go test -count=1 -v ./internal/experience/shell -run '^TestSteepRuntimeScenarios$'
go test -count=1 -v ./internal/experience/shell -run '^TestSteepMotionScenarios$'
go test -count=1 -v ./test/e2e -run '^(TestCharmRestrictedToExperience|TestSteepImportFence|TestApprovedCharmBaseline)$'
```

The scenarios boot a disposable real runtime, authenticate an IPC client, and
drive the production shell through an emulated terminal. They cover Home,
launcher launch/stop, the palette, prompt echo, below-minimum resize and
recovery, bracketed paste, decoded mouse input, toast geometry, and Ctrl-C
terminal restoration. The motion scenarios additionally advance both injected
clocks through a real failed launch, capturing 0/20/160 ms frames, and drive the
palette/dialog opening reveal and a new toast entrance to a settled snapshot
without wall-clock sleeps. Checked-in snapshots are
compared read-only by default; see
[experience.md](experience.md#verification-and-snapshots) for deliberate updates.

Terminal emulation does not replace host integration checks. When changing
terminal ownership, detach, or SSH transport, also run the SSH integration tests
and exercise a real local PTY and the real OpenSSH client against `boot --ssh`
with a disposable root and keys:

```sh
go test -count=1 -v ./internal/experience/sshx -run '^(TestSSHSessionAttachesShell|TestConcurrentSSHMotionPolicies)$'
```

Record resize/recovery, paste, mouse, toast invariance, detach survival, and
restored terminal modes for those host checks. Do not substitute mocked callers
or transports for the runtime under test.

## Repository layout

```text
cmd/gostalgia        environment runtime binary
cmd/gctl          control CLI for a running environment
internal/          config, events, ipc, vfs, process, session, security,
                   service (framework), services (core services), app,
                   runtime, experience/shell (Charm, IPC only)
sdk/               public stdlib-only app authoring contract
apps/              builtin applications: echo/ plus registration and
                   manifest seeding (apps.go)
platform/          host-specific listeners/dialers (build tags)
test/e2e           boots a real environment and exercises it over a socket
docs/              architecture + subsystem documents
status.md          living status + the canonical milestone tracker
```

## Conventions

- Core, SDK, and demo apps are stdlib-only. The experience layer has the
  approved Bubble Tea / Lip Gloss / bloom exception plus their required
  transitives. steep and reel are exact-pin, test-only dependencies confined to
  test files under `internal/experience/` and `test/`. See
  docs/architecture.md §4.1.
- Write apps using [the SDK specification](applications.md); do not import
  internal runtime packages, host APIs, or Charm into apps.
- No `GOOS` conditionals outside `platform/`.
- Every subsystem ships with tests; new IPC methods need a capability decision
  (docs/security.md) and a test covering the deny path.
- Errors are wrapped with `%w` and prefixed by the subsystem (`ipc:`, `vfs:`,
  `process:`); messages must not leak host paths.
- Documents under `docs/` are kept in sync with the implementation; a PR that
  changes behavior updates the matching document.
- **Workflow and PR isolation:** Feature PRs must **not** edit `status.md`.
  Task and milestone tracking is managed natively on GitHub using issue links
  (`Fixes #XX` in PR descriptions). Parallel feature branches touch only code,
  tests, and domain documentation under `docs/`. `status.md` is updated on
  milestone completion or periodic project syncs, eliminating merge conflicts
  between concurrent agents.

## CI

`.github/workflows/ci.yml` runs vet, tests, and (on unix) `-race` on
ubuntu/macos/windows matrix runners, plus a `gofmt` check.
