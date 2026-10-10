# Platform layer

`platform/` contains only what the standard library does not already
abstract, kept deliberately thin.

## Current contents

| Concern | macOS/Linux | Windows |
|---|---|---|
| IPC listener | unix domain socket inside a private per-boot directory (`$TMPDIR/gostalgia-ipc-*/gostalgia-<hash>.sock`, mode `0700`; the hash derives from the root to respect the 104-byte socket path limit) | loopback TCP, ephemeral port |
| IPC dial | `unix://` scheme | `tcp://` scheme |
| Shutdown signal set | `os.Interrupt`, `SIGTERM` | `os.Interrupt` only (Ctrl-C / console close) |
| Host clipboard | Darwin: `pbcopy`/`pbpaste`; Linux: `wl-copy`/`xclip`/`xsel` | Pure Go Win32 API (`user32.dll` / `kernel32.dll`) |
| Network adapter | Standard HTTP/Dialer client | Standard HTTP/Dialer client |

Both are selected by build tags (`//go:build unix`, `//go:build windows`).
Signals are the exception to portability, not an accident of it: the set of
terminating signals a host delivers differs per platform, so the requested
set lives behind build tags here.

### Host clipboard integration and sanitization

`platform.HostClipboardAdapter` provides host system clipboard access:
- **macOS (`platform/clipboard_darwin.go`):** Executes `pbcopy` and `pbpaste` directly without intermediate shell scripts.
- **Linux (`platform/clipboard_linux.go`):** Probes Wayland (`wl-copy`/`wl-paste`) and X11 (`xclip`/`xsel`) helpers cleanly.
- **Windows (`platform/clipboard_windows.go`):** Directly invokes Win32 clipboard APIs via `syscall.NewLazyDLL` (`OpenClipboard`, `GetClipboardData`, `SetClipboardData`, `CloseClipboard`) with zero cgo and robust memory copying.
- **Sanitization (`platform.SanitizeClipboard`):** Strips terminal escape codes, ANSI control codes, OSC 52 sequences (which could hijack the host terminal), DCS sequences, unprintable control characters, and invalid UTF-8 bytes, and enforces a 1 MiB payload ceiling.
- **Internal fallback:** When host integration is disabled or unsupported, the clipboard service falls back to an in-memory clipboard store without interrupting user workflows.

### Network egress adapter

`platform.NetworkAdapter` provides outbound network transport (`platform/network.go`). In conjunction with `internal/services.NetService`, it enforces explicit `net.egress` capability checks and operator-configured destination whitelisting, port filtering, and HTTPS transport rules.

### Host sandbox enforcement

`platform.ConfigureSandbox` confines external child processes per `ExecutionPolicy`, and `platform.GetHostSecurityCapabilities` reports what the host can actually enforce:

- **Linux:** unprivileged user/mount/network namespaces plus `prlimit64` resource ceilings applied post-start; a re-exec init hook (`GOSTALGIA_SANDBOX_INIT`) programs the mount namespace.
- **macOS:** an Apple Seatbelt profile via `/usr/bin/sandbox-exec`. Limits that XNU honors (`MaxOpenFiles`, `MaxCPUSeconds`, `MaxProcesses`) are applied by a self-limiting trampoline — the runtime binary re-executed inside the sandbox via `GOSTALGIA_SANDBOX_INIT` — because macOS offers no `prlimit64`-style API for another process. `MaxMemoryBytes` is not enforceable (`setrlimit` rejects `RLIMIT_AS`/`RLIMIT_DATA`), so `ResourceLimits` reports `false` on darwin.
- **Windows:** unsupported; sandboxed isolation fails closed with `ErrSandboxUnsupported`.

## Rules

1. No `GOOS` conditionals outside `platform/` (and, rarely, build-tagged files
   next to the subsystem that owns the concern).
2. Platform adapters implement the same functions on every target; callers
   stay portable.
3. If an adapter file grows, the concern probably belongs in the subsystem it
   serves — move the portable part back out.

## Planned additions

- Windowing/input/audio adapters once the desktop milestone picks a toolkit.
- Named-pipe listener option for Windows.
- Default-path resolution per platform if conventions diverge.
