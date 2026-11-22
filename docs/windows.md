# Multi-window shell design (Issue #246)

**Status: implemented.** [#246](https://github.com/drawmeanelephant/gostalgia/issues/246)
asked for a design note before any implementation. The note merged in
[#256](https://github.com/drawmeanelephant/gostalgia/pull/256), and the shell
now implements it in the [four stages](#build-it-in-four-stages) below. The
[answered questions](#answered-questions) record the choices on the decisions
that most change what users see. Where the implementation settled a detail
differently from the proposal, this note describes what shipped.

## Start from the spike and the single-view shell

Two pieces existed before this work, and this design joins them.

The experimental window service, then in
`internal/experience/experimental/windows`, keeps window identity, titles,
bounds, focus, z-order, and lifecycle separate from any toolkit. `Service`
changes state only through `Open`, `Focus`, `Move`, and `Close`, and it
publishes `window.opened`, `window.focused`, `window.moved`, and
`window.closed` on the existing `internal/events` bus.
The bloom adapter draws each window as an opaque layer with a static shadow
and a title bar that places `[X]` first (#237). The Gio adapter implements the
same service, which is how #216 showed that the API does not depend on bloom.
No production package imported the spike, and
`TestNeutralAPIAndProductionDependencyBoundaries` enforced that.

The production shell showed one app presentation at a time. `Model.presentation`
held a single `appView`. `openView` replaced it, `dismissView` cleared it, and
`switchScreen` dismissed it before it changed screens. The body slot showed one
screen, and the tab bar added one tab with the app's title while a view was open.

The spike has no resize, no minimum window size, and no connection to apps. Its
windows hold two SDK fixtures, drawn by a renderer that implements only the
presentation elements those fixtures use. This design keeps the spike's model
and chrome, and it fills the windows through the shell's real presentation path.

## Decisions

### Only app presentations become windows

Every app presentation opens in a window. Home, Apps, Prompt, Tasks, and
Notifications stay body screens with their current keys, tabs, and compact
layouts. Settings is the `com.gostalgia.settings` app, so it opens in a window
like any other app.

App presentations are the only surface where seeing two at once helps, such as
Sysmon meters beside Notes. The built-in screens each have their own key
handling, like the Tasks table with its log search, and their own collapse rules
at small sizes. Putting them in windows would rewrite most of the shell's input
routing for little gain. A full desktop, in which every surface is a window,
stays with the M4 desktop work in
[architecture §4.6](architecture.md#46-desktop-toolkit--experimental-bloom-and-gio-adapters-decision-deferred).

### Windows live on their own Windows screen

Open windows share one body screen, the **Windows screen**. Its tab replaces
today's per-app tab, reads `Windows (n)`, and appears only while at least one
window is open. The other screens never draw windows over themselves. Switching
to Home or Tasks hides the windows but keeps them open, with their drafts,
selection, and scroll position. `switchScreen` stops calling `dismissView`.

Every path that opens a presentation today opens that app's window instead, or
focuses and raises the window if it is already open, and then shows the Windows
screen. Those paths are Enter on a running app in the launcher or on Home, F4,
the palette's **Switch to** items, the `settings` command, and F7. The last two
start Settings first if it is not running. F7 keeps its toggle: pressed while
the Settings window is focused, it leaves the Windows screen, as Esc does.

The alternative was to draw windows over every screen. A window could then float
over Home, but it would also hide part of whichever screen is active, and every
screen's mouse hit testing would have to account for window rectangles. A
dedicated screen gives each key press one owner and leaves the built-in screens
as they are.

### Layers keep the modal contract

From bottom to top, the shell draws these layers:

1. The frame: tabs, status badge, body, prompt, and help bar.
2. On the Windows screen only, the windows, in service z-order and confined to
   the body rectangle.
3. Toasts, over the body as today.
4. The palette or a confirmation dialog.

Windows are never modal, and an app cannot open a dialog of its own.
Confirmations stay shell dialogs. While the palette or a dialog is open, windows
receive no keys, clicks, or hover. `modalOpen` and `baseMouseAllowed` already
block the base surface while a modal is open, and windows are part of that
surface.

### Keyboard focus has two levels

The shell still has one focus owner at a time. On the Windows screen, that owner
is the focused window, and inside it the presentation's own focus ring.

Key routing keeps its current order and adds window keys after the global keys:

1. Ctrl-C and Ctrl-D, the confirmation dialog, the palette shortcut, and the
   open palette, unchanged.
2. The screen shortcuts F1 through F7, and Ctrl-Y, unchanged. They also work on
   the Windows screen.
3. On the Windows screen, the window keys in the table below.
4. The focused window's presentation keys, handled by today's `viewKey` with
   that window's state.

| Key | Action on the Windows screen |
| --- | --- |
| F8 | Focus and raise the next window in z-order. |
| Alt-arrow | Move the focused window by one cell. |
| Ctrl-W | Close the focused window. |
| Esc | Cancel the focused window's running action. With no running action, return to the previous screen. |

Each window keeps its own `appView`: field drafts, the selected item and cell,
the focus-ring position, region scroll, its epoch and request sequence, and any
in-flight action. The ring inside a window is today's ring of fields, then
actions, then the grid, then the region. Tab and Shift-Tab never leave the
window. `FocusApp` becomes `FocusWindow`, and the model reports the focused
window's ID, so the rules in
[Predictable focus management](experience.md#predictable-focus-management) keep
one derived source of truth.

The spike cycles windows with F6, but F6 opens the Notification Center in the
shell. F8 is the first free function key. Like F1 through F7, it becomes a named
`shortcuts` action, `window_next`, so users can bind it to another key.

Esc no longer closes a presentation. Before this design, Esc called
`dismissView`, which was safe while only one view existed. With several
windows open, a stray Esc would discard a window's drafts. Esc now leaves the
Windows screen and closes nothing. Ctrl-W or the `[X]` control closes a
window. Closing works the way `dismissView` did: it cancels the window's
in-flight action, and the app keeps running.

Some terminals do not report Alt-arrows, or report them as other keys. So that
every operation still works from the keyboard alone, the palette adds **Close
window** and **Move window** items. The existing **Switch to** items list every
running app, so they also focus any open window. **Move window** starts a
move mode in which the arrow keys move the focused window, and Enter or Esc
ends it. The help bar shows the
move-mode keys while the mode lasts.

### The mouse adds nothing the keyboard lacks

With the mouse on, a click anywhere in a window focuses and raises it, a drag on
the title moves it, and a click on `[X]` closes it. Clicks inside the content go
through today's `viewHits`, offset by the window's origin. The title and the
close control get hover cues through `hoverTarget`, and hover never moves
keyboard focus (#242). With `mouse off`, the shell reports no window mouse
targets, and the keyboard paths above cover every operation.

### Apps still supply data, not windows

The SDK does not change. Apps keep returning `sdk.View` through `app/<id>/view`,
`app/<id>/action`, and `app/<id>/cancel`, and they never learn that windows
exist. An app cannot open, place, size, title, raise, or close a window. The
shell titles each window with the view's `Title`, as the per-app tab does today.
Window events stay internal to the shell, as `events.go` already states.

A window is keyed by app ID. Apps are single-instance, so each app has at most
one window. A window is bound to the app instance it opened: `applyView` already
rejects a snapshot from another instance, so a restarted app never inherits the
previous instance's drafts.

Each window draws its contents with the shell's presentation renderer
(`viewLayout`, `viewPanes`, and `inspectLayout`), sized to the window's content
box. That keeps v3 regions, multiline fields, focal-point blocks, and every
theme and Plain-mode rule, none of which the spike's renderer has. The shell
does not port the spike's adapter renderer, `bloomadapter/presentation.go`,
which stays with the experimental bloom demo that still draws the two fixtures
with it. The spike's chrome moves into the shell with the service: the opaque
body, the static shadow, the close control placed before the title, and
truncation of wide and hostile titles.

Each open window refreshes the way the single view refreshes now: on events
from the shared per-connection `app.view` subscription, routed by app ID, and
on the 500 ms view tick. Each window has its own epoch and request sequence, so
a late reply for one window cannot change another. A shell holds at most 8
windows, which bounds the view fetches on each tick. Opening a ninth window
shows a "Too many windows" warning toast and opens nothing.

### Window geometry stays simple

The first window fills the body, so one window has the room that the single
presentation had, which an 80×24 focal-block split needs. Each later window is
the body less a margin of 4 columns and 2 rows, and it opens 2 columns right of
and 1 row below the window opened before it. The shell then clamps the window
inside the body. A window is never smaller than 32×12 cells.
That floor leaves a content area of at least 10 rows, which is the renderer's
`primaryMinRows`, and room for the `[X]` control and a short title.

When the terminal resizes, the shell moves and shrinks windows as needed to keep
each one inside the body, without going below the floor.

The first version has no window resize, by mouse or by keyboard. Resize,
maximize, and restore are later work, and they need a `Resize` operation in the
service.

### Small terminals fall back to one window

When the body is smaller than 32×12, the Windows screen draws only the focused
window. It fills the body without window chrome, the way today's single
presentation does. F8, Ctrl-W, Esc, and the palette items still work, and the
tab still shows the window count. In short terminals like 30×10 (issue #298),
the shell also collapses the non-editable prompt and status badge rows when
the palette is closed, giving the window five body rows, omits the redundant
inline hint row inside the window, uses compact whole hints in the help bar
(`Esc BACK`, `Tab NEXT`, `Enter GO` or `↑↓ ITEM`), prioritizes the focused
control and active multiline editing tail, and shows a footer cue (`Tab i/n`,
off-screen primary action, and `↑`/`↓` hidden-row counts) whenever content or
controls continue beyond the viewport (at both 30×10 and larger sizes like
80×24). Other apps stay open behind the focused one, and below 30×10 the
existing `sizeNote` fallback applies.

### Motion stays finite

Moving and raising a window are direct manipulation, so both are instant and use
no easing. Closing a window removes it at once, and closing it during its reveal
also ends the reveal's frames. Opening a window plays one 140 ms `fx.Iris`
reveal, the one that the palette and dialogs use. Only the newest opening
reveals: a second opening takes the reveal from a window that is still
revealing, and focusing a window that is already open plays nothing. The window
takes keys and clicks from its first frame, because hit testing follows the
layout and not the reveal's mask. Below the 32×12 floor no window frame shows,
so nothing reveals. Shadows are static. ReducedMotion attaches no reveal and
settles a running one. An idle Windows screen schedules no frames. These are
the rules in
[Finite per-session motion](experience.md#finite-per-session-motion-issues-213-239-240).

### Windows belong to one shell session

Each shell model owns its window service, so local, attached, and SSH shells
keep separate windows, focus, and geometry. The first version stores and syncs
nothing about windows, and workspace sync keeps writing only `cwd` and `cmd`.
The shell reads the workspace's `active_view` today but never applies it. This
design does not change that.

### The service leaves experimental, and Gio stays

The neutral service moves from `internal/experience/experimental/windows` to
`internal/experience/windows`, with its `Window` and `Bounds` types and its
events. The package keeps its rule against importing bloom or Charm. Two changes
come with the move:

- `Window` loses its `Presentation` field, and the `Closed` event loses its
  presentation payload. The shell keeps each window's `appView` next to the
  service, so the service tracks only identity, title, bounds, focus, and
  stacking.
- `TestNeutralAPIAndProductionDependencyBoundaries` lets
  `internal/experience/shell` import `internal/experience/windows`. It still
  forbids production imports from `experimental/`.

The Gio adapter and both demos stay under `experimental/` and import the moved
package. The `gioImportAllowed` fence in `test/e2e/dependencies_test.go` does
not change. #216 showed that the service API fits Gio, but it did not test
native Gio windowing, so the desktop toolkit choice stays with M4.

`Service` is not safe for concurrent use. A shell model changes state only in
Bubble Tea's `Update`, so one service per model needs no locking.

## Build it in four stages

Each stage is one commit that leaves the shell working and tested.

1. **Move the service.** Move the package, drop the `Presentation` field, and
   update both adapters, the demos, and the dependency tests. Users see no
   change.
2. **Keep several views, show one.** Replace `Model.presentation` with
   per-window `appView` state keyed by app ID. Add the Windows screen in its
   small-terminal form at every size, so the focused window fills the body. Add
   the `Windows (n)` tab, F8, Ctrl-W, the new Esc behavior, the palette items,
   and the 8-window limit. From this stage on, several apps can be open at once.
3. **Float the windows.** When the body fits the 32×12 floor, draw windows as
   bloom layers with the moved chrome. Add placement, resize clamping, title
   drag, click to raise, `[X]`, hover cues, Alt-arrows, and move mode. Add steep
   scenarios at 80×24 and 30×10 for opening, focusing, moving, and closing
   windows, for modal blocking, and for ReducedMotion.
4. **Finish.** Add the optional open reveal under the motion rules. Update
   `docs/shell.md`, `docs/experience.md`, `README.md`, `status.md`, and the
   affected goldens.

## How this note meets #246

- **Which surfaces become windows, input routing, and the presentation mapping:**
  the [decisions](#decisions) above.
- **Draggable, focusable, closable windows over the shell frame, without a mouse:**
  the Windows screen, F8, Alt-arrows or move mode, and Ctrl-W.
- **The modal, palette, and dialog contract, and ReducedMotion:** the layer order
  and the motion rules.
- **Apps supply data, not views:** no SDK change, and the shell owns all window
  geometry.

## Answered questions

These decisions change the most for users, so they were answered before
stage 2. Each answer accepted the proposal:

1. Windows live on their own Windows screen, rather than floating over every
   screen.
2. Esc leaves the Windows screen and keeps every window open. Ctrl-W and `[X]`
   close a window.
3. F8 cycles windows by default, as the `window_next` shortcut.
4. A shell keeps at most 8 windows open.
