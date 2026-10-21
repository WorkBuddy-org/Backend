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
| `room_id` is a UUID | `400 {"error":"invalid room id"}` |
| `Origin` header allowed (see below) | `403 {"error":"origin not allowed"}` |
| `access_token` present, valid signature, not expired | `401 {"error":"missing or invalid access token"}` |
| User has an open participation in the room (room missing ⇒ none) | `403 {"error":"join the room before connecting"}` |
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
fresh access token. Do not reconnect after `4001` (removed from room, planned)
unless the user joins again via REST.

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
| `completed_no_extension` | planned | Task cycle ended and the user declined/ignored the extension prompt. |
| `kicked` | planned | Removed by the server/moderation. |

(Planned values match `room_participations.left_reason`.)

### `pong` — live, to the socket that sent `ping`

```json
{
  "type": "pong",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T12:36:00.000Z",
  "data": {}
}
```

### `timer_started` — planned, broadcast

A focus or break interval of the room's shared timer started.

```json
{
  "type": "timer_started",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:00:00.004Z",
  "data": {
    "session_id": "0c9b8a7f-6e5d-4c3b-2a19-08f7e6d5c4b3",
    "status": "focus",
    "started_at": "2026-10-20T13:00:00.000Z",
    "ends_at": "2026-10-20T13:25:00.000Z",
    "duration_seconds": 1500
  }
}
```

- `status`: `focus` | `break`. Compute the countdown locally from `ends_at`;
  there are no per-second ticks.

### `timer_ended` — planned, broadcast

```json
{
  "type": "timer_ended",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:25:00.010Z",
  "data": {
    "session_id": "0c9b8a7f-6e5d-4c3b-2a19-08f7e6d5c4b3",
    "status": "focus",
    "ended_at": "2026-10-20T13:25:00.000Z"
  }
}
```

- `status` is the interval that just ended. The next interval, if any,
  arrives as its own `timer_started`.

### `task_extension_prompt` — planned, **targeted** (only the task owner's sockets in this room)

Sent when the owner's task cycle ends and they must decide whether they need
more time. The answer is sent via REST (endpoint defined with the
task-extension feature); not answering by `respond_by` counts as declining.

```json
{
  "type": "task_extension_prompt",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:25:00.020Z",
  "data": {
    "task_id": "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d",
    "text": "Write the onboarding email",
    "extended_count": 0,
    "extra_minutes_used": 0,
    "respond_by": "2026-10-20T13:26:00.000Z"
  }
}
```

### `task_extended` — planned, broadcast

```json
{
  "type": "task_extended",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:25:20.311Z",
  "data": {
    "task_id": "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d",
    "user_id": "8d3c0f7a-1b2e-4c5d-9e8f-0a1b2c3d4e5f",
    "added_minutes": 10,
    "extended_count": 1,
    "extra_minutes_used": 10
  }
}
```

- `extended_count` / `extra_minutes_used` are the task's totals after this
  extension (columns of the same name on `tasks`).

### `kicked` — planned, **targeted** (only that user's sockets in this room)

The user was removed from the room. Right after this event the server closes
those sockets with code `4001`, and the rest of the room receives `user_left`
with the matching `reason`.

```json
{
  "type": "kicked",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:26:00.050Z",
  "data": { "reason": "completed_no_extension" }
}
```

- `reason`: `completed_no_extension` | `kicked`.

### `chat_message` — planned, broadcast (including the sender's sockets)

Sent after a message is created via REST. The sender gets it too, so render
from this event (or de-duplicate by `message_id`).

```json
{
  "type": "chat_message",
  "room_id": "3f2b8c1e-6a4d-4d5e-9b1a-2c7e8f9a0b1c",
  "ts": "2026-10-20T13:10:42.120Z",
  "data": {
    "message_id": "e1d2c3b4-a5f6-4e7d-8c9b-0a1f2e3d4c5b",
    "user_id": "8d3c0f7a-1b2e-4c5d-9e8f-0a1b2c3d4e5f",
    "name": "Alice",
    "text": "Anyone up for a 5 min break after this one?",
    "created_at": "2026-10-20T13:10:42.100Z"
  }
}
```

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
| `4001` | server | Removed from room — planned, follows `kicked`. | Do not reconnect unless the user joins again. |

---

## 7. Deployment notes / limits (MVP)

- **Single instance.** Connections are tracked in an in-memory hub;
  broadcasts reach only sockets on the same server process. Running more than
  one API instance requires a shared fan-out (e.g. Redis pub/sub) first.
- On startup the server deletes all `room:*:presence` sets, since no socket
  survives a restart (clients reconnect and re-populate presence).
- Behind a reverse proxy, WebSocket upgrade headers (`Upgrade`, `Connection`)
  must be forwarded and the proxy idle timeout must exceed 35 s.
