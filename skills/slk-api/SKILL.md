---
name: slk-api
description: Read Slack conversations, search messages, open DMs, and post messages through a running slk session's local Unix-socket API. Use for operating Slack through slk, rather than implementing or debugging the API itself.
---

# slk local API

Use HTTP/JSON over the running slk TUI's Unix socket on macOS or Linux. There
are no API auth tokens or separate write settings. Socket filesystem permissions
control access; the connected Slack account's permissions apply.

## Discover the session

Honor an explicitly supplied socket path. Otherwise inspect
`${XDG_CONFIG_HOME:-$HOME/.config}/slk/config.toml` for
`[local_api].socket_path`. For the default location, check
`$XDG_RUNTIME_DIR/slk/api.json` when `XDG_RUNTIME_DIR` is an absolute directory
owned by the current user with mode `0700`; otherwise check
`${TMPDIR:-/tmp}/slk-$(id -u)/api.json` (resolve symlinks in the temp path).
Read `socket_path` and `instance_id` from that discovery file. Do not recursively
scan other users' directories or read Slack credentials.

If no session is available, report that slk must be running with
`[local_api] enabled = true`. Do not silently change configuration, restart the
user's TUI, or invent a headless server command.

Set `SLK_API_SOCKET` to the discovered socket. Every URL must use `slk.local`
as its host. Use a Unix-socket-capable client such as curl; do not connect via
TCP or include an `Origin` header.

```sh
curl --silent --show-error --fail-with-body --max-time 45 \
  --unix-socket "$SLK_API_SOCKET" http://slk.local/v1/status
curl --silent --show-error --fail-with-body --max-time 45 \
  --unix-socket "$SLK_API_SOCKET" http://slk.local/v1/workspaces
```

Choose the workspace matching the user's request. Every operation below requires
its explicit team ID, independently of TUI focus. Resolve conversation/user IDs
from available results or user-supplied IDs; do not guess IDs from names. If the
destination remains ambiguous, clarify it before sending.

## Read and search

Use `/v1/workspaces/{team}` as the base path:

| GET suffix | Use |
|---|---|
| `/conversations` | Joined, non-archived conversations; `limit`, `cursor`. |
| `/conversations/{channel}` | Conversation metadata. |
| `/conversations/{channel}/messages` | Channel history, newest first; `limit`, `cursor`, exclusive `oldest`/`latest`. |
| `/conversations/{channel}/threads/{thread_ts}/replies` | Replies oldest first; `limit`, `cursor`; parent in `meta.parent`. |
| `/search` | Required `q` in Slack search syntax; `limit`, `cursor`. |
| `/users/{user}` | Minimal user profile. |

Use `--get --data-urlencode 'q=…'` for search terms and URL-encode cursors.
Responses contain `data` and `meta`. Pagination defaults to 50 items, maximum
100. Continue until `meta.next_cursor` is null, retaining the same path, query
and page size. An empty page with a cursor is not the end. On `invalid_cursor`,
restart the query and deduplicate by team/channel/ts. If `meta.truncated` is true,
state the incomplete coverage. Search is indexed and is not a complete archive.

Keep timestamps as strings. Message `text` is Slack mrkdwn; `has_blocks` and
`has_attachments` can indicate body content this API does not expose. Do not
present empty text as proof that a bot post has no content. Reads do not mark
messages read or change TUI navigation/drafts.

## Open conversations and post

Perform writes within the user's authorized task. Do not post a test message
just to check connectivity; status and read routes suffice.

POST `/conversations` with `{"user_ids":["U123"]}` to open/find a DM, or up to
eight distinct IDs for a group DM. The response contains `channel_id` and
`already_open`; opening does not navigate the TUI.

POST `/conversations/{channel}/messages` with `text`, optional `thread_ts`, and
optional `broadcast`. Text uses slk's CommonMark-like compose conversion, not raw
Slack mrkdwn. Mentions require explicit Slack ID markup. A leading slash sends
ordinary text; it does not execute a slash command. Broadcast defaults to false
and requires a thread; enable it only when the user intends a channel broadcast.

Obtain the current `instance_id` from discovery or `/v1/status` before a new
operation. Generate a fresh random UUID as its idempotency key. Keep the instance,
key, destination and exact body available for reconciliation or retries. Use a
JSON file or structured client encoding for user text rather than interpolating
it into shell commands.

```sh
curl --silent --show-error --fail-with-body --max-time 45 \
  --unix-socket "$SLK_API_SOCKET" \
  -H "X-Slk-Instance: $SLK_API_INSTANCE" \
  -H "Idempotency-Key: $SLK_API_IDEMPOTENCY_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @"$SLK_API_BODY_FILE" \
  "http://slk.local/v1/workspaces/$SLK_TEAM_ID/conversations/$SLK_CHANNEL_ID/messages"
```

Message sends return `201` with the acknowledged Slack timestamp and converted
text; DM opening returns `200`. Report the acknowledged target/timestamp. The
TUI may update later through its WebSocket; do not resend because display lags.

## Handle retries without duplicate sends

Inspect the JSON `error.code`, `retryable`, and `Retry-After`; retain error bodies.
Do not add curl's automatic retry flags to writes.

- For `local_busy`, `operation_in_progress`, or `slack_rate_limited`, honor
  `Retry-After` and retry with the same instance, key, path and body. Bound retries
  by the task's deadline; report a continuing block instead of looping forever.
- For `outcome_unknown` or a lost/timed-out write response, stop automatic retries
  and check history for the intended operation. Do not issue a new key merely
  because no acknowledgement arrived; report uncertainty if unresolved.
- For `instance_mismatch`, reconcile the old operation before adopting the new
  instance ID. Deduplication is in memory and does not survive restart.
- For `idempotency_conflict`, do not change the key to force the same uncertain
  operation through. Check the retained operation/body first.
- For `workspace_auth_required`, report that the workspace needs sign-in through
  slk. Do not search for a nonexistent local API token.

Completed outcomes are retained for 24 hours; pending/unknown records are not
evicted to admit writes. The ledger has a 10,000-entry cap. Retrying beyond
retention can duplicate an earlier successful send.

For additional schemas or limits in this checkout, consult
[the API guide](../../wiki/Local-API.md) and
[the OpenAPI contract](../../wiki/Local-API.openapi.json). Edits/deletes, reactions,
files, slash commands, event streaming and UI control are not V1 endpoints.
