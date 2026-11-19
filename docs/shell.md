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
| `motion [on\|off]` | Enable (`on`) or disable (`off`) ReducedMotion for this shell; see [Finite per-session motion](experience.md#finite-per-session-motion-issue-213) |
| `mouse [on\|off]` | Show, or turn on or off, mouse input for this shell; see [Mouse and clipboard](#mouse-and-clipboard-issue-211) |
| `exit`, `quit` | Leave shell and shut down owned runtime |
| `shutdown` | Request runtime shutdown over IPC |

Commands are case-insensitive, IDs and method names are case-sensitive. Paths
accept Unix or DOS separators: `cd C:\users\guest\documents`. Relative paths
and `..` work inside the VFS. Quote paths/messages containing spaces; use single
or double quotes. Backslashes remain literal in path arguments. Raw `call`
JSON is preserved, not passed through path/quote parsing.

## Keyboard

- **F2**: switch between prompt and app shelf. **↑/↓** selects an app;
  **Enter** launches it; **F3** stops it after a confirmation; **Esc** returns
  to the prompt. The
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
- **Confirmations**: `shutdown`, stopping an app (F3, the palette, or
  `stop APP-ID`), Task Manager `x`, and `r` or `reap` open a dialog first.
  Cancel is focused; **Tab/Shift-Tab** (or **←/→**) moves, **Enter** chooses,
  **Esc** cancels. See
  [Confirmations](experience.md#confirmations).
- **↑/↓** at prompt: history (100 commands, current draft restored).
- **Tab**: complete an unambiguous command or ID after launch/run/stop.
- **Left/Right, Home/End, Ctrl-A/E**: move cursor. Backspace/Delete edits;
  Ctrl-U clears input. Unicode and bracketed paste are supported.
- **PgUp/PgDown**: page the transcript by its height. The transcript keeps
  the newest 10,000 lines, and each result is capped at 12,000 runes. Long
  lines wrap; a resize keeps the line at the top of the transcript in place.
  At the end the transcript follows new output; paged back, it stays put
  while output arrives, and paging down to the end follows again. Running a
  command or switching views returns it to the end, and `cls` and `clear`
  empty it and return it there. Long input stays
  cursor-centered.
- **Task Manager log tail** (`l` on a process): **↑/↓** (or **k/j**) scroll a
  line, **PgUp/PgDown** a page, **Home/End** (or **g/G**) jump to the first or
  last line. **f** toggles following new output; a tail opens at its first
  line and does not follow until **f**. Scrolling up, or moving to a match
  above the lines on screen, stops following, and scrolling back to the end
  does not start it again; press **f**. **/** types a case-insensitive
  search, starting from the last one; **Enter** finds the first match at or
  below the top line on screen and **Esc** cancels. **n** and **N** move to
  the next and previous match, wrapping at the ends. The status line counts
  the matches. In color, every match is highlighted in
  the theme's selection colors; in every mode, Plain included, the current
  match's line is marked in the gutter. **Esc** or **Enter** returns to the
  table. These keys keep working while a command runs.
  - While a tail is open, and no command is running, the shell asks
    `proc/logs` for its last 100 lines every half second, one request at
    a time. A snapshot that continues the tail appends only the new lines,
    so a scrolled tail does not move. A snapshot that shares no lines with
    the tail replaces its lines and keeps the line number on screen: a log
    that was truncated or rotated, or one that grew by 100 lines or more
    between two refreshes. A tail keeps at most 100 lines. Snapshots carry
    no offsets, so a log whose new lines repeat the lines just before them
    can look unchanged, or grown by fewer lines than it has.
  - Each opened process keeps its own lines, scroll position, follow state
    and search while it is listed in the table, and the open one while it
    is open, so reopening it returns to the same place. A search being
    typed takes every key except Ctrl-C, Ctrl-D and the palette, and ends
    when you leave the tail for the table or a crash receipt; the palette,
    or another view opened from it, leaves it to finish when you come
    back. A response for a process you have since left never replaces the
    tail on screen.
- **Ctrl-Y**: copy to the host clipboard, under the operator's clipboard
  policy: the text selected with the mouse, or else the transcript or log
  tail lines on screen. See
  [Mouse and clipboard](#mouse-and-clipboard-issue-211).
- **Ctrl-C/D**: exit. A pending IPC command doesn't block the event loop;
  while it runs, command editing is disabled (exit/resize/scroll still work).

All commands use the same authenticated socket API as gctl, with a ten-second
command deadline. The shell is a **trusted operator**. Apps called by the shell
still execute with their own manifest permissions, never operator privileges.
Terminal controls from file/app output are filtered so OSC/ANSI output cannot
manipulate the terminal. Small layouts display a resize message; normal layouts
are clipped to the viewport and never require the core to know terminal size.

## Experimental windows, not shell wiring (Issue #215)

The standalone command below demonstrates the draft window service without
changing this shell's single-presentation model or creating another production
terminal owner:

```sh
go run ./internal/experience/experimental/windows/cmd/windowdemo
go build -tags giodemo ./internal/experience/experimental/windows/cmd/giowindowdemo
```

It opens the existing Screen and Pad presentation fixtures in two bloom layers.
Drag titles or use Alt-arrows to move them, click to focus and raise, press F6
to switch focus, Tab/Shift-Tab through the focused window's controls, Enter to
emit its semantic
SDK action, Ctrl-W or its border `[X]` to close it, and Ctrl-C to exit. Each
window retains its own draft. Typed open/focus/move/close events go through the
existing event bus; drawing publishes no events.

The second command is an equivalent Gio-adapter demo, gated behind the
`giodemo` build tag because `gioui.org/app` uses cgo on macOS and needs display
headers on Linux; it is compile-verified only. This is an experimental fixture
host, not production multiwindow support. Its actions remain local, it
implements only the presentation elements used by the fixtures, and its 30×10
clamped view clips content. See
[architecture §4.6](architecture.md#46-desktop-toolkit--experimental-bloom-and-gio-adapters-decision-deferred)
for the observed side-by-side toolkit leak inventory.

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
   backdrop effects at all, and neither does ANSI16, whose sixteen colors
   cannot show a dim. Under ANSI256 the shell converts only the composed
   content with `colorprofile.Writer{Profile: colorprofile.ANSI256}`, never
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
   ([#206 planning note](https://github.com/drawmeanelephant/gostalgia/issues/206#issuecomment-6990850564);
   see [Mouse and clipboard](#mouse-and-clipboard-issue-211)).

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
  holds a column of tabs, the status badge, the body (`Fill`) and the
  prompt; the help bar is the last terminal row, below the panel. The
  panel is a `ui.PanelNode`, so its border, title row and padding come from
  `Kit.PanelContentRect` (the same insets `PanelContentBounds` reports); the
  shell never subtracts them itself. Size-dependent kit output (`Tabs`,
  `HelpBar`, the Task Manager, Notification Center, palette, launcher and
  app views) enters the tree as `ui.Leaf` adapters that render into exactly
  the rectangle the layout gives them.
- **Responsive priorities.** When the frame is short, the blank spacer rows
  give way. Tabs, badge, prompt
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
  - The log tail shows a `LOGS · PID n · name` heading, the search or
    match status when there is one, and then as many log lines as fit. The
    keys scroll it from the same position as the bordered tail. Below seven
    body rows it is always compact.
  - The crash receipt shows the process and PID, then the reason, the exit
    code and state, the receipt ID and time, and the end of the log
    excerpt. It switches when the bordered receipt could not reach its
    reason line or would cut its heading before the PID.

  The keys do not change. Up, Down, Enter and `d` act on the entry on
  screen, `a`, `c` and `n` work as before, and Esc goes back. Growing the
  terminal restores the bordered panel with the same selection and log
  position.
- **Layers.** Toasts, the command palette and confirmations are bloom layers
  over the frame, never rows in it, so the solved frame is the same with or
  without them.
  - The toast stack is a layer over the body, anchored to its bottom-right
    corner: at most 48 cells wide, at most a third of the body tall (one row
    at 30×10), keeping the newest toasts and dropping the flood notice
    first. The body's rectangle, the transcript's height and scroll, and
    its visible lines are unchanged as toasts appear, stack and expire. The
    layer only draws `notifications.Manager`'s active toasts; it has no
    timer of its own.
  - The palette is a modal layer centered between the frame's top border and
    the prompt row (at most 66×14), and a confirmation is a
    `Kit.DialogLayer` centered one cell inside the screen. Only one modal is
    open at a time; a confirmation opened from the palette replaces it.
  - A modal layer's backdrop dims the screen beneath it by 45% (`fx.Dim`) in
    color themes at ANSI256 or TrueColor, converted back to the session's
    profile as rule 3 describes. Monochrome, `ui.Plain` and ANSI16 get no
    backdrop.
  - The shell routes keys to the open modal before anything else except
    `Ctrl-C`/`Ctrl-D`, so nothing beneath it, shortcuts included, sees a
    key or a paste.
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
  input. A busy prompt, any other mode, an open palette or confirmation, and the
  size notice show no cursor; closing the layer restores it.
- **Root lifecycle.** Each shell model owns one root, built with
  `bloom.WithColors` from its kit's text and surface colors, that model's
  `bloom.WithClock`, `bloom.WithCopyFlash(false)`, the kit's selection style
  and, while the mouse is on, the kit's tooltip style. The root is rebuilt lazily, on the
  next render after the effective theme, color mode, reduced-motion or mouse
  setting changes, so a config poll that re-applies the same settings keeps
  it.
  `Root.Init` is never called, and the terminal's `ForegroundColorMsg`,
  `BackgroundColorMsg` and `ColorProfileMsg` replies never reach the root.
  Separate models, such as concurrent SSH attaches in one process, never
  share a root.
- **Finite failed-launch feedback (Issue #213).** The only attached transition
  is a 160 ms badge `fx.Shake` after a failed app launch. Its `anim.Value` and
  the root receive the same per-model clock through `anim.Clock` and
  `bloom.WithClock`; tests advance both without wall-clock sleeps. No
  `anim.SetMotion`, palette/dialog/toast/layout transition, or idle animation
  is attached. ReducedMotion removes the value and transition, returns no
  animation-frame command even when enabled during an active shake, keeps
  `WithCopyFlash(false)`, and preserves the pre-#213 static goldens. Session
  `motion` overrides survive later polls and theme changes. Without one,
  explicit non-default config wins; otherwise the theme default wins, so the
  built-in `false` default cannot enable motion in a default-reduced theme.
  Existing process/config/log polling and notification expiry remain functional work.

### Home wordmark and ASCII chrome (Issue #214)

Home draws `GOSTALGIA` through `bloomfx.dev/bloom/fig` with the bundled `big`
font and two tracking cells between glyphs. When that figure does not fit the
solved body content width, Home uses bundled `mini`; when `mini` does not fit,
it uses the existing `G O S T A L G I A` spaced title. A short body also chooses
the spaced title when a FIGlet figure would leave less than one gap and one
actionable running-application row. These are cell-based thresholds, not raw
terminal-width checks, so panel borders and theme padding are already excluded.
The running-application section can shrink to its selected row at 30×10 without
covering the prompt or help.

Both FIGlet fonts emit ASCII, including under Monochrome and effective Plain.
The kit's heading tokens supply the accent/surface styling in color modes;
effective Plain emits no color escapes. The wordmark is static: it attaches no
effect, clock, animation frame command, or idle redraw work, regardless of the
model's motion setting.

The pinned bloom `fig/fonts/LICENSES.md` lists both `big` and `mini` under
**BSD-3-Clause**, from figlet. Their copyright notices are:

```text
Copyright (C) 1991, 1993, 1994 Glenn Chappell and Ian Chai
Copyright (C) 1996, 1997, 1998, 1999, 2000, 2001 John Cowan
Copyright (C) 2002 Christiaan Keet
Copyright (C) 2011 Claudio Matsuoka
All rights reserved.
```

Monochrome also applies bounded ASCII fallbacks to shell-owned help and hint
arrows, middle-dot separators, header rules, truncation suffixes, and empty
markers. The conversion happens at those owned literals and suffixes, including
`ui.HelpBar`, `help`/`session`/`apps`/`logs` command summaries, presentation
hints, and task-log headings; it is not a global Unicode filter. External and
application text, including CJK, combining
characters, and emoji, remains unchanged after `ui.Sanitize`. Non-Monochrome
Plain keeps its existing chrome Unicode; its wordmark is ASCII because the
bundled fonts and spaced title already are.

Home goldens cover all five built-in themes at 30×10, 80×24, and 200×60 in
`internal/experience/shell/testdata/home-wordmark/`, with focused tests around
the actual big/mini/spaced thresholds and short-height fallback.

### Mouse and clipboard (Issue #211)

Mouse input is additive: every click, wheel step and copy has a keyboard
equivalent, and the shell stays complete without a mouse.

- **On by default.** A new shell asks the terminal for mouse reports through
  the `tea.View` it returns (Bubble Tea stays the only terminal owner). The
  default is on because nothing is lost by it: every action keeps its key,
  and clicks, the wheel, tooltips and select-to-copy make the shell easier
  to discover. Many terminals still select natively with a modifier held
  while the shell has the mouse (Shift in xterm-like terminals, Option in
  iTerm2).
- **Turning it off.** `mouse off` stops mouse reporting at once, so the
  terminal's own selection and copy work; `mouse on` turns it back on and
  `mouse` shows the setting. Turning it off drops any press, double click,
  hover, tooltip or selection in progress: a click before `mouse off` and
  one after `mouse on` are never a double click. A report the terminal sent
  before it saw the change does nothing.
- **Configuration.** `accessibility.mouse` (boolean) sets the default:
  missing means on, `false` means off. Set it with
  `call config/set {"path":"accessibility.mouse","value":false}` or any
  `config/set` client. It has no built-in default in
  `internal/config/defaults.go`, so config validation does not check it; a
  value that is not a boolean is ignored, which leaves the mouse on. The
  shell reads it at start and with each config poll, and applies it only
  when the effective value changes: `mouse on|off` lasts for this shell,
  is not saved, and holds until the configured value next changes. Each
  shell, including each SSH attach, has its own setting, root and gestures.
- **Mouse modes.** While on, the shell asks for cell-motion reports
  (clicks, the wheel and drags); while a cut line with a tooltip is on
  screen, bloom asks for all motion, for hover. Off asks for none. On exit
  or detach the terminal is restored as before, mouse tracking included
  (`ESC[?1000l`, `ESC[?1002l`, `ESC[?1003l`, `ESC[?1006l`).

Each click runs the same function as its key, which checks the state when
the click arrives, not when the frame was drawn: a busy shell, an action in
flight, a disabled action or cell, a loading or unnegotiated app view, and an
open palette or confirmation stop a click as they stop the key. Viewport wheel
callbacks and selection completion recheck mouse/modal ownership when they run,
so an event delivered through a frame drawn before a modal opened cannot scroll
the covered surface or start a copy; log-tail wheels still work while a command
runs. A copy that already started keeps its completion feedback. A click only
selects or opens; stopping, reaping and shutdown stay behind their keys and
confirmations. Borders, headings, blank cells and the active tab do nothing.

| Mouse | Keyboard equivalent |
|---|---|
| Click a tab | F1 Home, F2 Apps, Esc Prompt, F5 Tasks, F6 Notifications (or their `shortcuts.*` keys). The click always shows the tab's screen, where a key pressed on its own screen goes back to the prompt or the screen beneath. |
| Click a launcher card | ↑/↓ to select it |
| Double-click a launcher card | Select it and Enter: launches it once. The cards stay in place while the clicked one is in view, so the second click lands on the same card; they stay there as ↑/↓ move the selection within them, and scroll once it leaves them. |
| Click a palette item | ↑/↓ to it, Enter |
| Click a running app or document on Home | ↑/↓ to it, Enter |
| Click a Task Manager row | ↑/↓ to it, Enter: its crash receipt if one is recorded, else its log tail |
| Click a notification | ↑/↓ to it, Enter: marks it seen, and opens its crash receipt if it has one |
| Click an app view's action or grid cell | Tab and the arrows to it, Enter |
| Wheel over the transcript | PgUp/PgDown. Scrolled up, it stays put as output arrives; back at the end it follows again. |
| Wheel over a log tail | ↑/↓ and PgUp/PgDown. Wheeling never starts following; press **f**. |
| Wheel over the launcher, palette, Home, Task Manager table or Notification Center | ↑/↓, one row a step, stopping at the ends; the wheel never opens anything |
| Hover a cut app description or `GRANT` line in the launcher | A wider terminal shows it uncut; `apps` lists every app's grants |
| Drag over transcript or log tail text | Ctrl-Y copies the lines on screen |

**Selection and copy.** Dragging over the transcript or a log tail selects
text with bloom's public selection (`bloom.SelectionMsg`). When the drag
ends, the shell copies the selection; Ctrl-Y copies it again, or with no
selection, the transcript or log tail lines on screen, and says "Nothing to
copy" elsewhere. A `shortcuts.*` key set to Ctrl-Y wins over copying. A copy
is an asynchronous command calling the runtime's authenticated
`clipboard/write` with `{"text": …, "target": "host"}`; its answer comes
back as a toast badged COPY, and a slow answer never holds up keys, resize
or exit. "Copied" and "Nothing to copy" are toasts only, except on the
Notification Center, which draws no toasts and lists them instead; a blocked
or failed copy is always kept in the Notification Center, where it is the
only trace under Do-Not-Disturb.

- **Policy.** The runtime's clipboard service decides. The host clipboard is
  off by default: a copy then fails with `policy_denied` and changes no
  clipboard, and the "Copy blocked" toast says an operator can turn on
  `clipboard.enabled` and `clipboard.allow_host_write` with
  `sys/policy/update` (only `clipboard.allow_host_write` when the integration
  is on but writes are off). The shell cannot grant either. "Copied"
  reports a write the service accepted, and "Copy failed" a host without a
  clipboard or any other error.
- **Small terminals.** The blocked toast starts "Type mouse for how to allow
  it", and `mouse` prints the policy settings in the transcript, which wraps.
  The newest toast is never cut to fit the third of the body older toasts
  get: it may cover all but one body row until it expires, and where even
  that is too few rows (30×10) it keeps its first rows, ending in "…".
- **Host, not auto.** The shell asks for `target: "host"` because `auto`
  falls back to the environment's internal clipboard when policy denies the
  host, and reports success: a denied copy would look like a host copy. A
  host write also becomes the internal clipboard's content. The shell has
  no internal-only copy; `call clipboard/write {"text":"…","target":"internal"}`
  writes the internal clipboard explicitly.
- **SSH.** The host clipboard is the clipboard of the machine running the
  runtime. An SSH-attached shell copies there, under that machine's
  operator policy, never to the SSH client's clipboard. To copy into the
  client's clipboard, use `mouse off` and the client terminal's own
  selection.
- **Approved deviation: no OSC 52.** At alpha.6, bloom's
  `WithClipboard`/`CopySelection` send the text to the terminal's
  clipboard with OSC 52 whatever the clipboard function answers
  (`select.go`: the write's error is recorded, then `tea.SetClipboard` runs
  regardless), which over SSH is the client's clipboard and outside
  operator policy. The shell never installs `WithClipboard`, never calls
  `CopySelection` or `tea.SetClipboard`, and writes no OSC 52 on any
  outcome; `TestNoTerminalClipboardWrites` keeps those names out of
  `internal/experience`.
