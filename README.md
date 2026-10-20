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

## Room Presence (Redis-only, no migration)

Room timers run as one shared server-side cycle per room, not per user. When a
task cycle ends for a participant, the server asks whether they need extra
time; declining removes them from the session (they can rejoin later and
start a new task). Answering that "who's here right now" question needs a
fast, ephemeral set that the WebSocket layer can update on every
connect/disconnect — that's what this key is for.

### `room:{room_id}:presence` — set

Holds the `user_id`s with at least one open WebSocket to the room
(`GET /rooms/{id}/ws`, see [docs/ws-protocol.md](docs/ws-protocol.md)).

- Added on a user's first WebSocket connect to the room (REST join is a
  prerequisite for connecting, but does not touch this set by itself).
- Removed when the user's last connection to the room closes — including
  abnormal disconnects detected by the 25 s server ping. Later phases also
  remove users on kick / task-cycle timeout with no extension.
- No TTL on the set itself; membership is maintained by the connection
  lifecycle. The connection hub is in memory (single instance for the MVP),
  so the server deletes all `room:*:presence` sets on startup to drop
  entries left behind by a crash.
- Read by anything that needs "who's in this room right now" — live room UI,
  matching a chat message to active participants, etc.

### Presence vs. `room_participations` — Redis is *now*, Postgres is *history*

This key is **not** a replacement for the `room_participations` table
(migration `00009_create_room_participations.sql`). They answer different
questions and neither is a cache of the other:

| | `room:{room_id}:presence` (Redis) | `room_participations` (Postgres) |
|---|---|---|
| Answers | "Who is in the room right now?" | "Who was in the room, when, and for how long?" |
| Lifetime | Ephemeral — reflects live connection state | Durable — one row per join, with `joined_at`/`left_at` |
| Written on | Every join/leave/disconnect | Join (insert) and leave (update `left_at` + `left_reason`) |
| Used for | Live presence indicators, real-time room state | Stats, presence history, distinguishing why someone left (`completed_no_extension` / `kicked` / `left_voluntarily`) |

In practice: a join opens both a Redis set entry and a `room_participations`
row; a leave closes both. If the two ever disagree (e.g. after a crash), the
Redis set is allowed to be wrong until the next join/leave reconciles it —
Postgres is the source of truth for history, Redis is the source of truth for
"right now."

## Security Scanning

CI runs two security scanners alongside `go vet` and `golangci-lint`. Both
currently run with `continue-on-error: true` — they report findings in the
CI logs but don't fail the build. This is intentional for now: get visibility
into what they flag before deciding what should hard-block merges.

### govulncheck

The official Go vulnerability scanner. It checks your dependencies and the
standard library against the [Go vulnerability database](https://vuln.go.dev),
and — unlike a plain dependency-version check — traces whether your code
actually *calls* the vulnerable code path, so it doesn't flag CVEs in code
you never reach.

Run it locally:

```
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0
govulncheck ./...
```

### gosec

Static analysis for common Go security mistakes: hardcoded credentials, SQL
injection risk, weak/unsafe crypto, unsafe cookie flags, and similar patterns
— things `go vet`/`golangci-lint` don't check for.

Run it locally:

```
go install github.com/securego/gosec/v2/cmd/gosec@v2.28.0
gosec ./...
```
