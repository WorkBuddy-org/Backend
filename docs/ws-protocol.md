# WorkBuddy WebSocket protocol

The real-time contract for the frontend, written from `internal/ws/*.go`,
`internal/task/timer.go`, `internal/task/extension.go`,
`internal/chat/service.go` and their tests. REST endpoints are in
[api.md](api.md).

**The socket is receive-only for domain data.** Every command (join, leave,
start a timer, extend, send a chat message, …) goes through REST. The socket
delivers the resulting events. The only message a client may send is an
optional `ping`.

- [1. Connecting](#1-connecting)
- [2. Envelope](#2-envelope)
- [3. Client → server messages and heartbeat](#3-client--server-messages-and-heartbeat)
- [4. Presence rules](#4-presence-rules)
- [5. Events](#5-events)
- [6. Close codes](#6-close-codes)
- [7. Reconnecting and recovering state](#7-reconnecting-and-recovering-state)
- [8. Deployment notes](#8-deployment-notes)

---

## 1. Connecting

```
GET /rooms/{room_id}/ws
```

| Environment | URL |
|---|---|
| Local | `ws://localhost:8080/rooms/{room_id}/ws` |
| Production | `wss://api.<domain>/rooms/{room_id}/ws` |

**Auth.** The handshake reads the `access_token` cookie that login set (see
[api.md](api.md#authentication)). Browsers send it on the upgrade request
automatically, so `new WebSocket(url)` needs nothing extra. Fallback for
non-browser clients: the same JWT as `?access_token={access_token}`. When both
are present the cookie wins.

**Join first.** The user needs an open participation in the room
(`POST /rooms/{room_id}/join`), or the handshake is rejected with `403`.

**Several connections.** One user may have several sockets to the same room
(several tabs); see [presence rules](#4-presence-rules).

### Handshake checks (in this order)

A rejected handshake is a plain HTTP response in the
[error format](api.md#errors):

| Check | Failure |
|---|---|
| `room_id` is a UUID | `400 VALIDATION_ERROR`, `{"id": "INVALID_FORMAT"}` |
| `Origin` allowed (see below) | `403 FORBIDDEN` ("origin not allowed") |
| A token is present, well-formed and correctly signed | `401 UNAUTHORIZED` ("invalid access token") |
| The token is not expired | `401 TOKEN_EXPIRED` ("access token expired") |
| Open participation in the room (a missing room has none) | `403 FORBIDDEN` ("join the room before connecting") |
| Presence store reachable | the upgrade succeeds, then the server closes with `1011` |

Browsers do **not** expose the HTTP status of a failed handshake: the
`WebSocket` fires `error` and then `close` with code `1006`, without `open`.
In that case check over REST before retrying: `POST /auth/refresh`, then
`POST /rooms/{room_id}/join` (idempotent).

### Allowed origins

`WS_ALLOWED_ORIGINS` on the server: a comma-separated list of exact origins,
e.g. `https://app.example.com,http://localhost:3000`.

- Matching is exact on `scheme://host[:port]`, case-insensitive, trailing `/`
  ignored.
- Requests **without** `Origin` (native clients, scripts) are allowed.
- An empty list accepts any origin when `ENV=development` and rejects every
  browser origin otherwise.

This allow-list is the WebSocket's CSRF protection; the REST
[CSRF rule](api.md#csrf-rule) doesn't apply to it.

### Token lifetime

The token is checked only during the handshake. An open socket is **not**
closed when the access token expires; a reconnect needs a valid token, so
refresh first if the reconnect gets `1006`. With the query-string fallback
the token can end up in proxy logs, which is one more reason browsers should
rely on the cookie.

---

## 2. Envelope

Every server message is one JSON text frame:

```json
{
  "type": "user_joined",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:34:56.789Z",
  "data": { "user_id": "b7e6d5c4-3a2b-4c1d-8e9f-a0b1c2d3e4f5", "name": "Bob" }
}
```

| Field | Type | Notes |
|---|---|---|
| `type` | string | Event name, see [§5](#5-events) |
| `room_id` | UUID string | The room this socket is connected to |
| `ts` | string | When the server created the event: RFC 3339, UTC, always 3 fractional digits (`…:56.789Z`) |
| `data` | object | Event payload. Always an object (`{}` when empty), never `null` |

All timestamps inside `data` use the same format as `ts`.

---

## 3. Client → server messages and heartbeat

| Message | Effect |
|---|---|
| `{"type":"ping"}` | The server replies with a `pong` event on this socket |
| anything else | Ignored: invalid JSON, other types, binary frames. The connection stays open |

Messages over 4096 bytes close the socket with `1009`.

**Heartbeat.** The server sends a WebSocket **protocol** ping frame every
**25 s** and drops the connection when the pong doesn't arrive within
**10 s**. Browsers answer protocol pings automatically, so the client needs
no code for this. A vanished client leaves presence within about 35 s.

**`ping` / `pong` is not a domain event.** It exists only as an optional
application-level liveness check (for example to detect a half-open
connection sooner). It changes no state, and `pong` is sent only to the
socket that pinged.

---

## 4. Presence rules

Presence is the set of users with **at least one open socket** to the room,
kept in Redis (`room:{room_id}:presence`). It is not the same as membership:
participants (`GET /rooms/{id}/participants`) may have no socket open.

- On connect, `presence_snapshot` is **always the first message** on the
  socket. Treat it as the full list and replace local presence, on every
  reconnect too.
- `user_joined` is broadcast when a user's **first** socket to the room
  opens. The joining socket doesn't get it (it gets the snapshot). A second
  tab sends nothing to others.
- `user_left` is broadcast when a user's **last** socket to the room closes
  (clean close, network drop, missed heartbeat), or once when the user leaves
  or is removed over REST. Closing one of several tabs sends nothing.
- Apply `user_joined` / `user_left` idempotently by `user_id`.
- `POST /rooms/{room_id}/leave` closes **all** of that user's sockets to the
  room with `1000` and broadcasts a single `user_left` (reason
  `left_voluntarily`) to the others. Their sockets to other rooms are not
  touched. A handshake that races with the leave is closed with `4001` right
  after the upgrade.

---

## 5. Events

| Type | Direction | Sent when |
|---|---|---|
| `presence_snapshot` | targeted: the connecting socket | Right after every successful connect, as the first message |
| `user_joined` | broadcast: the room, except the joining socket | A user's first socket to the room opened |
| `user_left` | broadcast: the room | A user's last socket closed, or they left or were removed |
| `timer_started` | broadcast: the room | `POST /tasks/{id}/start` succeeded |
| `timer_ended` | broadcast: the room | A task timer expired, or its task was completed while running or prompting |
| `task_extension_prompt` | targeted: the task owner's sockets in this room | The owner's timer expired (right after `timer_ended`) |
| `task_extended` | broadcast: the room | `POST /tasks/{id}/extend` succeeded |
| `kicked` | targeted: the removed user's sockets in this room | The server removed the user (declined or ignored the extension prompt) |
| `chat_message` | broadcast: the room, sender's sockets included | `POST /rooms/{id}/messages` succeeded |
| `pong` | targeted: the socket that pinged | The client sent `{"type":"ping"}` (not a domain event) |

"Broadcast" reaches every open socket of the room. Users without a socket
miss the event; see [§7](#7-reconnecting-and-recovering-state).

### `presence_snapshot`

```json
{
  "type": "presence_snapshot",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:34:56.789Z",
  "data": {
    "users": [
      { "user_id": "8d3c0f7a-1b2e-4c5d-9e8f-0a1b2c3d4e5f", "name": "Alice" },
      { "user_id": "b7e6d5c4-3a2b-4c1d-8e9f-a0b1c2d3e4f5", "name": "Bob" }
    ]
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `users` | array | Everyone with an open socket, the connecting user included. Sorted by `name`, then `user_id` |
| `users[].user_id` | UUID | |
| `users[].name` | string | `"Unknown"` if the user record no longer exists |

### `user_joined`

```json
{
  "type": "user_joined",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:35:10.002Z",
  "data": { "user_id": "b7e6d5c4-3a2b-4c1d-8e9f-a0b1c2d3e4f5", "name": "Bob" }
}
```

| Field | Type |
|---|---|
| `user_id` | UUID |
| `name` | string |

### `user_left`

```json
{
  "type": "user_left",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:40:00.513Z",
  "data": { "user_id": "b7e6d5c4-3a2b-4c1d-8e9f-a0b1c2d3e4f5", "reason": "disconnect" }
}
```

| Field | Type | Notes |
|---|---|---|
| `user_id` | UUID | |
| `reason` | string | See below |

| `reason` | Emitted | Meaning |
|---|---|---|
| `disconnect` | yes | The user's last socket to the room closed, for any reason. Their participation stays open |
| `left_voluntarily` | yes | `POST /rooms/{id}/leave` |
| `kicked_no_extension` | yes | Removed by the server: declined or ignored the extension prompt |
| `kicked` | no (reserved) | Any other server-side removal |
| `completed_no_extension` | no (reserved) | |

The values other than `disconnect` match the participation's `left_reason`
in REST.

### `timer_started`

```json
{
  "type": "timer_started",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:00:00.004Z",
  "data": {
    "task_id": "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d",
    "user_id": "8d3c0f7a-1b2e-4c5d-9e8f-0a1b2c3d4e5f",
    "started_at": "2026-10-20T13:00:00.000Z",
    "ends_at": "2026-10-20T13:25:00.000Z",
    "duration_minutes": 25,
    "server_time": "2026-10-20T13:00:00.004Z"
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `task_id` | UUID | |
| `user_id` | UUID | Task owner |
| `started_at` | timestamp | |
| `ends_at` | timestamp | |
| `duration_minutes` | int | 25, 50 or 90 |
| `server_time` | timestamp | Server clock when sent, for skew correction |

Timers are **per task**: each participant runs their own, and a user has at
most one `running`/`prompting` timer. There are **no per-second ticks**.
Count down locally from `ends_at`, corrected by
`offset = server_time − local time at receipt`. The timer keeps running when
the owner leaves the room or loses the socket. It stops only when it expires
or the task is completed.

### `timer_ended`

```json
{
  "type": "timer_ended",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:25:00.010Z",
  "data": {
    "task_id": "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d",
    "user_id": "8d3c0f7a-1b2e-4c5d-9e8f-0a1b2c3d4e5f",
    "reason": "expired",
    "server_time": "2026-10-20T13:25:00.010Z"
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `task_id` | UUID | |
| `user_id` | UUID | Task owner |
| `reason` | `"expired"` \| `"completed"` | `expired`: reached `ends_at`; the task's `timer_status` becomes `prompting`. `completed`: the task was completed via `PATCH /tasks/{id}`; `timer_status` becomes `ended` |
| `server_time` | timestamp | |

Expiry is found by a 1-second server poll, so the event can arrive up to
about 1 s after `ends_at`.

### `task_extension_prompt`

Sent only to the task owner, right after `timer_ended` with reason
`expired`. The owner answers over REST before `deadline`:

- `POST /tasks/{task_id}/extend` with `{"minutes": 15 | 30}`: the timer runs
  again (`task_extended` is broadcast).
- `POST /tasks/{task_id}/no-extend`: the timer ends and the user is removed
  from the room (`kicked`).
- No answer by `deadline` counts as `no-extend`.
- Completing the task during the prompt ends the timer without removing the
  user.

```json
{
  "type": "task_extension_prompt",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:25:00.020Z",
  "data": {
    "task_id": "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d",
    "deadline": "2026-10-20T13:26:00.010Z",
    "allowed_minutes": [15, 30],
    "extensions_left": 3,
    "server_time": "2026-10-20T13:25:00.020Z"
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `task_id` | UUID | |
| `deadline` | timestamp | When the prompt was sent + `EXTENSION_PROMPT_TIMEOUT` (60 s); same as the task's `prompt_deadline_at` |
| `allowed_minutes` | int[] | `[15, 30]`, or `[]` when no extensions are left |
| `extensions_left` | int | `EXTENSION_MAX_COUNT` (3) minus `extended_count` |
| `server_time` | timestamp | |

If the owner has no socket in the room, the prompt is not delivered, but the
deadline still applies. The task in REST shows `timer_status: "prompting"`
and `prompt_deadline_at`.

### `task_extended`

```json
{
  "type": "task_extended",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:25:20.311Z",
  "data": {
    "task_id": "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d",
    "user_id": "8d3c0f7a-1b2e-4c5d-9e8f-0a1b2c3d4e5f",
    "added_minutes": 15,
    "extended_count": 1,
    "extra_minutes_used": 15,
    "ends_at": "2026-10-20T13:40:20.300Z",
    "server_time": "2026-10-20T13:25:20.311Z"
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `task_id` | UUID | |
| `user_id` | UUID | Task owner |
| `added_minutes` | int | 15 or 30 |
| `extended_count` | int | Task total after this extension |
| `extra_minutes_used` | int | Task total after this extension |
| `ends_at` | timestamp | New end of the running timer; restart the countdown from it |
| `server_time` | timestamp | |

### `kicked`

Sent to the removed user's sockets in this room. Right after it the server
closes those sockets with `4001`, closes the participation with
`left_reason` = `data.reason`, and broadcasts `user_left` with the same
reason to the others.

```json
{
  "type": "kicked",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:26:00.050Z",
  "data": { "reason": "kicked_no_extension" }
}
```

| Field | Type | Notes |
|---|---|---|
| `reason` | `"kicked_no_extension"` \| `"kicked"` | Only `kicked_no_extension` is emitted today. Treat both the same |

### `chat_message`

Broadcast after `POST /rooms/{room_id}/messages`, to the sender's sockets as
well: render either the event or the REST response and de-duplicate by `id`.

```json
{
  "type": "chat_message",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:10:42.120Z",
  "data": {
    "id": "e1d2c3b4-a5f6-4e7d-8c9b-0a1f2e3d4c5b",
    "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
    "user_id": "8d3c0f7a-1b2e-4c5d-9e8f-0a1b2c3d4e5f",
    "user_name": "Alice",
    "text": "Anyone up for a 5 min break after this one?",
    "created_at": "2026-10-20T13:10:42.100Z"
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `room_id` | UUID | |
| `user_id` | UUID | Author |
| `user_name` | string | Author's name at send time. REST history shows the *current* name |
| `text` | string | 1–2000 characters |
| `created_at` | timestamp | Millisecond precision here; REST has microseconds. Compare by `id`, not by timestamp string |

### `pong`

```json
{ "type": "pong", "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c", "ts": "2026-10-20T12:36:00.000Z", "data": {} }
```

`data` is always `{}`.

---

## 6. Close codes

| Code | Sent by | Meaning | Client action |
|---|---|---|---|
| `1000` | either | Normal close; the server uses it after `POST /rooms/{id}/leave` ("left room") | None |
| `1001` | server | Server shutting down or restarting | Reconnect with backoff |
| `1006` | (browser) | No close frame: network drop, heartbeat timeout, or a rejected handshake ([§1](#1-connecting)) | Refresh / re-join over REST, reconnect with backoff |
| `1008` | server | Client too slow: more than 64 queued messages. A fully stuck socket may see `1006` instead | Reconnect and recover state ([§7](#7-reconnecting-and-recovering-state)) |
| `1009` | server | Client message over 4096 bytes | Fix the client |
| `1011` | server | Internal error (presence store unavailable) | Reconnect with backoff |
| `4001` | server | Removed from the room; follows `kicked`. Also when a handshake races with leaving | Don't reconnect unless the user joins again |

---

## 7. Reconnecting and recovering state

On `close` with any code other than `1000` and `4001`, reconnect with
exponential backoff (for example 1 s, 2 s, 4 s … capped at 30 s, with jitter).
If reconnecting fails with `1006`, call `POST /auth/refresh` first.

**Events are not replayed.** Whatever happened while the socket was down is
lost, so on every (re)connect:

1. Open the socket. Its first message, `presence_snapshot`, is the full
   presence; replace local presence with it.
2. Re-fetch room state over REST:

   | State | REST call |
   |---|---|
   | Room | `GET /rooms/{id}` |
   | Participants (membership) | `GET /rooms/{id}/participants` |
   | Own tasks and their timers (`timer_status`, `timer_ends_at`, `prompt_deadline_at`) | `GET /tasks/me` (filter by `room_id`) |
   | Chat messages | `GET /rooms/{id}/messages` (newest page), merged with what is shown, de-duplicated by `id` |

3. Apply events from then on. Buffer events that arrive while the REST calls
   are in flight and apply them afterwards. They are idempotent by
   `user_id` / `task_id` / message `id`.

A `prompting` task in `GET /tasks/me` means an extension prompt is pending
even if `task_extension_prompt` was missed: show the prompt with
`prompt_deadline_at` as the deadline.

There is no REST endpoint for **other participants'** tasks or timers. Their
running timers become visible again only with their next `timer_started` /
`task_extended` / `timer_ended` event.

---

## 8. Deployment notes

- **Single instance.** Connections live in an in-memory hub, and broadcasts
  reach only sockets on the same process. More than one API instance needs a
  shared fan-out (e.g. Redis pub/sub) first.
- On startup the server deletes every `room:*:presence` set, since no socket
  survives a restart. Clients reconnect and repopulate it.
- A reverse proxy must forward the `Upgrade` / `Connection` headers and keep
  idle connections open longer than 35 s.
