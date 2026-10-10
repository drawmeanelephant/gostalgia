# Portable Backup and Disaster Recovery

Gostalgia provides verifiable personal backup, export/import, and transactional
restore under `internal/recovery`. It allows users and operators to preserve and
migrate personal state (documents, workspace configurations, profiles, and
preferences) without leaking live credentials, network endpoints, or running
environment artifacts.

## Archive Format (`.gbar`)

Backups are standard ZIP-compatible archives stored with the `.gbar` extension.
The structure strictly mirrors the portable environment paths under `data/`:

```
backup.gbar
├── backup.json                                      # Signed manifest descriptor
└── data/
    ├── config/
    │   ├── profiles.json                            # Profile registry
    │   └── system.json                              # System preferences (optional)
    └── users/
        ├── guest/
        │   ├── config/
        │   │   ├── user.json                        # Theme and user preferences
        │   │   └── workspace.json                   # Sanitized window & directory layout
        │   └── documents/
        │       └── notes.txt                        # User files
        └── <profile_id>/
            └── ...
```

### Manifest Schema (`backup.json`)

The manifest contains metadata, profile summaries, and a content-addressed SHA-256
index of all included files:

```json
{
  "format_version": 1,
  "created_at": "2026-10-06T20:30:00Z",
  "source_version": "0.1.0",
  "description": "User notes and settings backup",
  "profiles": [
    {
      "id": "guest",
      "name": "Guest User"
    }
  ],
  "files": [
    {
      "vfs_path": "/users/guest/documents/notes.txt",
      "arc_name": "data/users/guest/documents/notes.txt",
      "size": 15,
      "sha256": "e301be22f004a6acc03a405a2e84d485ec816e30fccf2cabe274d1a9738fb965",
      "modified": "2026-10-06T20:25:00Z"
    }
  ]
}
```

## Security and Invariants

1. **Strict Resource Bounds**:
   - Max compressed archive size: 100 MiB (`MaxArchiveBytes`)
   - Max total extracted size: 200 MiB (`MaxExtractBytes`)
   - Max total file count: 5,000 files (`MaxFiles`)
   - Max compression ratio: 100x (`MaxCompressionRatio`)
   - Max single file size: 32 MiB (`MaxSingleFileBytes`)

2. **Zip Central Directory Pre-Verification**:
   - The archive central directory is parsed and validated in its entirety before
     inflating any file content. Discrepancies between file header lengths,
     declared manifest entries, or compression bounds cause immediate rejection.

3. **Path Traversal & Canonical Path Enforcement**:
   - Archive entry paths must use forward slashes and begin with `data/`.
   - Paths containing `..`, absolute roots, drive letters, null bytes, or
     control characters are refused.
   - VFS mapping is restricted to allowed roots (`/config` and `/users/<id>/*`).
     Restoring or exporting outside allowed roots (such as `/runtime.json`,
     `/apps`, `/tmp`, `/logs`, or host mounts) is strictly denied.

4. **Secret Scrubbing & Credential Exclusion**:
   - Live session credentials, auth tokens, operator tokens in `runtime.json`,
     transient locks (`.tmp.*`, `.recover`), logs, and trash (`.trash`) are
     never included in backups.
   - Shell command history in `workspace.json` is automatically scrubbed of
     authentication or token commands before export.
   - Active operator tokens are scanned during export: if any file accidentally
     contains a live token, export is halted with an explicit refusal error.

## Conflict Detection & Transactional Rollback

### Preview and Conflict Inspection

Restores can be previewed without modifying live VFS state. The preview compares
manifest hashes with live files and groups items into:
- `create`: New files not currently present in the live system.
- `identical`: Files whose live SHA-256 matches the backup copy.
- `conflicts`: Files present in both places whose content has diverged.

### Conflict Strategies

- `abort` (default): Halts with an error if any file conflict is detected. No
  changes are made to the live system.
- `overwrite`: Replaces conflicting live files with the versions from the backup.
- `skip`: Preserves existing live files and restores only new files.

### Transactional Rollback

Restore operations are journaled. Prior to modifying an existing file, its original
contents are captured. If any write fails (e.g. disk full, permission failure, or
cancellation), the rollback journal automatically:
1. Deletes all newly created files.
2. Restores all overwritten files to their exact prior contents.
3. Leaves the live environment in an intact, uncorrupted state.

## IPC Methods

The recovery service owns the `backup/*` namespace and is guarded by `backup.read`
and `backup.write` capabilities:

| Method | Capability | Parameters | Description |
|---|---|---|---|
| `backup/export` | `backup.write` | `path`, `profile_id`, `include_system`, `description` | Gathers state and writes `.gbar` |
| `backup/inspect` | `backup.read` | `path` | Validates archive and returns manifest |
| `backup/preview` | `backup.read` | `path` | Reports create, identical, and conflicting files |
| `backup/restore` | `backup.write` | `path`, `strategy`, `profile_id` | Applies backup transactionally |

The capabilities gate *whether* a caller may use these methods; they do not
widen filesystem access. Operator and admin callers operate on the full
environment filesystem. Application principals operate through their
grant-scoped VFS view — the same authorization `fs/*` applies — so an app can
only export, inspect, preview, or restore paths covered by its own grants and
its private storage partition. Paths on shared host mounts additionally
require the `hostfs.read`/`hostfs.write` capabilities. A restore archive
naming ungranted paths fails during planning, before any write; an export
skips unreadable subtrees and records each omission in the result.

## CLI & Shell Usage

### Control Tool (`gctl`)

```bash
# Export all profiles and personal data
gctl backup export /users/guest/downloads/backup.gbar --description "Full backup"

# Export a specific profile
gctl backup export /users/alice/downloads/alice.gbar --profile alice

# Inspect manifest
gctl backup inspect /users/guest/downloads/backup.gbar

# Preview conflicts against live state
gctl backup preview /users/guest/downloads/backup.gbar

# Restore with explicit conflict strategy
gctl backup restore /users/guest/downloads/backup.gbar --strategy overwrite
```

### Shell (`gostalgia`)

```text
C:\> backup export "downloads/mybackup.gbar"
C:\> backup inspect "downloads/mybackup.gbar"
C:\> backup preview "downloads/mybackup.gbar"
C:\> backup restore "downloads/mybackup.gbar" --strategy skip
```
