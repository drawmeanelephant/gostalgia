# IPC

The environment's IPC layer lives in `internal/ipc`; the host-specific
listener lives in `platform/`.

## One router, two transports

The same `ipc.Router` serves:

- **In-process callers** — `Router.Dispatch(ctx, req)` directly (used by the
  runtime self-test, tests, and any in-process code).
- **Local socket clients** — `cmd/gctl`, the Charm shell, and later
  out-of-process applications.

Because both go through the same routes, the wire never defines a second API.

## Protocol

NDJSON: one JSON value per newline-terminated line. Requests use positive
integer IDs, unique among outstanding calls on that connection. Responses
are correlated by `id` and may arrive out of order. Event notifications have
`kind: "event"` and no request ID.

```json
{"id":1,"method":"app/com.gostalgia.echo/echo","params":{"msg":"hi"}}
{"id":1,"ok":true,"data":{"msg":"hi","echoes":1}}
```

Errors: `{"id":1,"ok":false,"error":"..."}`. Handlers may not panic through
(`Dispatch` recovers). Frames must be smaller than 4 MiB including the
newline. Invalid JSON, missing IDs/methods, truncated or oversized frames,
duplicate outstanding IDs, and more than 32 outstanding requests disconnect
the peer. Oversized responses become a small correlated error response.
Application-level errors (unknown methods, invalid params, permission errors)
do not disconnect it.

The server dispatches handlers concurrently, with a bounded response queue
and one socket writer. Queued replies take priority over notifications.
Handlers must protect their own shared state and honor connection cancellation.
There is no call-cancellation wire method: canceled calls may still execute.

Route retraction is safe against in-flight dispatches: `Unhandle` and
`UnhandlePrefix` wait — for up to two seconds — for dispatches that
already resolved a route to finish before returning, so after either
returns normally no dispatch has or will reach a retracted handler. If the
bound elapses first, the routes are still retracted and can never be
entered again, but a stale in-flight dispatch keeps its dead route
reference and may still run or hang; the bound keeps one wedged handler
from blocking a cleanup path forever. Handlers run outside the router lock
(a handler may register routes — application launch does), but a handler
must not synchronously retract its own route.

## Authentication

The first request on every connection must be `auth` with either the operator
environment token or a scoped application token
(`{"id":1,"method":"auth","params":{"token":"..."}}`).
The handshake and each socket write have a five-second timeout.

- **Operator connections:** The operator token lives in `runtime.json` (mode
  0600 on unix; on Windows the mode does not map to an ACL — the file inherits
  the environment directory's permissions) under the environment root.
  Authenticated operator connections receive the `admin` capability set and
  operator principal identity.
- **Application connections:** When an application is launched, the runtime
  generates a distinct, cryptographically random token bound to the application
  identity, process ID, session ID, and declared manifest capability grants.
  App tokens are never written to `runtime.json`, leaked to process listings,
  or exposed in child environments.
- **Continuous validation & lifecycle revocation:** The IPC server validates
  connection credentials against the token store before dispatching each request
  and before writing each response or event notification. When
  an application stops or exits, its credentials are immediately revoked.
  Subsequent requests on existing connections are rejected with
  `unauthorized: credential revoked` and the connection is closed. Responses
  still pending at revocation are rejected too, but already-dispatched handler
  side effects cannot be rolled back. Event delivery with an invalid credential
  closes the connection and removes its subscription. New
  connection attempts with stale tokens fail handshake with `unauthorized: token
  revoked`.

This protects against accidental cross-user access and enforces least-privilege
grants between applications. Arbitrary external code remains untrusted until OS
sandbox enforcement is implemented — see security.md.

## Method namespaces

| Namespace | Owner |
|---|---|
| `sys/*` (`ping`, `status`, `shutdown`) | sys service |
| `proc/*` (`list`, `stop`) | process service |
| `fs/*` (`list`, `read`, `write`, `mkdir`, `remove`) | fs service |
| `doc/*` (`search`, `lookup`, `recents`, `favorites`, `associations`, `handoff`) | doc service |
| `session/*` (`list`, `get`, `create`, `close`, `attach`, `detach`, `workspace/get`, `workspace/set`, `workspace/clear`) | session service |
| `profile/*` (`list`, `get`, `active`, `create`, `update`, `switch`, `delete`) | profile service |
| `pkg/*` (`install`, `update`, `uninstall`, `list`, `inspect`, `rollback`) | package service |
| `backup/*` (`export`, `inspect`, `preview`, `restore`) | recovery service |
| `app/list`, `app/launch`, `app/stop`, `session/whoami` | sys service |
| `app/<app-id>/<method>` | the application instance |
| `events/v1/*` (`subscribe`, `unsubscribe`, `history`) | ipc service |

App routes require `ipc` from the incoming caller, then execute under the
app's manifest grant (not the caller's grant). SDK service calls likewise
replace capabilities. `app/stop` checks `proc.stop`; `app/launch` uses the
runtime lifetime, not the request lifetime. `session/*` read endpoints require
`session.read`; mutation endpoints (`create`, `close`, `attach`, `detach`,
`workspace/set`, `workspace/clear`) require `session.write`. `profile/*` read
endpoints (`list`, `get`, `active`) require `profile.read`; mutation endpoints
(`create`, `update`, `switch`, `delete`) require `profile.write`. The app contract includes the
[complete method schemas and capability table](applications.md#5-routes-and-scoped-service-calls).
Package reads require `package.read`, writes require `package.write`, and
permission expansion requires explicit operator confirmation. See
[packages](packages.md) for archive verification and transaction semantics.
Backup inspect and preview endpoints require `backup.read`; export and restore
endpoints require `backup.write`. See [recovery](recovery.md) for archive bounds,
conflict resolution, and transactional rollback semantics.

## Endpoints per platform

- macOS/Linux: unix domain socket inside a private per-boot directory,
  `$TMPDIR/gostalgia-ipc-<random>/gostalgia-<hash>.sock` (mode `0700`; the
  `<hash>` derives from the root to stay under the 104-byte limit). Per-app
  child sockets (`gs-app-<random>.sock`) share the same directory. The
  directory is masked from sandboxed children so they cannot discover or
  unlink live sockets.
- Windows: loopback TCP on an ephemeral port.

Clients dial via `platform.DialIPC(endpoint)`; both schemes are recorded in
`runtime.json`.

## Client

`ipc.Client` authenticates on construction and multiplexes up to 32 calls on
one connection. Additional calls fail immediately rather than creating an
unbounded backlog. A dedicated reader dispatches replies and notifications;
a dedicated writer preserves framing. Each call honors its context deadline
(default 30 s) and plain cancellation, including while queued.

Canceling a call while awaiting its response leaves other calls usable: late
responses are discarded by ID. An interrupted socket write, malformed frame,
or disconnect fails all pending calls and closes the event channel. Use a
fresh authenticated client after transport failure.

## Version 1 event subscriptions

All three event methods require `admin`, not merely `ipc`. They expose a
runtime-wide operator trail, not an app-scoped view. In-process callers can
query history through the router; subscribe/unsubscribe require a socket.
Unknown protocol versions are unknown methods.

### Subscribe and unsubscribe

`events/v1/subscribe` accepts:

- `topic`: required, exact event type or `"*"`; no prefix matching.
- `buffer`: 1–256, default 64 (zero means default).
- `after`: optional last processed event ID, exclusive; omit to start live.
- `epoch`: required with `after`, obtained from a previous subscribe/history
  response. This public runtime-lifetime identifier is not a credential.

One subscription is allowed per connection, with at most 128 buffered
subscriptions on the bus. The subscription response precedes every replay
or live notification for that subscription:

```json
{"id":2,"method":"events/v1/subscribe","params":{"topic":"proc.state","buffer":64}}
{"id":2,"ok":true,"data":{"epoch":"runtime-epoch","subscription":7,"cursor":42,"oldest":1,"resync":false}}
{"kind":"event","version":1,"subscription":7,"event":{"id":43,"type":"proc.state","source":"process","time":"2026-10-05T12:00:00Z"},"dropped":0}
{"id":3,"method":"events/v1/unsubscribe","params":{"subscription":7}}
{"id":3,"ok":true,"data":null}
```

IDs are bus-wide, monotonically increasing within one epoch. Filtered streams
can have ID gaps without drops. Replay and attachment to future events are
atomic under the bus lock, so concurrent publishers cannot introduce a
replay/live gap or reorder notifications.

Both the server subscription and Go client's event channel use bounded
**drop-oldest** queues: on overflow, discard the oldest queued record and keep
the new one. Publishers never wait for a remote consumer or perform socket
I/O. `dropped` is cumulative at enqueue time, including replay overflow; the
Go client adds its own local drops. A rise means the consumer must refresh
state or query retained history. At most one notification is being written
in addition to the server queue.

A slow application consumer cannot stall the client's reader or command
responses. A peer that stops reading the socket entirely can stall its own
connection until the five-second write deadline disconnects it, but not
publishers or other clients. RPC and event traffic share socket bandwidth,
so this is bounded delivery, not hard real-time latency.

Unsubscribe removes the bus subscription before acknowledging and sends no
new notification for it after the acknowledgement. Disconnect also removes
it. Already-buffered client notifications can still be drained from the
closed channel. A subscription context governs creation only; callers must
use `ClientSubscription.Close(ctx)` or `Client.Close()` to end its lifetime.
Canceling subscribe/unsubscribe closes the client to avoid an unowned
server-side subscription if the acknowledgement was lost.

```go
sub, err := client.Subscribe(ctx, ipc.SubscribeParams{Topic: "*", Buffer: 64})
if err != nil {
    return err
}
// Consume sub.Events while other goroutines use client.Call.
// Save sub.Info.Epoch and the last processed notification's Event.ID.
return sub.Close(ctx)
```

### Recent events / audit history

`events/v1/history` accepts required `topic`, optional `after` (default 0),
optional `limit` (1–256, default 256), and `epoch` (required if `after > 0`).
It returns:

```json
{"epoch":"runtime-epoch","events":[{"id":43,"type":"proc.state","source":"process","time":"2026-10-05T12:00:00Z"}],"oldest":1,"cursor":43,"next":43,"more":false,"resync":false}
```

The bus retains the newest **256 records in memory**, oldest first on
retrieval, evicting older records on every publish. There is no disk
persistence or time-based retention. `oldest` is the earliest globally
retained ID (0 when empty); `cursor` is the snapshot high-water mark.
Paginate with `after: next` and the returned epoch while `more` is true.

`resync: true` means the supplied cursor predates retention, exceeds this
bus's current ID, belongs to another epoch, or (on subscribe) replay overflowed
the requested buffer. An epoch mismatch replays/returns available records
from the new lifetime, explicitly reporting the discontinuity. Reconnect
with the last processed ID and its epoch, not the subscribe high-water mark
if replay has not been consumed. A reconnect is never a guarantee of full
history: after a gap, reload current state through the relevant RPCs.

**Redaction is structural:** live events and audit records contain only
`id`, `type`, `source`, and `time`. They never serialize or retain event
payloads, RPC params/results, auth tokens, credentials, document bodies,
paths, or arbitrary error messages. Type/source are non-sensitive publisher
identifiers, limited to 128 ASCII identifier bytes; free-form/oversized
metadata becomes `"redacted"`. Publishers must not encode secrets into
identifiers. The trail records event occurrence, not complete actions,
authentication attempts, or per-user security audit evidence. The UI must
fetch authorized current state rather than rely on payloads in this trail.

## Shutdown

`Server.Close` stops the listener and closes every live connection,
including idle ones, and never waits on a connection it did not close
itself. Connections accepted concurrently with `Close` are covered by the
same sweep, so shutdown cannot hang on a connected client.
Close is safe to call concurrently. It cancels connection contexts, removes
subscriptions, and joins transport goroutines without waiting for arbitrary
application handlers; route retraction still waits for its in-flight handlers.

## Verification

`internal/events` tests cover retention, pagination, deterministic drops,
concurrent publish/cancel, subscription limits, and structural redaction.
`internal/ipc` tests exercise simultaneous slow/fast calls and events, event
floods with slow consumers, disconnect/replay/epoch changes, malformed frames,
request limits, authorization, and shutdown with a deterministically blocked
event writer. Scoped-token integration also checks operator-only event access,
authenticated principal preservation, pending-response rejection on revocation,
and subscription cleanup when an event credential is revoked.
`test/e2e` boots the real runtime, subscribes to app lifecycle
events, and checks that document contents and the runtime token never enter
IPC history. Run these with `go test -race ./...`.
