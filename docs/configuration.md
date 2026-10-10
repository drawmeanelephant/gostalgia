# Gostalgia configuration & layered settings specification

Gostalgia implements a multi-tier, layered configuration engine with schema validation, keybinding conflict detection, atomic file persistence, rollback-safe in-memory live previewing, and live change events.

## 1. Layer hierarchy and precedence

The configuration store evaluates settings with strict layer precedence:

```
┌────────────────────────────────────────────────────────┐
│                   LayerPrecedence                      │
├────────────────────────────────────────────────────────┤
│ 1. Preview  (in-memory ephemeral session overrides)    │
│ 2. App      (app-scoped user preferences: apps.<id>.*) │
│ 3. User     (user preferences: /users/<user>/config)   │
│ 4. System   (host/administrator: /system/config)       │
│ 5. Default  (built-in immutable defaults)              │
└────────────────────────────────────────────────────────┘
```

When querying a setting via `Get` or `Snapshot`:
- Higher layers override lower layers.
- If an in-memory preview is active, its value takes precedence over all persistent layers.
- App-scoped settings (`apps.<id>.<path>`) override global user settings when queried with `QueryOpts{AppID: "..."}`.
- User settings override system settings.
- System settings override built-in defaults.
- Corrupt or invalid JSON files on disk are automatically quarantined, falling back safely to default configuration without crashing the environment.

## 2. Built-in defaults

Default settings are declared in `internal/config/defaults.go`:

| Path | Type | Default | Description |
|---|---|---|---|
| `theme` | `string` | `"nostalgia"` | Active aesthetic theme (`nostalgia`, `midnight`, `monochrome`, `high-contrast`, `high-contrast-light`). |
| `accessibility.color_mode` | `string` | `"ansi256"` | Terminal color fidelity (`plain`, `ansi256`, `truecolor`). |
| `accessibility.reduced_motion` | `bool` | `false` | Suppresses animations and decorative spinners. |
| `accessibility.high_contrast` | `bool` | `false` | Enables high-contrast borders and text tokens. |
| `startup.view` | `string` | `"home"` | Default landing view on boot (`home`, `prompt`, `launcher`, `tasks`, `notifications`). |
| `notifications.enabled` | `bool` | `true` | Enables notification reception. |
| `notifications.dnd` | `bool` | `false` | Do-Not-Disturb: silences live toasts. |
| `shortcuts.help` | `string` | `"f1"` | Shell shortcut for help / home. |
| `shortcuts.apps` | `string` | `"f2"` | Shell shortcut for application launcher. |
| `shortcuts.tasks` | `string` | `"f5"` | Shell shortcut for task manager. |
| `shortcuts.notifications` | `string` | `"f6"` | Shell shortcut for notification center. |
| `shortcuts.settings` | `string` | `"f7"` | Shell shortcut for settings application. |
| `shortcuts.palette` | `string` | `"ctrl+p"`| Shell shortcut for quick command palette. |

## 3. Validation and keybinding conflict prevention

Before any configuration value is accepted by `Set` or `Preview`:
- **Path format**: Paths must be non-empty dot-separated alphanumeric segments (`^[a-z0-9_]+(\.[a-z0-9_]+)*$`).
- **Schema typing**: Values must match their expected type (string, bool, int/float).
- **Domain restrictions**:
  - `theme`: Must be one of the known theme identifiers.
  - `accessibility.color_mode`: Must be `plain`, `ansi256`, or `truecolor`.
  - `startup.view`: Must be a recognized shell mode (`home`, `prompt`, `launcher`, `tasks`, `notifications`).
- **Keybinding conflict detection**:
  - Key combinations for shell actions (`shortcuts.*`) are checked against all effective keybindings.
  - If a requested shortcut key matches an existing action, `Set` rejects the operation with a descriptive `KeybindingConflictError` detailing the colliding actions and keys.

## 4. Configuration IPC Service (`config/*`)

The runtime exposes configuration management over IPC via `ConfigService`. Access requires caller authorization and appropriate capabilities:

| Method | Parameters | Permissions | Description |
|---|---|---|---|
| `config/get` | `{path: string, layer?: string, app_id?: string}` | `config.read` or `admin` | Retrieves effective value or layer-scoped value. Returns `{path, value, found}`. |
| `config/set` | `{path: string, value: any, layer?: string, app_id?: string}` | `config.write` or `admin` | Validates, updates, persists atomically, and publishes a change event. |
| `config/unset` | `{path: string, layer?: string, app_id?: string}` | `config.write` or `admin` | Deletes a path from the specified persistent layer. |
| `config/reset` | `{layer?: string, app_id?: string}` | `config.write` or `admin` | Resets the target layer to an empty state. |
| `config/list` | `{layer?: string, app_id?: string}` | `config.read` or `admin` | Lists values across defaults, system, user, app, and preview, plus merged effective. |
| `config/snapshot`| `{app_id?: string}` | `config.read` or `admin` | Returns the complete merged effective configuration tree. |
| `config/preview` | `{path: string, value: any}` or `{settings: map}` | `config.write` or `admin` | Applies an in-memory preview override owned by the caller and emits live change events. |
| `config/cancel_preview` | none | `config.write` or `admin` | Reverts the caller's staged preview overrides and emits change events with restored values. |
| `config/commit_preview` | `{layer?: string}` | `config.write` or `admin` | Persists the caller's staged preview overrides to disk (defaulting to user layer). `system` additionally requires `admin`. |

| `config/validate` | `{path?: string, value?: any, batch?: map}` | `config.read` or `admin` | Pre-validates paths, values, and shortcut conflict absence without writing. |
| `config/explain` | `{path: string, app_id?: string}` | `config.read` or `admin` | Returns resolution breakdown across every layer and identifies the winning layer. |

Preview overrides are staged per caller: application callers own the paths they stage under their own application ID, while operator and in-process callers share the global scope. `config/cancel_preview` and `config/commit_preview` only affect the caller's own staged values, so one principal can never commit or discard another's preview. Staged values remain visible in effective reads while active, preserving live theme previews; `config/unset` and `config/reset` on the `preview` layer are likewise scoped to the caller's own staged paths. For `config/unset`, `config/reset`, `config/get`, `config/list`, `config/snapshot`, and `config/explain`, an application caller's `app_id` is pinned to the calling application — applications may only address their own app layer, while operators may address any.

The user layer's `apps.<id>.*` subtree is each application's app-layer backing store, so raw paths under it carry the same boundary: an application caller may only address `apps.<its-own-id>` and its descendants — `config/get`, `config/set`, `config/unset`, `config/reset`, `config/preview`, and `config/explain` reject foreign `apps.*` paths (including the bare `apps` root and partial app-ID prefixes), and `config/list`/`config/snapshot` confine the `apps` subtree of the effective, user-layer, and preview-layer views to the caller's own namespace. Operators are unconstrained.

## 5. Live updates and event subscriptions

When configuration values change via `Set`, `Preview`, `CancelPreview`, `CommitPreview`, or `Reset`, the store publishes a `config.changed` event over the event bus:

```json
{
  "type": "config.changed",
  "data": {
    "layer": "user",
    "path": "theme",
    "value": "midnight",
    "prev_value": "nostalgia",
    "is_preview": false
  }
}
```

Clients subscribe to `config.changed` using the IPC subscription mechanism (`ipc.SubscribeParams{Topic: "config.changed"}`). The Gostalgia shell listens for these events and dynamically updates its active lipgloss theme, color mode, motion preferences, keybindings, and views without restarting.
