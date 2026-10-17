# Local API

slk exposes an opt-in HTTP/JSON API over a private Unix socket on macOS and Linux.
Local scripts and tools can read conversations, search messages, open DMs, and
post messages through the workspaces connected in the running TUI. Each request
names its workspace; changing the workspace on screen does not redirect API calls.

Enabling the API enables both reads and writes. There are no API authentication
tokens, credential files, or separate write settings. The private socket directory
and socket filesystem permissions control access. Processes running as your OS
user can use the API with your connected Slack account's permissions.

## Setup and discovery

Add this to `~/.config/slk/config.toml` and restart slk:

```toml
[local_api]
enabled = true
# Optional: absolute path, in a directory owned by you with mode 0700.
# socket_path = "/home/me/.local/run/slk/api.sock"
```

The default socket is `$XDG_RUNTIME_DIR/slk/api.sock` when `XDG_RUNTIME_DIR` is an
absolute directory owned by you with mode `0700`. Otherwise it is
`<system temp directory>/slk-<uid>/api.sock`. On macOS, the system temp directory
usually follows `TMPDIR`; it is not necessarily `/tmp`. The socket directory is
created with mode `0700`; an existing directory must already have that mode.
Configured paths containing symlink components are rejected. Ancestors must be
owned by you or root and must not permit other users to replace the private
directory (shared writable ancestors such as `/tmp` require the sticky bit). Socket paths are
limited to 100 bytes; configure a shorter path if needed.

The directory contains:

| File | Purpose |
|---|---|
| `api.sock` | HTTP Unix socket, mode `0600`. |
| `api.json` | Atomically published discovery information, mode `0600`. |
| `api.lock` | Exclusive process lock, mode `0600`; remains after shutdown. |

`api.json` contains `socket_path`, `pid`, `instance_id`, `api_versions`, and
`capabilities`. The instance ID changes on every start. It identifies the
process for safe write retries; it is not a secret or authentication credential.
The socket and discovery file are removed on normal shutdown. A crashed process's
stale socket is recovered under the process lock. An active socket is preserved.
Only one server can use a socket directory; use separate private directories for
independent instances.

The listener starts while workspaces are connecting. Explicitly enabling the API
with an invalid path, conflicting listener, or unsupported platform fails startup
with an error. When disabled, no API listener or artifacts are created. Demo mode,
export, and other one-shot commands do not start the API. There is no headless
server command, TCP listener, browser integration, or Windows API transport.

The [OpenAPI 3.1 contract](Local-API.openapi.json) describes the request and response
schemas. All URLs use the synthetic HTTP host `slk.local`. No DNS lookup or TCP
connection is involved when using a Unix socket. Requests carrying an `Origin`
header are rejected, and responses do not enable CORS.

## Examples

Set `SLK_API_SOCKET` to the socket from discovery. `jq` is only an example client
convenience; slk does not require it.

```sh
export SLK_API_SOCKET="$XDG_RUNTIME_DIR/slk/api.sock"

curl --unix-socket "$SLK_API_SOCKET" http://slk.local/v1/status
curl --unix-socket "$SLK_API_SOCKET" http://slk.local/v1/workspaces

curl --unix-socket "$SLK_API_SOCKET" \
  'http://slk.local/v1/workspaces/T123/conversations/C123/messages?limit=20'

curl --unix-socket "$SLK_API_SOCKET" --get \
  --data-urlencode 'q=in:general build' \
  http://slk.local/v1/workspaces/T123/search
```

Every POST requires the current instance ID and a unique idempotency key:

```sh
SLK_API_INSTANCE=$(curl --silent --show-error --unix-socket "$SLK_API_SOCKET" \
  http://slk.local/v1/status | jq -r '.data.instance_id')

curl --unix-socket "$SLK_API_SOCKET" \
  -H "X-Slk-Instance: $SLK_API_INSTANCE" \
  -H 'Idempotency-Key: 25b35002-94f4-468b-a5f1-63eac5feeb44' \
  -H 'Content-Type: application/json' \
  --data '{"text":"Build finished.","thread_ts":"1792240000.000100"}' \
  http://slk.local/v1/workspaces/T123/conversations/C123/messages
```

Omit `thread_ts` to send a top-level message. Add `"broadcast":true` to also
broadcast a reply to its channel. Broadcast defaults to false and requires a
thread timestamp. Choose a new idempotency key for each intended operation; keep
that key when retrying the same operation.

To open or find a DM, POST `{"user_ids":["U123"]}` to
`/v1/workspaces/T123/conversations` with the same required headers. Supply up to
eight distinct user IDs to open a group DM. This does not navigate the TUI.

## Endpoints

All IDs are explicit Slack IDs. Slack timestamps are decimal strings, never
floating-point JSON numbers. Workspace names/slugs and conversation names are not
accepted in place of IDs.

| Method and path | Parameters and behavior |
|---|---|
| `GET /v1/status` | API version, instance ID, capabilities, startup state, idempotency retention. Local only. |
| `GET /v1/workspaces` | Connected/configured sessions and their `connecting`, `ready`, or `unavailable` bootstrap state. Local only. |
| `GET /v1/workspaces/{team}/conversations` | Joined, non-archived channels, private channels, DMs and group DMs. `limit`, `cursor`. |
| `GET /v1/workspaces/{team}/conversations/{channel}` | Conversation metadata. Does not join or open it. |
| `GET /v1/workspaces/{team}/conversations/{channel}/messages` | Top-level history, newest first. `limit`, `cursor`, optional exclusive `oldest` and `latest`. |
| `GET /v1/workspaces/{team}/conversations/{channel}/threads/{thread_ts}/replies` | Replies oldest first, parent separately in `meta.parent`. `limit`, `cursor`. |
| `GET /v1/workspaces/{team}/search` | Required `q`, optional `limit`, `cursor`. Slack search syntax, relevance order. |
| `GET /v1/workspaces/{team}/users/{user}` | ID, display/real name, bot/deleted flags; no email or phone. |
| `POST /v1/workspaces/{team}/conversations` | `user_ids`; returns `channel_id`, `already_open`. |
| `POST /v1/workspaces/{team}/conversations/{channel}/messages` | `text`, optional `thread_ts`, `broadcast`; returns the acknowledged message. |

Reads fetch from Slack, with no silent SQLite fallback, implicit joining, or
read-cursor changes. The conversation list is account membership, not the TUI's
sidebar ordering or starred/filter state. Search uses Slack's index and is not a
complete archival export; use `slk export` for that. Workspace `ready` describes
bootstrap, not an assertion that the WebSocket is currently connected. Web API
calls may continue working during a WebSocket outage.

Successful reads and DM opening return `200`; message posting returns `201`.
Responses have `data` and `meta.request_id`/`meta.source` (`local` or `slack`).
Slack-backed collections use arrays, including `[]` for empty results, and include
`meta.next_cursor`, which is a string or `null` at the end. `meta.truncated=true`
indicates that Slack reported more results without a usable continuation or that
search reached the 100-page ceiling. A page's `limit` is a maximum; a replies
page containing its parent may have fewer reply items.

```json
{
  "data": [],
  "meta": {
    "request_id": "example-request-id",
    "source": "slack",
    "next_cursor": null
  }
}
```

Pagination defaults to 50 items, maximum 100. Continue with the returned cursor
and identical route/query parameters. Cursors are signed and scoped to the
workspace, route, filters, page size, and current process. Changing any of these
or restarting slk produces `400 invalid_cursor`; restart pagination and deduplicate
by workspace/channel/timestamp. Pages are not a transactionally consistent
snapshot of Slack.

Messages contain `team_id`, `channel_id`, `ts`, nullable `thread_ts`/`user_id`/
`bot_id`, `text`, `text_format: "slack_mrkdwn"`, `subtype`, `reply_count`, file
references, and `has_blocks`/`has_attachments`. File references contain only ID,
title and permalink. Full blocks, attachments, and private download URLs are not
exposed. A bot post's text may be empty when its body lives in blocks or
attachments; the flags identify this limitation.

Send input uses slk's existing CommonMark-like compose conversion. The response
contains the converted Slack mrkdwn text and acknowledged timestamp. Use explicit
Slack ID markup for mentions; there is no @name resolution. Text beginning with
`/` is sent as message text, not run as a slash command.

## Write retries

`X-Slk-Instance` must match discovery or `/v1/status`; an earlier process's
instance ID receives `409 instance_mismatch` before any send. Do not silently
replace this header and replay an uncertain earlier operation after a restart.
First reconcile it against Slack history.

`Idempotency-Key` contains 1–128 printable ASCII bytes; a random UUID is a useful
choice. Deduplication is scoped to the workspace, route and key. Concurrent
identical requests dispatch at most once. Completed duplicates replay the original
status/body; reusing a key with a different validated request returns
`409 idempotency_conflict`. A duplicate while the first operation runs receives
`409 operation_in_progress` and `Retry-After`.

An admitted write continues under its own 30-second deadline if its client
disconnects. A lost connection, timeout or missing/invalid Slack acknowledgement
after dispatch returns `409 outcome_unknown` with `retryable:false`. It may already
have succeeded. That key is never dispatched again within the instance; check
history before consciously issuing a new key.

Definitive Slack refusals are retained too. A definitive Slack rate limit returns
`429 slack_rate_limited` and `Retry-After`; the same request/key may dispatch again
after that delay. The server does not sleep or retry upstream writes automatically.

The in-memory ledger keeps terminal outcomes for 24 hours and has a 10,000-entry
cap. Pending and unknown outcomes are never evicted to admit more writes. A full
ledger returns `503 idempotency_capacity` before dispatch. Successful retries
after retention expires can send again. Records do not survive restart; the
instance header protects against blindly reusing an old request. This is bounded
duplicate suppression, not an exactly-once delivery guarantee.

## Errors and limits

```json
{
  "error": {
    "code": "workspace_unavailable",
    "message": "Workspace is not ready",
    "retryable": true
  },
  "meta": {"request_id": "example-request-id", "source": "local"}
}
```

| Status | Meaning |
|---|---|
| `400` | Invalid input, JSON, cursor, instance header, or idempotency key. |
| `403` | Host/Origin rejected or Slack access refused. |
| `404` | Unknown route/workspace or missing/inaccessible resource. |
| `405` / `415` | Unsupported method / expected JSON content type. |
| `409` | Instance/key conflict, operation pending, or uncertain write outcome. |
| `413` | Body exceeds 256 KiB. |
| `429` | Local concurrency limit or upstream rate limit; includes `Retry-After`. |
| `502` | Slack refused or failed a read. |
| `503` | Workspace unavailable, Slack sign-in needed, shutdown, or ledger full. |
| `504` | Read timed out or was canceled. |

Workspace sign-in failures use `503 workspace_auth_required`; there is no local
API authentication challenge. Errors do not expose raw upstream bodies. Responses
use `Cache-Control: no-store`.

Message text is limited to 32 KiB of UTF-8, queries to 4 KiB. Writes require one
JSON object with no unknown or duplicate keys; nesting is limited to 64 levels.
Backend requests have eight global and two per-workspace concurrency slots; there
is no unbounded queue. Operation deadlines are 30 seconds. Headers, body reads,
responses and idle connections have bounded timeouts.

## Implementation design

`internal/localapi` owns HTTP DTOs, validation, signed cursors, idempotency,
admission limits, socket discovery and lifecycle. A narrow context-aware `Backend`
interface separates it from Slack and the TUI. `cmd/slk/local_api.go` implements
that interface using `workspaceRouter.ByID(team)` and an independent synchronized
registry populated at workspace connection boundaries. It never reads
`WorkspaceContext.Channels`, `FinderItems`, or `App` state from HTTP goroutines.

`internal/slack/pages.go` supplies context-aware single-page methods, preserving
Slack cursors and completeness metadata. Search's existing first-page helper
now delegates to the shared page method. Message/reply sends use the existing
conversion and transport, updated to honor cancellation through
`PostMessageContext`. No second Slack session or raw Slack method proxy is added.
The UI and bubbles retain their I/O boundary.

Startup binding is opt-in in `cmd/slk/main.go`. On shutdown the API stops accepting
requests and cancels operations before workspace/cache teardown, with a bounded
five-second handler drain. Cleanup removes only this instance's artifacts under
the process lock. Permission, symlink and live-listener checks protect startup;
no credential files are generated.

API reads do not update SQLite or the TUI. API sends return Slack's acknowledgement
and rely on normal WebSocket events for cache/UI reconciliation. They do not
inject composer `MessageSentMsg`/`ThreadReplySentMsg` events or consume drafts.
The display may lag the HTTP response during a WebSocket outage. The existing
UI's in-flight composer echo suppression can also defer a simultaneous external
self-authored message in that same channel; a subsequent history reload repairs
it. API acknowledgement and history remain authoritative for callers.

Tests cover real temporary Unix sockets, discovery/permissions, conflicting and
stale listeners, shutdown/cancellation, token-free reads/writes, pagination/cursor
scope, request validation, workspace routing during UI switches, concurrent
idempotency, uncertain outcomes, capacity and rate limits. Fake Slack HTTP servers
verify the upstream request shapes and that contexts reach the transport.

Future headless mode should reuse this backend after extracting the shared session
lifecycle. Windows transport, TCP, event streaming, UI navigation, files, reactions,
message edits/deletes, slash commands and attachment actions require separate
extensions. No background API maintenance or external service is needed for V1.
