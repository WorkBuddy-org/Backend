# WorkBuddy WebSocket protocol

Real-time channel for a room. **Broadcast-only:** the server pushes events;
every client *command* (join, leave, send chat, extend task, …) goes through
the REST API. The only thing a client may send over the socket is a `ping`.

Status of each event is marked **live** (emitted today) or **planned** (shape
agreed, emitted once that feature ships). Clients must ignore unknown event
types and unknown fields; new fields may be added without notice.

---

## 1. Connecting

```
GET /rooms/{room_id}/ws?access_token={access_token}
```

- Scheme: `wss://` in production, `ws://` locally (`ws://localhost:8080/rooms/{room_id}/ws?access_token=…`).
- `access_token`: the same JWT access token used in `Authorization: Bearer …`
  for REST. It goes in the query string because browsers cannot set headers
  on a WebSocket handshake.
- **Join first.** The user must have an open participation in the room:
  call `POST /rooms/{room_id}/join` (REST) before connecting, otherwise the
  handshake is rejected with 403.
- One user may open several connections to the same room (several tabs);
  see presence rules in §4.

### Handshake checks (in this order)

| Check | Failure |
|---|---|
| `room_id` is a UUID | `400 VALIDATION_ERROR` ("invalid room id") |
| `Origin` header allowed (see below) | `403 FORBIDDEN` ("origin not allowed") |
| `access_token` present with a valid signature | `401 UNAUTHORIZED` ("invalid access token") |
| `access_token` not expired | `401 TOKEN_EXPIRED` ("access token expired") |
| User has an open participation in the room (room missing ⇒ none) | `403 FORBIDDEN` ("join the room before connecting") |
| Presence store reachable | upgrade succeeds, then the server closes with `1011` |

Browsers do **not** expose the HTTP status of a failed handshake: the
`WebSocket` fires `error` then `close` with code `1006` without ever firing
`open`. If that happens, check with REST before retrying: refresh the access
token (`POST /auth/refresh`) and make sure the user is still in the room
(`POST /rooms/{room_id}/join` is idempotent).

### Allowed origins

Configured on the server with `WS_ALLOWED_ORIGINS` — a comma-separated list
of exact origins, e.g. `https://app.workbuddy.example,http://localhost:3000`.

- Matching is exact on `scheme://host[:port]`, case-insensitive, trailing `/` ignored.
- Requests **without** an `Origin` header (native/mobile clients, scripts) are allowed.
- If the list is empty: any origin is accepted when `ENV=development`; in any
  other environment every browser origin is rejected.

### Token lifetime

The token is checked only during the handshake. An open socket is **not**
closed when the access token expires (15 min by default); a reconnect needs a
fresh token. Because the token is in the URL it can end up in proxy access
logs — never log full WebSocket URLs on the client side either.

---

## 2. Envelope

Every server message is one JSON text frame:

```json
{
  "type": "user_joined",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:34:56.789Z",
  "data": { }
}
```

| Field | Type | Notes |
|---|---|---|
| `type` | string | Event name, see §5. |
| `room_id` | UUID string | Room the socket is connected to. |
| `ts` | string | Server time the event was created, RFC 3339, UTC, millisecond precision. |
| `data` | object | Event payload; always an object (`{}` when empty), never `null`. |

---

## 3. Client → server messages

| Message | Effect |
|---|---|
| `{"type":"ping"}` | Server replies with a `pong` event. Optional — use it if you want an application-level liveness check. |
| anything else | Ignored (invalid JSON, other types, binary frames). The connection stays open. |

Max client message size: 4096 bytes; larger frames close the socket with `1009`.

---

## 4. Presence rules

- Presence is the set of users with **at least one open WebSocket** to the
  room. It is stored in Redis as the set `room:{room_id}:presence` (user ids).
- On connect the client always receives `presence_snapshot` **as the first
  message** on that socket. Treat it as the full, authoritative list and
  replace any local state (also on every reconnect).
- `user_joined` is broadcast when a user's **first** connection to the room
  opens; it is not sent to the connection that just joined (it gets the
  snapshot instead). Opening a second tab sends nothing to others.
- `user_left` is broadcast when a user's **last** connection to the room
  closes — clean close, network drop, or missed heartbeats. Closing one of
  several tabs sends nothing.
- Apply `user_joined` / `user_left` idempotently keyed by `user_id` (adding a
  user already in the list or removing one that is absent is a no-op).
- `POST /rooms/{room_id}/leave` closes **all** of that user's sockets to the
  room with `1000` ("left room"), removes them from presence, and broadcasts a
  single `user_left` with `reason: "left_voluntarily"` to the rest of the room
  (the leaving user's sockets do not receive it). The user's sockets to other
  rooms are untouched. A handshake that races with the leave is closed with
  `4001` right after the upgrade.

### Heartbeat

The server sends a WebSocket protocol ping every **25 s** and drops the
connection if the pong does not arrive within **10 s** (browsers answer
protocol pings automatically). A vanished client is therefore removed from
presence within ~35 s.

### Reconnecting

On `close` with any code other than `1000` and `4001`, reconnect with
exponential backoff (e.g. 1 s, 2 s, 4 s … capped at 30 s, with jitter) and a
fresh access token. Do not reconnect after `4001` (removed from room) unless
the user joins again via REST.

---

## 5. Events

### `presence_snapshot` — live, to the connecting socket only

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

- Includes the connecting user. Sorted by `name`, then `user_id`.
- `name` is `"Unknown"` if the user record no longer exists.

### `user_joined` — live, broadcast (except the joining socket)

```json
{
  "type": "user_joined",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:35:10.002Z",
  "data": { "user_id": "b7e6d5c4-3a2b-4c1d-8e9f-a0b1c2d3e4f5", "name": "Bob" }
}
```

### `user_left` — live, broadcast

```json
{
  "type": "user_left",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:40:00.513Z",
  "data": { "user_id": "b7e6d5c4-3a2b-4c1d-8e9f-a0b1c2d3e4f5", "reason": "disconnect" }
}
```

`reason`:

| Value | Status | Meaning |
|---|---|---|
| `disconnect` | live | The user's last socket to the room closed (any cause). |
| `left_voluntarily` | live | Left via `POST /rooms/{id}/leave`. |
| `completed_no_extension` | planned | Reserved; not emitted today (declining an extension uses `kicked_no_extension`). |
| `kicked_no_extension` | live | Removed by the server: declined or ignored the task extension prompt. |
| `kicked` | reserved | Any other removal by the server (e.g. a future admin kick); not emitted today. |

(Values other than `disconnect` match `room_participations.left_reason`.)

### `pong` — live, to the socket that sent `ping`

```json
{
  "type": "pong",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:36:00.000Z",
  "data": {}
}
```

### `timer_started` — live, broadcast

A participant started the timer of one of their tasks via
`POST /tasks/{task_id}/start` with `{"duration_minutes": 25 | 50 | 90}`.
Timers are **per task** (each participant runs their own); a user has at most
one running/prompting timer at a time.

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

- There are **no per-second ticks**. Count down locally from `ends_at`,
  corrected for clock skew: `remaining = ends_at - (Date.now() + (server_time - receivedAt))`,
  i.e. compute `offset = server_time - local time at receipt` once per event.
- The countdown keeps running if the user leaves the room or loses the
  socket; the timer only stops when it expires or the task is completed.
- Current timer state is also on the task in REST (`timer_status`,
  `timer_duration_minutes`, `timer_started_at`, `timer_ends_at`), so a client
  that (re)connects mid-timer reads it from `GET /tasks/me`.

### `timer_ended` — live, broadcast

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

- `reason`: `expired` (reached `ends_at`; the task's `timer_status` becomes
  `prompting`) | `completed` (the task was completed via
  `PATCH /tasks/{id}` while its timer ran; `timer_status` becomes `ended`).
- Expiry is detected by a 1 s server poll, so the event may arrive up to ~1 s
  after `ends_at`.

### `task_extension_prompt` — live, **targeted** (only the task owner's sockets in this room)

Sent when the owner's task timer expires (right after the room's
`timer_ended` with `reason: "expired"`). The owner must answer via REST
before `deadline`:

- `POST /tasks/{task_id}/extend` with `{"minutes": 15 | 30}` — the timer runs
  again for that many minutes from now (`task_extended` is broadcast).
- `POST /tasks/{task_id}/no-extend` (no body) — the timer ends and the user
  is removed from the room (see `kicked`).
- No answer by `deadline` (default 60 s, `EXTENSION_PROMPT_TIMEOUT`) is the
  same as `no-extend`.
- Completing the task (`PATCH /tasks/{id}` with `{"completed": true}`) during
  the prompt also ends the timer, without removing the user.

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

- `extensions_left`: how many more times this task may be extended (at most
  `EXTENSION_MAX_COUNT`, default 3, per task). When it is `0`,
  `allowed_minutes` is `[]`; the only options are `no-extend`, completing the
  task, or letting the deadline pass.
- If the owner has no socket open in the room the prompt is not delivered;
  the deadline still applies. The current state is on the task in REST:
  `timer_status: "prompting"` and `prompt_deadline_at`.
- REST answers: `200` with the task; `409` if the timer is not prompting,
  the deadline has passed / the prompt was already answered, or the limit is
  reached; `400` for other `minutes`; `403` for someone else's task.

### `task_extended` — live, broadcast

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

- `extended_count` / `extra_minutes_used` are the task's totals after this
  extension (columns of the same name on `tasks`).
- `ends_at` is the new end of the running timer; restart the local countdown
  from it, exactly as for `timer_started`.

### `kicked` — live, **targeted** (only that user's sockets in this room)

The user was removed from the room. Right after this event the server closes
those sockets with code `4001`, the user's participation is closed with
`left_reason` equal to `data.reason`, and the rest of the room receives
`user_left` with the same `reason`. `data.reason` is `kicked_no_extension`
when they declined or did not answer the extension prompt (the only case
today), or `kicked` for any other server-side removal. Treat both the same.

```json
{
  "type": "kicked",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:26:00.050Z",
  "data": { "reason": "kicked_no_extension" }
}
```

### `chat_message` — live, broadcast (including the sender's sockets)

Sent after a message is created with `POST /rooms/{room_id}/messages`. The
sender gets it too, so either render from this event or render the REST
response and de-duplicate by `id`. `data` has the same fields as a message in
the REST API (see "Chat REST API" below); `user_name` is the author's name at
send time.

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

#### Chat REST API

Text only (no files, images, edits, deletes or delivery/read statuses in the
MVP). Both endpoints require `Authorization: Bearer …` and an **open
participation** in the room (joined and not left/kicked); otherwise `403
FORBIDDEN` ("join the room before using its chat"). A room that does not exist
has no participants, so it also gets 403.

**Send:** `POST /rooms/{room_id}/messages` with `{"text": "…"}`.

- `text` is trimmed; after trimming it must be 1–2000 characters (Unicode
  code points, not bytes). Empty → `400 VALIDATION_ERROR` (`details.fields` =
  `{"text": "REQUIRED"}`), longer → `400 VALIDATION_ERROR`
  (`{"text": "TOO_LONG"}`). Unknown fields → `400 VALIDATION_ERROR`
  (`{"<name>": "UNKNOWN_FIELD"}`).
- `201` with the stored message:
  `{"id", "room_id", "user_id", "user_name", "text", "created_at"}`
  (`created_at` is RFC 3339 with the database's precision).
- Rate limit: by default 10 messages per user per 10 seconds, counted across
  all rooms (`CHAT_RATE_LIMIT` / `CHAT_RATE_WINDOW`). Over the limit →
  `429 RATE_LIMITED` ("too many messages, slow down") with a `Retry-After` header
  (whole seconds until the window resets). The message isn't stored or
  broadcast. Invalid messages get their `400` first and don't count.

**History:** `GET /rooms/{room_id}/messages?before={message_id}&limit={n}`.

- Newest first, ordered by `(created_at, id)`. Response:
  `{"items": [message, …], "next_cursor": "<message id>" | null}`. `items` is
  `[]` when empty, never `null`.
- `limit`: default 50, values above 100 are capped to 100; `0`, negative or
  non-integer → `400 VALIDATION_ERROR` (`{"limit": "INVALID_VALUE"}`).
- `before`: omit for the newest page; to load older messages pass the
  previous response's `next_cursor`. `next_cursor` is `null` when there are no
  older messages. A `before` that is not the id of a message in this room →
  `400 VALIDATION_ERROR` (`{"before": "INVALID_VALUE"}`).
- Messages that arrive while paging do not shift the cursor; merge them from
  `chat_message` events (de-duplicate by `id`).

---

## 6. Close codes

| Code | Sent by | Meaning | Client action |
|---|---|---|---|
| `1000` | either | Normal close (e.g. client closed the tab/socket). | None. |
| `1001` | server | Server shutting down / restarting. | Reconnect with backoff. |
| `1006` | (browser) | No close frame: network drop, heartbeat timeout, or rejected handshake (§1). | Check token/participation, reconnect with backoff. |
| `1008` | server | Client too slow — its outgoing buffer (64 messages) overflowed. If the socket is fully stuck the browser may see `1006` instead. | Reconnect; the new snapshot resyncs state. |
| `1009` | server | Client message larger than 4096 bytes. | Fix the client. |
| `1011` | server | Internal error (presence store unavailable). | Reconnect with backoff. |
| `4001` | server | Removed from room; follows `kicked`. Also used if a handshake races with leaving the room. | Do not reconnect unless the user joins again. |

---

## 7. Deployment notes / limits (MVP)

- **Single instance.** Connections are tracked in an in-memory hub;
  broadcasts reach only sockets on the same server process. Running more than
  one API instance requires a shared fan-out (e.g. Redis pub/sub) first.
- On startup the server deletes all `room:*:presence` sets, since no socket
  survives a restart (clients reconnect and re-populate presence).
- Behind a reverse proxy, WebSocket upgrade headers (`Upgrade`, `Connection`)
  must be forwarded and the proxy idle timeout must exceed 35 s.
