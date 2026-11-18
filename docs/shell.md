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
- **Keyboard enhancements**: the shell asks the terminal for Kitty-protocol
  event types on top of the disambiguation Bubble Tea v2 requests by default.
  Terminals negotiate support per session; a terminal that doesn't answer keeps
  exactly this keyset.
  - **Shift-Enter**: in a version-3 multiline field (e.g. Notes' editor),
    newline — the same as plain Enter, since legacy terminals report both as
    Enter.
  - **Ctrl-Enter**: submits — a multiline form's action without leaving the
    field, or the command line at the prompt. Only disambiguating terminals
    can name it.
  - **Super-Space**: toggles the command palette, a chord that cannot collide
    with typing because it carries no text.
  - **Held keys**: where event types negotiated, a held activation key
    (Enter on an app action, palette item, launcher card, home entry, task
    row) commits once instead of repeating work per auto-repeat; key releases
    are consumed and never type, submit, or activate. Where they didn't
    negotiate, repeats behave as they always have.
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

- **Bubble Tea**: `charm.land/bubbletea/v2 v2.1.1` — Elm-architecture terminal application lifecycle, alternate screen (declared on `tea.View`), and event loop.
- **Lip Gloss**: `charm.land/lipgloss/v2 v2.0.6` — Style definitions, borders, colors, and layout blocks.
- **Bubbles**: `charm.land/bubbles/v2 v2.2.1` — Reusable TUI components (text input, viewports, spinners, tables, key bindings) for upcoming visual language and launcher milestones (#22, #23).
- **bloom**: `bloomfx.dev/bloom v0.0.0-alpha.6` — layout, compositing, and additive input for the experience layer only, approved in #207. See [bloom layout and compositing](#bloom-layout-and-compositing-issue-207).

The direct helpers `github.com/charmbracelet/x/ansi v0.11.9`,
`github.com/charmbracelet/colorprofile v0.4.3`, and
`github.com/muesli/termenv v0.16.0`, and the SSH attach transport
`charm.land/ssh v0.4.3` with `golang.org/x/crypto v0.54.0`, complete the
approved direct baseline that `TestApprovedCharmBaseline` locks.

### Evaluation and decision rationale

1. **v1 vs. v2 evaluation**:
   - Issue #21 originally pinned the production-proven v1 trio (`bubbles v1.0.0`, `bubbletea v1.3.10`, `lipgloss v1.1.0`) while Bubble Tea v2 was still in flux.
   - The migration to v2 (`charm.land/*` vanity paths, `View() tea.View`, `tea.KeyPressMsg` key events, bracketed paste as `tea.PasteMsg`, Kitty-protocol key disambiguation) has since landed. Go 1.27.3+ is required, the floor set by bloom's `go.mod` in #207.
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
     - Only packages under `internal/experience/` import bloom (any `bloomfx.dev/` package, production or test imports).
     - Direct module dependencies in `go.mod` match the approved baseline.
   - Issue #22 promotes two already pinned transitive helpers to direct
     experience imports: `x/ansi` (now v0.11.9) for grapheme cell layout and
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
   - Bubble Tea, Lip Gloss, Bubbles, and bloom are licensed under the MIT License, compatible with Gostalgia's license and dependency policy.

7. **Separation from VirelaiOS**:
   - Early VirelaiOS bring-up (toolchain, guest runner, kernel integration) is separately owned by the repository owner and tracked outside this roadmap. The Charm shell does not assume POSIX or claim guest-OS support; it runs on standard Go host targets (macOS, Linux, Windows).

### bloom layout and compositing (Issue #207)

`bloomfx.dev/bloom` is approved as a direct dependency for `internal/experience/`
only, pinned to exactly `v0.0.0-alpha.6`.

- **Role.** bloom solves layout, composes layers (modal dialogs, anchored
  toasts), draws viewports over append-only buffers, places the real cursor,
  and routes mouse input. The theme and `ui` kit stay the visual vocabulary:
  any `View() string` component, including a kit `Panel`, is a bloom leaf, and
  the kit's `Panel`/`PanelContentBounds` remain the source of border, title,
  and padding geometry. Bubble Tea stays the only terminal and event-loop
  owner, and apps still supply presentation data, never views.
- **Risk.** bloom is pre-1.0, has a single maintainer, and expects breaking
  changes. Pin one exact version; never use `@latest`, vendor, or fork it.
  Upgrade deliberately in a change of its own that reads the upstream
  changes, updates `go.mod` and `TestApprovedCharmBaseline`, and reruns the
  proofs below and the dependency fences.
- **Version floors.** bloom's `go.mod` requires Go 1.27.3, Bubble Tea v2.1.1,
  x/ansi v0.11.9, and a newer `charmbracelet/ultraviolet` pseudo-version.
  Minimal version selection raised the module to those, and with them the
  indirect go-runewidth v0.0.31, xo/terminfo v1.3.1, x/sync v0.24.0, and
  x/sys v0.49.0. Lip Gloss v2.0.6, colorprofile v0.4.3, and
  `clipperhouse/uax29/v2 v2.7.0` already matched.
- **Production wiring.** Since #208 the shell frame is a bloom layout tree;
  see [Shell frame layout](#shell-frame-layout-issue-208). The proofs in
  `internal/experience/ui/bloom_proof_test.go` still pin the pin's behavior.

Rules every later integration keeps, established by that proof against the
exact pin:

1. **Explicit colors, no probing.** Never call or return `Root.Init()`; it
   asks the host terminal for its colors. Build roots with
   `bloom.WithColors(...)` from the theme palette. Do not let host
   `BackgroundColorMsg`/`ForegroundColorMsg` replies override the theme. At
   alpha.6 a root records `tea.ColorProfileMsg` but does not downsample with
   it, and Bubble Tea sends its own configured profile (TrueColor for local
   shells) at startup, so profile safety never relies on that message.
2. **Kit panels are profile-safe.** A kit `Panel` drawn through
   `bloom.Render` or `Root.Render` keeps the kit's own SGR: `ui.Plain` emits
   zero escape sequences for every built-in theme (including
   `theme.Monochrome()`), and `ui.ANSI256` emits indexed colors with no
   truecolor (`38;2`/`48;2`, semicolon or colon form). No workaround is needed
   for plain composition.
3. **Blending needs a workaround.** Effects that mix colors, such as
   `fx.Dim` backdrops, produce 24-bit colors even after the root receives an
   ANSI256 `tea.ColorProfileMsg`. Plain and Monochrome attach no blending or
   backdrop effects at all. Other profiles convert only the composed content
   with `colorprofile.Writer{Profile: <the shell's chosen profile>}`, never
   the whole terminal stream (cursor, alternate-screen, keyboard-enhancement
   and restoration sequences pass through untouched).
4. **Sanitize first.** External text passes through `ui.Sanitize` before it
   reaches any bloom node; never mark it `Trusted()`.
5. **Per-session motion.** Never use the process-global `anim.SetMotion`;
   honor each session's ReducedMotion when attaching effects, and disable the
   copy flash (`bloom.WithCopyFlash(false)`) under it.
6. **No bloom clipboard at alpha.6.** `WithClipboard`/`CopySelection` always
   emit OSC 52, even when the callback denies the copy. Copy goes through the
   shell's `clipboard/write` IPC instead
   ([#206 planning note](https://github.com/drawmeanelephant/gostalgia/issues/206#issuecomment-6990850564)).

Run the proofs with:

```sh
go test ./internal/experience/ui -run 'Bloom|Truecolor' -count=1 -v
```

### Shell frame layout (Issue #208)

The shell renders every frame through bloom. `Model.layout()` returns the
frame as a bloom tree, and `View()` draws it with the model's own root,
`root.View(width, height, layout())`, keeping `AltScreen` and the kitty
event-type request on the returned `tea.View`.

- **Frame.** A kit `Panel` titled `G O S T A L G I A / PERSONAL COMPUTING`
  holds a column of tabs, the status badge, the body (`Fill`), toasts and
  the prompt; the help bar is the last terminal row, below the panel. The
  panel is a `ui.PanelNode`, so its border, title row and padding come from
  `Kit.PanelContentRect` (the same insets `PanelContentBounds` reports); the
  shell never subtracts them itself. Size-dependent kit output (`Tabs`,
  `HelpBar`, the Task Manager, Notification Center, palette, launcher and
  app views) enters the tree as `ui.Leaf` adapters that render into exactly
  the rectangle the layout gives them.
- **Responsive priorities.** When the frame is short, the optional rows give
  way in this order: the blank spacer rows, then toasts. Tabs, badge, prompt
  and at least three body rows always stay, which is exactly what the
  smallest frame, 30×10, leaves. On Home, a document list longer than the
  space first shrinks to a window that follows the selection. When even its
  heading and one document do not fit beside the other sections, *system
  activity* collapses first, then *document shortcuts*, so system activity
  never shows without the documents. *Running
  applications* never collapse and window around the selected app. Home
  keys act only on the items on screen: with documents collapsed, Up/Down
  stop at the last running app and Enter opens it. In the Task Manager
  table, `RESTARTS` collapses first, then `MEMORY`, `CPU` and `STATE`;
  `PID` and `NAME` never collapse, and the `PID` column widens so no ID is
  cut. Below six body rows the table drops its border and footer and shows
  a header plus the rows around the selected process.
- **Compact diagnostic views.** The Notification Center, the task log tail
  and the crash receipt are bordered panels when there is room. In a body
  too short for that panel to show its content (the 30×10 frame leaves
  three rows), each drops its border and shows its essential content:
  - The Notification Center shows a position and unread summary, then the
    entries around the selection, each as its title and message. It also
    switches when the panel is too narrow to show an entry's title beside
    its badge and timestamp. Below seven body rows it is always compact.
  - The log tail shows a `LOGS · PID n · name` heading and then as many log
    lines as fit. Up and Down scroll them from the same offset as the
    bordered tail. Below seven body rows it is always compact.
  - The crash receipt shows the process and PID, then the reason, the exit
    code and state, the receipt ID and time, and the end of the log
    excerpt. It switches when the bordered receipt could not reach its
    reason line.

  The keys do not change. Up, Down, Enter and `d` act on the entry on
  screen, `c` and `n` work as before, and Esc goes back. Growing the
  terminal restores the bordered panel with the same selection and log
  offset.
- **Below the minimum.** Under 30 columns or 10 rows the shell keeps its own
  notice, `GOSTALGIA / Resize to 30×10 or larger. / Ctrl-C exits.`, clipped
  to the terminal, instead of a squeezed frame. Exactly 30×10 draws the
  normal frame. A zero or negative dimension draws nothing. Resizing below
  the minimum and back changes no state: the mode, selection and prompt
  draft return as they were, and Ctrl-C still exits (or detaches an SSH
  attach) while the notice shows.
- **Real cursor.** In editable prompt mode the prompt row places the
  terminal's own cursor (`bloom.Text(...).Cursor(...)`) at the insertion
  point, measured in cells, so it lands correctly after combining marks,
  wide CJK characters and ZWJ emoji. Long input is sliced around the cursor
  by whole graphemes, keeping one cell free for the cursor at the end of the
  input. A busy prompt, any other mode, and the size notice show no cursor.
- **Root lifecycle.** Each shell model owns one root, built with
  `bloom.WithColors` from its kit's text and surface colors and
  `bloom.WithCopyFlash(false)`. The root is rebuilt lazily, on the next
  render after the effective theme, color mode or reduced-motion setting
  changes, so a config poll that re-applies the same settings keeps it.
  `Root.Init` is never called, and the terminal's `ForegroundColorMsg`,
  `BackgroundColorMsg` and `ColorProfileMsg` replies never reach the root.
  Separate models, such as concurrent SSH attaches in one process, never
  share a root.
