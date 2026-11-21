# Application packages

Gostalgia installs ZIP-based `.gpkg` archives without using a host package
manager. Package verification never runs the binary. Installed applications
must use external mode and explicitly select `sandbox` or `strict` isolation.
Launch still enforces host isolation availability, capabilities, path grants,
and operator platform policies. A signature proves publisher provenance only,
not that code is safe. Unsigned packages are accepted and reported as unverified.

## Format v1

An archive contains `package.json`, `manifest.json` (the strict `sdk.Manifest`
format), a nonempty executable, and optional regular resource files. For example:

```text
package.json
manifest.json
bin/counter
```

The manifest's `executable` is a portable, archive-relative filename, not a host
path or command name. Installation resolves it to the immutable installed
version. Identity and version must agree with `package.json`.

```json
{
  "format_version": 1,
  "id": "com.example.counter",
  "version": "1.0.0",
  "publisher": "Example",
  "checksums": {
    "bin/counter": "<lowercase SHA-256 hex>",
    "manifest.json": "<lowercase SHA-256 hex>"
  }
}
```

Every regular file except `package.json` must have exactly one matching SHA-256
checksum. For signatures add `key_id` and a base64 Ed25519 `signature`.
The signed message is Go `encoding/json.Marshal` of the metadata fields in this
order: `format_version`, `id`, `version`, `publisher`, `key_id`, `checksums`.
Empty `publisher`/`key_id` fields are omitted; checksum map keys are sorted by
`encoding/json`. The `signature` field is omitted from the signed message.
`internal/pkg.Metadata.SigningBytes()` defines the encoding.

The operator configures trusted public keys before boot in
`<root>/config/package-trust.json`, a JSON object mapping key IDs to base64
Ed25519 public keys. Keys bundled in packages are never trusted. Signed
packages with unknown keys, invalid signatures, or no publisher are rejected.
Key IDs identify operator-approved publisher keys; the publisher display name
is an authenticated claim, not an independently verified legal identity.

Limits apply before extraction: 50 MiB compressed, 50 MiB total uncompressed,
500 archive entries and extracted paths, 64 KiB per JSON document, and a maximum
100:1 compression ratio per file. ZIP64 and multi-disk archives are unsupported.
Traversal, absolute paths, backslashes, NULs, Windows device/alternate-stream
names, duplicate/case-alias paths, links, and special files are rejected.
CRC and checksum failures reject the whole package.

## Permissions and confirmation

`permissions` lists capabilities. `path_grants` optionally requests scoped VFS
access:

```json
"path_grants": [
  {"path": "/users/guest/documents", "access": "read", "recursive": true}
]
```

Grant paths must be canonical absolute environment paths. Root, installed code,
and other apps' private storage cannot be requested. Own private storage at
`/apps/data/<app-id>` remains available independently.

Inspection shows added/removed capabilities, added/removed path grants, any
strict-to-sandbox reduction, and an `expansion` flag. Read-to-write, newly
recursive, or broader path access is expansion. Installs, updates, and rollbacks
that expand permissions require explicit operator confirmation. Applications
cannot confirm expansions, even if they hold `package.write`.

Successful replacement revokes old path grants (including ad hoc operator
grants) and issues only the new manifest's grants. Inspection includes currently
active grants separately. Failed replacement leaves old grants intact.

## Control surface

Archive paths are **VFS paths**, not arbitrary host filenames. Place downloads
under `/users/guest/downloads` (host backing: `<root>/vfs/users/guest/downloads`).

```text
gctl pkg list
gctl pkg inspect --archive /users/guest/downloads/counter.gpkg
gctl pkg install /users/guest/downloads/counter.gpkg --confirm-permissions
gctl pkg inspect com.example.counter
gctl pkg update /users/guest/downloads/counter-2.gpkg --confirm-permissions
gctl pkg rollback com.example.counter
gctl pkg uninstall com.example.counter
```

The shell accepts the same subcommands as `pkg` or `package`, with relative and
DOS-style environment paths. `gctl package` is also an alias.

| IPC route | Capability | Parameters |
|---|---|---|
| `pkg/list` | `package.read` | none |
| `pkg/inspect` | `package.read` | exactly one of `id` or archive `path` |
| `pkg/install`, `pkg/update` | `package.write` | archive `path`, optional `confirm_permissions` |
| `pkg/rollback` | `package.write` | `id`, optional `confirm_permissions` |
| `pkg/uninstall` | `package.write` | `id` |

Archive reads also enforce existing filesystem capabilities, path grants, and
shared-host policy. Package capabilities do not grant arbitrary file access.

## Transactions and lifecycle

Files are verified and staged in a private temporary directory before stopping
any live app. Maintenance blocks new launches, revokes launch credentials, and
waits for process termination and cleanup. Initializing instances or stop
timeouts abort the operation rather than replace live code.

`/apps/<app-id>/versions/<generation>` holds immutable payloads. Atomic
replacement of `/apps/<app-id>/state.json` selects the active and previous
versions, with no partially replaced directory. A failed commit preserves the
old pointer, binary, manifest, and grants. Explicit rollback swaps the two
complete versions after reverification. Boot reverifies both retained versions.
Only one previous version is retained; failed cleanup can leave inert orphan
directories that are never selected or launched.

A package whose boot-time load fails — corrupt `state.json`, a missing or
corrupt active version, an invalid pointer — is quarantined rather than
bricking the environment boot. Quarantined packages are logged, reported in
`pkg list`/`pkg inspect` with `quarantined` and `quarantine_reason`, never
registered for launch, and refuse rollback. `pkg uninstall` removes a
quarantined package, which is the repair path before reinstalling. A
corrupt or missing previous version alone is rollback-only damage: the
pointer is dropped and the package stays usable.

Update and rollback stop live apps but do not automatically relaunch them.
After a failure occurring after termination, the previous version remains
runnable via `launch APP-ID`. Uninstall atomically retracts the app directory
and registration, revokes grants, and leaves private user data under
`/apps/data/<app-id>` intact. Compiled-in apps cannot be replaced or uninstalled.
Atomic visibility does not promise recovery from hardware/power-loss failures.
