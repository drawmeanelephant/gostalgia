# Charm terminal experience

```sh
go run ./cmd/gostalgia                         # default: own boot + shell
go run ./cmd/gostalgia shell --root /tmp/gs     # explicit root
```

Use a real interactive terminal (30×10 minimum, 80×24 recommended). Bubble Tea
owns its alternate screen and event loop; the shared [visual kit](experience.md)
draws thick slate borders, cream surfaces, amber filled headers and DOS prompt,
and focused app cards. `C:` is the
**environment VFS**, not your host drive. Runtime logs stay in
`<root>/logs/gostalgia.log`, not on top of the TUI. `--verbose` enables debug
file logging. The shell owns the runtime: exit, Ctrl-C, or Ctrl-D restores the
terminal and shuts down apps/services. External IPC shutdown also exits it.
A booted root cannot be opened a second time; use gctl to control a headless
runtime (`gostalgia boot --root /tmp/gs`). Shell attachment is not implemented.

## Prompt commands

| Command | Meaning |
|---|---|
| `help`, `?` | Command reference |
| `apps` | IDs, versions, live/ready states, manifest grants |
| `launch ID`, `run ID` | Launch a registered app; single-instance errors are visible |
| `stop ID` | Wait for app cleanup and retract routes |
| `echo MESSAGE` | Invoke the sole demo app (must be running) |
| `call METHOD [JSON]` | Raw service/app IPC, e.g. `call app/com.gostalgia.echo/identity` |
| `dir [PATH]`, `ls [PATH]` | List the current/given VFS directory |
| `cd PATH` | Validate and change the current directory |
| `type PATH`, `cat PATH` | Read a VFS file |
| `profile [list|switch|create|delete]` | Manage workspace profiles and switch active user |
| `backup [export|inspect|preview|restore]` | Export portable backup, preview conflicts, or restore workspace |
| `ps`, `status` | Process table with capability grants / runtime status |
| `cls`, `clear` | Clear transcript |
| `exit`, `quit` | Leave shell and shut down owned runtime |
| `shutdown` | Request runtime shutdown over IPC |

Commands are case-insensitive, IDs and method names are case-sensitive. Paths
accept Unix or DOS separators: `cd C:\users\guest\documents`. Relative paths
and `..` work inside the VFS. Quote paths/messages containing spaces; use single
or double quotes. Backslashes remain literal in path arguments. Raw `call`
JSON is preserved, not passed through path/quote parsing.

## Keyboard

- **F2**: switch between prompt and app shelf. **↑/↓** selects an app;
  **Enter** launches it; **F3** stops it; **Esc** returns to the prompt. The
  shelf displays description and manifest permissions.
- **F4**: open the selected running app's data/action view. Tab/Shift-Tab moves
  focus, Enter invokes an enabled action, and ↑/↓ selects an item. Esc cancels
  busy work or returns to the prompt. F2 returns to the shelf. See
  [the presentation contract](applications.md#7-dataaction-presentation-contract-version-1).
- **↑/↓** at prompt: history (100 commands, current draft restored).
- **Tab**: complete an unambiguous command or ID after launch/run/stop.
- **Left/Right, Home/End, Ctrl-A/E**: move cursor. Backspace/Delete edits;
  Ctrl-U clears input. Unicode and bracketed paste are supported.
- **PgUp/PgDown**: scroll transcript. Output is bounded to 400 lines;
  each result is capped at 12,000 runes. Long input stays cursor-centered.
- **Ctrl-C/D**: exit. A pending IPC command doesn't block the event loop;
  while it runs, command editing is disabled (exit/resize/scroll still work).

All commands use the same authenticated socket API as gctl, with a ten-second
command deadline. The shell is a **trusted operator**. Apps called by the shell
still execute with their own manifest permissions, never operator privileges.
Terminal controls from file/app output are filtered so OSC/ANSI output cannot
manipulate the terminal. Small layouts display a resize message; normal layouts
are clipped to the viewport and never require the core to know terminal size.

## Verification and boundaries

Charm is confined to `internal/experience/` (shell, theme, and component kit).
The runtime, SDK, and Echo demo remain standard-library-only. Module
transitives are documented in [architecture.md §4.1](architecture.md#41-dependency-policy).

Shell tests exercise key editing, Unicode, history, completion, quoting, DOS
paths, bad usage, transcript bounds, viewport dimensions, and control-character
filtering. An actual Bubble Tea program is driven through its key-message
interface against a booted runtime and authenticated socket: app listing,
Echo, scoped identity, stop/relaunch reset, VFS navigation, shelf control, and
terminal rendering. The app SDK spec is [applications.md](applications.md).

## Coordinated Charm baseline and dependency rules

### Approved versions (Issue #21)

Gostalgia adopts a coordinated **Charm v2** baseline pinned in `go.mod`:

- **Bubble Tea**: `charm.land/bubbletea/v2` — Elm-architecture terminal application lifecycle, alternate screen (declared on `tea.View`), and event loop.
- **Lip Gloss**: `charm.land/lipgloss/v2` — Style definitions, borders, colors, and layout blocks.
- **Bubbles**: `charm.land/bubbles/v2` — Reusable TUI components (text input, viewports, spinners, tables, key bindings) for upcoming visual language and launcher milestones (#22, #23).

### Evaluation and decision rationale

1. **v1 vs. v2 evaluation**:
   - Issue #21 originally pinned the production-proven v1 trio (`bubbles v1.0.0`, `bubbletea v1.3.10`, `lipgloss v1.1.0`) while Bubble Tea v2 was still in flux.
   - The migration to v2 (`charm.land/*` vanity paths, `View() tea.View`, `tea.KeyPressMsg` key events, bracketed paste as `tea.PasteMsg`, Kitty-protocol key disambiguation) has since landed; Go 1.26+ is required.
   - The pinned v2 versions keep a single coordinated baseline, eliminate version divergence across components, and compile cleanly without cgo (`CGO_ENABLED=0`) across macOS, Linux, and Windows.

2. **Evaluated extensions (Huh, Glamour, Harmonica)**:
   - **Huh** (form builder) and **Glamour** (markdown renderer): Evaluated and excluded from the direct baseline to minimize dependency surface. Neither is required for the DOS prompt shell or app shelf. They may be revisited in Milestone 2 if rich markdown manuals or complex interactive forms require them.
   - **Harmonica** (spring physics animation): Transitive dependency of Bubbles (`v0.2.0`), available if subtle physics-based motion is needed without adding direct dependencies.

3. **Dependency fences and boundaries**:
   - Charm libraries are strictly restricted to `internal/experience/`.
   - Core runtime (`internal/runtime`), public SDK (`sdk`), builtin applications (`apps/...`), and operator CLI (`cmd/gctl`) remain **100% standard-library-only**.
   - Dependency fences in `test/e2e/dependencies_test.go` enforce that:
     - `go list -deps gostalgia/internal/runtime gostalgia/sdk gostalgia/apps/... gostalgia/cmd/gctl` contains zero non-stdlib and zero Charm dependencies.
     - Only packages under `internal/experience/` import Charm libraries.
     - Direct module dependencies in `go.mod` match the approved baseline.
   - Issue #22 promotes two already pinned transitive helpers to direct
     experience imports: `x/ansi v0.11.6` for grapheme cell layout and
     `termenv v0.16.0` for explicit color profiles, not background probes.
     See [experience.md](experience.md#dependencies-and-boundary).

4. **No standalone host binaries or host shells**:
   - The shipped runtime does not require or execute standalone host tools or Charm binaries (`gum`, `glow`, `vhs`). All terminal rendering runs in-process via pure-Go libraries.
   - No host shell (`sh`, `bash`, `cmd.exe`) is invoked by the runtime or shell.

5. **Terminal API assumptions**:
   - Relies on standard ANSI escape sequences, alternate screen mode (declared via `tea.View.AltScreen`), raw terminal mode, and bracketed paste.
   - Output from files and untrusted processes is filtered to strip OSC and dangerous control sequences before rendering.
   - Viewport resizing is handled dynamically through `tea.WindowSizeMsg`, with graceful fallback down to 30×10.

6. **Licensing and attribution**:
   - Bubble Tea, Lip Gloss, and Bubbles are licensed under the MIT License, compatible with Gostalgia's license and dependency policy.

7. **Separation from VirelaiOS**:
   - Early VirelaiOS bring-up (toolchain, guest runner, kernel integration) is separately owned by the repository owner and tracked outside this roadmap. The Charm shell does not assume POSIX or claim guest-OS support; it runs on standard Go host targets (macOS, Linux, Windows).