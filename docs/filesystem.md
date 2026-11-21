# Filesystem

The environment filesystem lives in `internal/vfs`.

## The model

Applications and services speak **environment paths** (`/users/guest/documents`,
`/apps/manifests`); host paths never appear in any API, IPC payload, or error
message. `/` maps onto `<root>/vfs` on the host, and a mount table lets
subtrees shadow the root.

## Interfaces

### Core Filesystem (`FS`)

```go
type FS interface {
    fs.FS          // Open
    fs.StatFS      // Stat
    fs.ReadDirFS   // ReadDir
    fs.ReadFileFS  // ReadFile
    MkdirAll(path string) error
    WriteFile(path string, data []byte, perm fs.FileMode) error
    Remove(path string) error
    Rename(oldPath, newPath string) error
    RemoveAll(path string) error
    SaveAtomic(path string, data []byte, perm fs.FileMode) error
}
```

### Document Operations (`DocumentFS`)

Higher-level document manipulation operations are provided by `*vfs.VFS` and the `DocumentFS` interface:

```go
type DocumentFS interface {
    FS
    Copy(src, dst string, overwrite bool) error
    Move(src, dst string, overwrite bool) error
    Trash(path string) (*TrashEntry, error)
    Restore(trashID, dst string, overwrite bool) (string, error)
    ListTrash() ([]TrashEntry, error)
    EmptyTrash() (int, error)
    PurgeTrash(trashID string) error
}
```

Two path forms are accepted where documented:

- **Environment form** (`/users/guest`, `/tmp/x`) — at the `*VFS` layer and in
  IPC params. Leading slash, `/`-separated. Dot segments (`.`/`..`),
  backslashes, and interior empty elements are rejected outright.
- **io/fs form** (`users/guest`) — the raw backends (`HostFS`, `MemFS`), which
  enforce `fs.ValidPath` strictly so they work with `testing/fstest`,
  `fs.WalkDir`, and the rest of the stdlib.

## Document Operations & Recoverable Saves

### Atomic Saves (`SaveAtomic`)

To prevent data loss and corrupted documents during crashes or write failures:
1. Data is written to a unique staging file (`<file>.tmp.<timestamp>.<seq>`) in the target directory (ensuring same-mount placement).
2. The file is flushed and synced (`Sync()`) to storage.
3. The staging file is atomically renamed over the destination.
4. **Recovery fallback:** If the atomic rename fails, the staged data is preserved as a recoverable artifact (`<file>.recover`) rather than discarded, and a structured `*vfs.Error` with `RecoverPath` is returned.

Size bounds: documents are capped at `MaxDocumentSize` (16 MiB). IPC writes and saves are bounded to `MaxIPCWriteLimit` (4 MiB). Bulk data paths with their own documented ceilings — backup archives and restore payloads under `internal/recovery` — save through `SaveAtomicBounded`, which enforces a caller-supplied limit rather than the interactive cap.

### Copy & Move

- **Copy:** Supports both single files and recursive directory hierarchies. Collision detection honors the `overwrite` flag; copying a directory into its own descendant is prevented. Cross-mount copies (e.g. between root and `/tmp`) work transparently.
- **Move:** Same-mount moves use backend atomic rename. Cross-mount moves copy the entire source tree to the destination and cleanly remove the source upon successful copy.
- **Rename:** Strictly same-mount. Cross-mount rename requests fail explicitly with `ErrCrossMount` rather than silently degrading atomicity.

### Trash & Restore Lifecycle

Rather than immediate unrecoverable deletion, user document flows can use the trash lifecycle:
- **Trash Directory:** Per profile at `/users/<profile_id>/.trash`. The filesystem service points the trash tree at the active profile via `SetTrashDir` and re-points it on every profile switch, so one profile's trashed items never appear in another's bin and a deleted profile's trash dies with its `/users/<id>` tree.
- **Structure:**
  - `/users/<profile_id>/.trash/files/<id>/<basename>`: the preserved file or directory hierarchy.
  - `/users/<profile_id>/.trash/info/<id>.json`: metadata containing original path, trashed timestamp, file size, and directory flag.
- **Trash metadata is advisory:** `info/<id>.json` is an ordinary document. Restore derives the payload location from the trash ID and the `files/<id>/` tree — never from the recorded `trash_path` — and refuses payloads that resolve outside `files/`. Profile trash trees are also ungrantable: no path grant may cover them, so applications cannot author or tamper with metadata through the ordinary filesystem.
- **Restore:** Restores the item to its original path (or an explicit alternative destination). Parent directories are created as needed. Re-identifies collisions if `overwrite=false`.
- **Empty & Purge:** `EmptyTrash()` empties all trashed entries and files; `PurgeTrash(id)` permanently removes an individual entry.
- **Shared-mount items:** Items trashed from shared host mounts keep their origin policy: listing them requires `hostfs.read`, restoring or purging them requires `hostfs.write`, and the operator hostfs policy applies throughout.
- **Safety checks:** Deleting or trashing the root, active mount points, or the trash directory itself is prohibited.

## Structured Errors

Filesystem operations return structured `*vfs.Error` values containing machine-readable error codes:
- `ErrNotFound` (`not_found`): file or directory does not exist.
- `ErrExist` (`already_exists`): destination exists and `overwrite` was false.
- `ErrPermission` (`permission_denied`): capability check failed or read-only mount.
- `ErrConfinement` (`confinement_violated`): attempt to traverse outside the VFS root.
- `ErrInvalid` (`invalid_argument`): illegal path, empty name, or invalid ID.
- `ErrIsDir` (`is_directory`): expected file but got directory.
- `ErrNotDir` (`not_directory`): expected directory but got non-directory.
- `ErrCrossMount` (`cross_mount`): atomic rename attempted across distinct mount points.
- `ErrTooLarge` (`too_large`): document or payload exceeded size bounds.
- `ErrIO` (`io_error`): hardware or underlying system I/O error.

## Backends

- **HostFS** — a directory on the host, opened through `os.Root`. Path escapes
  via `..` or symlinks fail closed; errors reference the environment path, not
  the host path. Backslash names are rejected on every platform (they are
  separators on Windows, characters on Unix — one portable contract).
- **MemFS** — in-memory FS, used for `/tmp` (the environment's tmpfs) and as
  the platform test double.

### Windows metadata caveat

HostFS metadata comes from one source per file kind, so that `entry.Info()`,
`Stat()`, and `Open()+File.Stat()` agree: regular files and directories are
served through the open handle's `Stat`; special files (FIFOs, sockets,
devices, symlinks) are never opened — `Open` on a FIFO blocks until a writer
arrives on the other end — and report `Lstat` instead, which is what their
directory entries report too. For *directories* on
Windows, the OS itself reports inconsistent values across those calls for
freshly created directories (the first query of a new directory reports the
query time rather than its mtime), so exact cross-call equality is impossible
to guarantee there — `testing/fstest.TestFS` cannot pass for any host-backed
FS on Windows, including stdlib `os.DirFS`. Consequences:

- `TestHostFSMatchesFstest` runs on unix; on Windows a structural
  consistency suite (`TestHostFSConsistency`) runs instead.
- Directory `ModTime` values on Windows should be treated as advisory until
  the OS behavior is worked around (revisit with platform integration, [#44](https://github.com/drawmeanelephant/gostalgia/issues/44)).

## Mounts

`VFS.Mount("/tmp", vfs.NewMem())` shadows the root's `tmp` subtree; longest
prefix wins; `Unmount` restores the root view.

## Layout

```text
/               ← <root>/vfs on the host
├── users/<user>/
│   ├── documents/
│   ├── downloads/
│   ├── desktop/
│   ├── config/
│   └── .trash/        trash store (files/ and info/)
├── apps/
│   ├── manifests/     application manifests (JSON)
│   └── data/          partitioned app-private storage (/apps/data/<app_id>)
├── data/
├── mounts/            (reserved)
└── tmp/               ← memfs mount (ephemeral)
```

## App-Private Storage & Scoped VFS Grants

Rather than open-ended access to the entire root, applications receive scoped views (`ScopedVFS`):

### 1. App-Private Storage (`/apps/data/<app_id>`)
- Every application automatically receives a private partition under `/apps/data/<app_id>`.
- Applications have inherent read/write access to their own partition without requiring global filesystem permissions.
- Isolation is strictly enforced: an application cannot enumerate, read, write, stat, remove, rename, copy, or trash files belonging to another application's partition.
- `ReadDir("/apps/data")` presents a principal-aware view showing only the calling application's partition.
- Applications cannot delete or rename the `/apps/data` root or their partition root directory itself.

### 2. Scoped VFS Grants (`GrantStore`)
- Access to paths outside an application's private storage requires an explicit path-scoped grant issued by an operator.
- Grants specify:
  - `app_id`: the target application identifier.
  - `path`: canonical environment path (e.g. `/users/guest/documents/report.txt` or `/shared/workspace`).
  - `access`: access mode (`read` or `read-write`).
  - `recursive`: whether access extends to all descendants of a directory.
- Confinement checks prevent grant expansion:
  - Path traversal (`..`) is rejected.
  - Host symlinks resolving outside the authorized root fail closed with `path_escape`.
  - Same-mount rename enforces write permissions on both source and destination; cross-mount rename fails with `cross_mount`.
  - Cross-mount copy requires read on source and write on destination; cross-mount move requires write on both.
- Revocation:
  - Operators revoke grants via `fs/grant/revoke`.
  - Revoked grants fail immediately on subsequent operations with `permission_denied: grant revoked`.
  - Grants are scoped to `app_id`; standing grants persist across application process restarts until explicitly revoked. Revoked grants remain revoked across restarts.
  - Session-bound grants (`session_bound`, issued by `doc/handoff`) are bound to the receiving application's run: they are revoked when that app exits, crashes, or is stopped, and on any handoff failure after issuance.

### 3. Trash Isolation
- Trash is per profile: payloads and metadata live under the active profile's `/users/<profile_id>/.trash` and are repointed on profile switch.
- Profile trash trees are ungrantable — no `fs/grant` may cover `/users/<id>/.trash`, so applications cannot write trash metadata or payloads directly.
- Restore derives the payload location from the trash ID and `files/<id>/`, treating the recorded metadata as advisory.
- Applications listing the trash (`fs/trash/list`) only see items that originated from paths they are authorized to access; items trashed from shared host mounts additionally require `hostfs.read` to list and `hostfs.write` to restore or purge.
- Restoring or purging trash items belonging to another application or unauthorized paths is denied.
- `EmptyTrash()` only purges the calling application's trashed entries.

## IPC Surface (`fs/*`)

The filesystem is exposed to applications and shell commands over IPC:

| Method | Capability Required | Description |
|---|---|---|
| `fs/list` | `fs.read` (or app private/granted) | Lists directory entries with sizes, modes, and directory indicators. |
| `fs/stat` | `fs.read` (or app private/granted) | Returns path metadata (size, mod_time, is_dir, mode). |
| `fs/read` | `fs.read` (or app private/granted) | Reads file data in base64. Supports bounded reads with `offset` and `limit` (max 4 MiB). Files > 4 MiB require bounded parameters. |
| `fs/write` | `fs.write` (or app private/granted) | Writes base64 data to a file (bounded to 4 MiB). |
| `fs/save` | `fs.write` (or app private/granted) | Atomic recoverable save with collision detection (`overwrite` flag, perm). |
| `fs/mkdir` | `fs.write` (or app private/granted) | Creates directories recursively (`MkdirAll`). |
| `fs/remove` | `fs.write` (or app private/granted) | Removes a file, empty directory, or tree (`recursive: true`). |
| `fs/rename` | `fs.write` (or app private/granted) | Atomic same-mount rename with collision detection. |
| `fs/copy` | `fs.read` + `fs.write` | Copies file or directory tree with collision detection. |
| `fs/move` | `fs.write` (or app private/granted) | Moves file or directory with collision detection across mounts. |
| `fs/trash` | `fs.write` (or app private/granted) | Moves target to `.trash/files` and registers metadata in `.trash/info`. |
| `fs/restore`| `fs.write` (or app private/granted) | Restores trashed item by ID with collision handling. |
| `fs/trash/list` | `fs.read` | Lists trashed items accessible to the caller. |
| `fs/trash/empty` | `fs.write` | Permanently deletes trashed entries accessible to the caller. |
| `fs/grant` | `admin` | Issues a path-scoped capability grant to an application. |
| `fs/grant/revoke` | `admin` | Revokes an issued grant by ID. |
| `fs/grant/list` | Caller app or `admin` | Lists active grants for an application or across the system. |

## Document Operations Surface (`doc/*`)

The document subsystem provides permission-aware search, indexed lookup, persistent recents and favorites, registered associations, and scoped open-with handoff over IPC:

| Method | Capability Required | Description |
|---|---|---|
| `doc/search` | `fs.read` (or granted) | Bounded permission-aware recursive VFS search filtered to caller directory grants. |
| `doc/lookup` | `fs.read` (or granted) | Exact name and extension document lookup with stale path pruning. |
| `doc/recents` | Caller app or `admin` | Lists recent documents filtered by caller permissions, with optional existence checks. |
| `doc/recents/add` | Caller app or `admin` | Records a document access into persistent recents (`/users/guest/config/recents.json`). |
| `doc/recents/remove` | Caller app or `admin` | Removes an entry from recents. |
| `doc/recents/clear` | Caller app or `admin` | Clears caller or all recents. |
| `doc/favorites` | Caller app or `admin` | Lists pinned favorite documents (`/users/guest/config/favorites.json`). |
| `doc/favorites/add` | Caller app or `admin` | Adds or updates a pinned favorite with rank and optional custom label. |
| `doc/favorites/remove` | Caller app or `admin` | Removes a pinned favorite. |
| `doc/favorites/reorder`| Caller app or `admin` | Reorders favorite documents by ordered paths. |
| `doc/favorites/clear` | Caller app or `admin` | Clears pinned favorites. |
| `doc/associations` | `ipc` | Lists all registered document type associations. |
| `doc/associations/resolve` | `ipc` | Resolves target application for a document path or extension. |
| `doc/associations/register`| `admin` | Registers a custom document type association. |
| `doc/handoff` | Caller app or `admin` | Versioned open-with contract. Launches the target app if needed, then issues a scoped single-document session-bound grant (default mode `read`) without broader directory access, and dispatches open. The grant is revoked on dispatch failure and when the target app exits. |

## Testing

Both backends pass `testing/fstest.TestFS`. Dedicated suites verify:
- Atomic save replacement, staging cleanup on failure, and `.recover` artifact creation.
- Recursive directory and file copy and move across mounts.
- Trash, list, restore, and purge lifecycle with metadata preservation.
- Confinement enforcement (rejection of dot segments, escapes, symlink traversal, and mount overrides).
- Payload bounds and structured error code propagation over IPC.
