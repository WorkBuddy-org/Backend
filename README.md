# WorkBuddy Backend

Go backend for WorkBuddy — shared focus rooms with live timers, tasks, and chat.

## Getting Started

```
make docker-up      # Postgres + Redis
make migrate-up      # apply migrations (seeds fixed rooms)
make run             # start the API server
```

See `.env.example` for required configuration.

## Redis Key Structure

Live timer countdown state is **ephemeral** and lives only in Redis — it is never
persisted to Postgres. Postgres only stores the durable `room_sessions` record
(start/end/status) once a session begins or ends; the second-by-second countdown
that clients poll/subscribe to is Redis-only.

### `room:{room_id}:timer` — hash

Holds the current timer state for a room. Absence of this key means the room has
no active timer.

| Field              | Type              | Description                                              |
|--------------------|-------------------|------------------------------------------------------------|
| `session_id`        | UUID string        | FK to the `room_sessions` row this timer belongs to        |
| `status`             | `focus` \| `break`  | Mirrors `room_sessions.status`                              |
| `started_at`        | RFC3339 timestamp  | When the current countdown started                          |
| `duration_seconds`  | integer             | Total length of this focus/break interval                   |
| `ends_at`            | RFC3339 timestamp  | `started_at` + `duration_seconds`, used for countdown math  |

- Set with a TTL slightly longer than `duration_seconds` (e.g. `+30s` grace) so a
  crashed process can't leave a stale timer running forever.
- On explicit stop/skip, the key is deleted rather than waiting for TTL expiry.
- When a timer naturally completes, the server writes `ended_at`/`status` to the
  corresponding `room_sessions` row in Postgres and removes (or replaces, for the
  next interval) the Redis key.

### `room:{room_id}:timer:subscribers` — set (optional, for presence)

Reserved for tracking which connections are currently watching a room's timer
(e.g. for websocket fan-out); not required for MVP polling-based clients.
