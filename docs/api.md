# WorkBuddy REST API

Reference for frontend developers. Everything here is derived from the
handlers in `internal/*/handler.go` and the router in
`internal/httpapi/router.go`. Real-time events (timers, presence, chat
broadcasts, kicks) are described in [ws-protocol.md](ws-protocol.md).

## Conventions

**Base URL.** The API is served from the server root, with no `/api` or
version prefix: `http://localhost:8080` locally (port from `PORT`). Paths
below are relative to that root.

**Content type.** Request and response bodies are JSON
(`Content-Type: application/json`). Request bodies are strict:

- an **unknown field** is rejected with `400 invalid request body`;
- bodies are read up to 1 MiB;
- a malformed or missing JSON body on an endpoint that expects one is a
  `400 invalid request body`.

**IDs** are UUID strings. A path id that isn't a valid UUID returns `400`
(`invalid room id`, `invalid task id`).

**Timestamps** are RFC 3339 strings in UTC with fractional seconds, e.g.
`"2026-10-22T14:03:11.482913Z"`.

**Optional fields.** Fields marked *omitted when empty* are left out of the
JSON instead of being `null`.

### Error format

Every error response, on every endpoint, has the same body:

```json
{ "error": "human-readable message" }
```

The message is safe to log, but don't match on it in code. Use the HTTP status.
Unexpected failures return `500` with a generic message.

Statuses used across the API:

| Status | Meaning |
|---|---|
| `400` | Invalid path id, query parameter or body |
| `401` | Missing/invalid `Authorization` header, invalid or expired access token, bad credentials |
| `403` | Authenticated, but not allowed (not the task owner, not in the room) |
| `404` | Resource not found |
| `409` | Conflict with the current state (timer already running, prompt expired, …) |
| `429` | Rate limited (registration, login attempts, chat messages). Always has a `Retry-After` header: whole seconds to wait. |
| `502` | OAuth provider failed |
| `503` | A dependency or feature is not configured/available |

## Authentication

Protected endpoints need an access token in the `Authorization` header:

```
Authorization: Bearer <access_token>
```

Without a valid header the response is `401` with
`missing or invalid authorization header` or `invalid access token` (also
when it's expired).

The WebSocket endpoint can't use headers. It takes the token as
`?access_token=` instead (see [ws-protocol.md](ws-protocol.md)).

### Token pair

Register, login, refresh and the OAuth callback (JSON mode) return:

```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIs...",
  "refresh_token": "Zk3p...opaque...",
  "expires_in": 900,
  "token_type": "Bearer"
}
```

- `access_token`: a JWT valid for `JWT_ACCESS_TTL` (default 15 min).
  `expires_in` is its lifetime in seconds.
- `refresh_token`: an opaque string valid for `JWT_REFRESH_TTL` (default 30
  days). Store it securely.

### Refresh flow

1. Call protected endpoints with the access token.
2. On `401` (or shortly before `expires_in` runs out), call
   `POST /auth/refresh` with the refresh token.
3. The response is a **new pair**. The old refresh token is revoked
   (rotation), so always replace both tokens. Reusing an old refresh token
   gives `401 token revoked`.
4. If refresh fails with `401`, send the user to login.

---

### `POST /auth/register`

Create an email + password account. **Auth:** none.

Request:

| Field | Type | Rules |
|---|---|---|
| `email` | string | Trimmed and lower-cased. Must contain `@` with text on both sides and no whitespace. |
| `password` | string | At least 8 bytes. |
| `name` | string | Display name. Not validated (may be empty). |

`201 Created` → [token pair](#token-pair).

Errors: `400 invalid email address`, `400 password must be at least 8 characters`,
`400 invalid request body`, `409 email already registered with another provider`
(the email is taken by any account),
`429 too many registration attempts, try again later`: more than 5 requests
from the same client IP within 10 minutes (`REGISTER_RATE_LIMIT` /
`REGISTER_RATE_WINDOW`; the window starts at the first request). Every
request counts, including failed ones. The `Retry-After` header gives the
seconds until the window resets. If the server's Redis is unavailable,
requests aren't limited. The client IP is the connection address, or the
first `X-Forwarded-For` value when the server runs behind a proxy with
`TRUSTED_PROXY_HEADER` set.

### `POST /auth/login`

**Auth:** none.

Request: `{ "email": "string", "password": "string" }`

`200 OK` → [token pair](#token-pair).

Errors:
- `401 invalid email or password`: unknown email, OAuth-only account or wrong
  password (deliberately indistinguishable).
- `429 too many login attempts, try again later`: more than 5 attempts for
  the same email within 15 minutes (the window starts at the first attempt).
  The `Retry-After` header gives the seconds until the window resets. A
  successful login resets the counter. If the server's Redis is unavailable,
  attempts aren't limited (same as chat).
- `400 invalid request body`.

### `POST /auth/refresh`

**Auth:** none. Request: `{ "refresh_token": "string" }`

`200 OK` → a new [token pair](#token-pair). The submitted refresh token is revoked.

Errors: `400 refresh_token is required`, `401 invalid token` (unknown),
`401 token expired`, `401 token revoked`.

### `POST /auth/logout`

Revokes a refresh token. The access token stays valid until it expires, so the
client should drop it. **Auth:** none.

Request: `{ "refresh_token": "string" }`

`204 No Content`.

Errors: `400 refresh_token is required`, `401 invalid token`.

### `GET /auth/me`

**Auth:** required.

`200 OK`:

```json
{ "id": "6f1c…", "email": "ada@example.com", "name": "Ada" }
```

Errors: `401`, `404 user not found`.

### OAuth: Google and GitHub

| Start | Callback |
|---|---|
| `GET /auth/google` | `GET /auth/google/callback` |
| `GET /auth/github` | `GET /auth/github/callback` |

**Auth:** none. These are browser navigations, not fetch calls.

1. **Start**: navigate the browser (full-page redirect, not XHR) to
   `/auth/google` or `/auth/github`. The server sets a short-lived state
   cookie (10 min, `HttpOnly`, `Secure`, `SameSite=Lax`) and answers
   `302` to the provider. If that provider isn't configured on the server:
   `503 oauth provider not configured`.
2. **Callback**: the provider redirects back to `…/callback?code=…&state=…`
   (this URL is configured on the server via `GOOGLE_REDIRECT_URL` /
   `GITHUB_REDIRECT_URL`). The server finds or creates the user and issues a
   token pair, then:
   - by default answers `302` to `FRONTEND_CALLBACK_URL` with the tokens in the
     query string:
     `…/auth/callback?access_token=…&refresh_token=…&expires_in=900`.
     The frontend should read them, store them and remove them from the URL
     (`history.replaceState`).
   - if the request has `Accept: application/json`, it answers `200` with the
     token pair instead of redirecting.

Callback errors (JSON error body): `400 oauth authorization denied` (user
cancelled), `400 invalid oauth state` (missing or mismatched state cookie),
`400 missing authorization code`, `409 email already registered with another provider`
(the email belongs to an account from a different provider or to an
email+password account), `502 oauth token exchange failed`,
`502 failed to fetch oauth profile`, `503 oauth provider not configured`.

## Profile

### `GET /profile`

**Auth:** required. `200 OK`:

```json
{
  "id": "6f1c…",
  "email": "ada@example.com",
  "name": "Ada",
  "role": "Backend developer",
  "stack": "Go, Postgres",
  "timezone": "Europe/Berlin",
  "avatar_status": "none",
  "created_at": "2026-10-15T09:12:44.123456Z"
}
```

- `role`, `stack`, `timezone`: strings, *omitted when empty* (never set).
- `avatar_status`: `"none"`, `"pending"` or `"ready"`. There is no avatar
  upload endpoint yet, so it's always `"none"` for now.

Errors: `401`, `404 user not found`.

### `PATCH /profile`

**Auth:** required. Partial update. Send only the fields to change.

| Field | Type | Rules (after trimming whitespace; lengths in characters) |
|---|---|---|
| `name` | string | 1–100 characters. Can't be empty. |
| `role` | string | 0–100 characters. `""` clears it. |
| `stack` | string | 0–200 characters. `""` clears it. |
| `timezone` | string | A valid IANA time zone name, e.g. `Europe/Berlin`, `America/Argentina/Buenos_Aires`, `UTC`. Case-sensitive. Offsets like `+03:00`, `""` and `Local` are rejected. |

A field that is absent or `null` is left unchanged. A cleared `role`/`stack`
comes back as `""`. If any field is invalid, nothing is saved.

`200 OK` → the full profile (same shape as `GET /profile`).

Errors (all `400` unless noted): `no fields to update` (none of the four
fields present), `name must be 1-100 characters`,
`role must be at most 100 characters`, `stack must be at most 200 characters`,
`timezone must be a valid IANA time zone name, e.g. Europe/Berlin`,
`invalid request body`; `401`; `404 user not found`.

## Rooms

Rooms are fixed and seeded by migrations: there are no create/update/delete
endpoints. Room `type` is `"focus"`, `"collab"` or `"break"`.

Room object:

```json
{
  "id": "0b6e…",
  "name": "Deep Focus",
  "type": "focus",
  "tags": ["quiet", "solo"],
  "created_at": "2026-10-01T00:00:00Z"
}
```

### `GET /rooms`

**Auth:** required.

Query parameters (both optional):

| Param | Meaning |
|---|---|
| `type` | Only rooms of this type. An unknown type returns `[]`. |
| `tags` | Comma-separated, e.g. `tags=quiet,code`. A room matches if it has **any** of them. |

`200 OK` → array of room objects, sorted by name. `[]` when nothing matches.

Errors: `401`.

### `GET /rooms/{id}`

**Auth:** required. `200 OK`:

```json
{
  "room": { "id": "0b6e…", "name": "Deep Focus", "type": "focus", "tags": ["quiet"], "created_at": "…" }
}
```

Rooms have no sessions: timers are per task (see [Timer flow](#timer-flow)).

Errors: `400 invalid room id`, `401`, `404 room not found`.

### `POST /rooms/{id}/join`

Join a room. **Auth:** required. No body.

This is **idempotent**: if the user is already in the room, the existing
participation is returned and nothing new is created. A user may be in
several rooms at once.

Joining is required before: connecting the room WebSocket, sending or reading
chat, starting a task timer, and getting a call token.

`200 OK`:

```json
{
  "room": { "id": "0b6e…", "name": "Deep Focus", "type": "focus", "tags": ["quiet"], "created_at": "…" },
  "participation": {
    "id": "…",
    "user_id": "6f1c…",
    "room_id": "0b6e…",
    "joined_at": "2026-10-22T14:03:11.482913Z"
  }
}
```

The participation's `left_at` and `left_reason` are *omitted when empty*
(always, for an open participation).

Errors: `400 invalid room id`, `401`, `404 room not found`.

### `POST /rooms/{id}/leave`

Leave a room. **Auth:** required. No body.

`204 No Content`. The user's room WebSockets are then closed and the room gets
a `user_left` event.

Errors: `400 invalid room id`, `401`, `404 not currently in this room`.

### `GET /rooms/{id}/participants`

Who is in the room right now (users with an open participation: joined and not
left or removed). **Auth:** required, and the caller must be in the room too.

`200 OK` → array, earliest joiner first. It always includes the caller:

```json
[
  { "user_id": "6f1c…", "name": "Ada", "joined_at": "2026-10-22T14:03:11.482913Z" },
  { "user_id": "8a2d…", "name": "Linus", "joined_at": "2026-10-22T14:05:40.019311Z" }
]
```

- There is no online status: a participant may have no open WebSocket. For
  "online now", use WS presence (`presence_snapshot`, `user_joined`,
  `user_left`; see [ws-protocol.md](ws-protocol.md)).
- No avatar field yet: users have no avatar URL.

Errors: `400 invalid room id`, `401`,
`403 join the room to see its participants` (caller not in the room, or the
room doesn't exist).

## Invites

Invites are referral codes only. Registration doesn't require or consume
them, and checking a code never marks it as used.

### `POST /invites`

**Auth:** required. No body.

`201 Created`:

```json
{
  "id": "…",
  "code": "a7Kp3XmQ9z",
  "created_by": "6f1c…",
  "created_at": "2026-10-22T14:03:11.482913Z"
}
```

`code` is 10 characters from an alphabet without look-alikes (no `0 O 1 l I`).

Errors: `401`, `500 failed to create invite`.

### `GET /invites/{code}`

**Auth:** none.

`200 OK`. Always 200 for any code, valid or not:

```json
{ "valid": true }
```

```json
{ "valid": false, "reason": "not_found" }
```

`reason` is `"not_found"` or `"already_used"` and is *omitted when valid*.

## Tasks

Task object:

```json
{
  "id": "9d2a…",
  "room_id": "0b6e…",
  "text": "Write the API reference",
  "created_at": "2026-10-22T14:00:00.000000Z",
  "updated_at": "2026-10-22T14:05:00.000000Z",
  "extended_count": 1,
  "extra_minutes_used": 15,
  "timer_status": "running",
  "timer_duration_minutes": 25,
  "timer_started_at": "2026-10-22T14:30:00.000000Z",
  "timer_ends_at": "2026-10-22T14:45:00.000000Z"
}
```

| Field | Notes |
|---|---|
| `session_id` | Room session the task was created in; *omitted when empty* (always, today). |
| `completed_at` | Set when completed; *omitted when empty*. |
| `extended_count` | Extensions granted so far. |
| `extra_minutes_used` | Sum of extension minutes. |
| `timer_status` | `"idle"` (never started), `"running"`, `"prompting"` (timer ran out, waiting for the extension answer), `"ended"`. |
| `timer_duration_minutes` | Length chosen at start (25, 50 or 90); *omitted when empty*. |
| `timer_started_at`, `timer_ends_at` | Current run; after an extension they are the extension's start and end. *Omitted when empty.* |
| `prompt_deadline_at` | While `prompting`: when the prompt times out. *Omitted when empty.* |

### `POST /rooms/{id}/tasks`

Create a task in a room. **Auth:** required. Being in the room is **not**
required to create a task, only to start its timer.

Request: `{ "text": "string" }`. Trimmed, then 1–200 characters (Unicode
code points, not bytes). Tasks have a single text field; there is no
separate description.

`201 Created` → task object (`timer_status: "idle"`).

Errors: `400 invalid room id`, `400 text is required`,
`400 text must be at most 200 characters`,
`400 invalid request body`, `401`, `404 room not found`.

### `GET /tasks/me`

The current user's tasks across rooms, newest first, one page at a time.
**Auth:** required.

| Param | Default | Rules |
|---|---|---|
| `limit` | 50 | Positive integer. Values above 100 are capped to 100. |
| `offset` | 0 | Non-negative integer: how many tasks to skip. |

`200 OK` → a plain array of task objects (`[]` when none, or past the end).
There is no total count: a page shorter than `limit` is the last one. To load
more, request `?offset=<items loaded so far>`. Tasks created meanwhile shift
the offsets, so de-duplicate by `id`.

Errors: `400 limit must be a positive integer`,
`400 offset must be a non-negative integer`, `401`.

### `PATCH /tasks/{id}`

Edit or complete your own task. **Auth:** required (owner only).

| Field | Type | Rules |
|---|---|---|
| `text` | string | Optional. Trimmed, then 1–200 characters, as on create. |
| `completed` | bool | Optional. `true` completes (sets `completed_at` to now), `false` un-completes. |

Absent (or `null`) fields are unchanged, but at least one must be present:
`{}` is a `400 no fields to update`, the same as `PATCH /profile`.
Completing a task whose timer is `running` or `prompting` ends the timer
(`timer_status: "ended"`), and the room receives `timer_ended` with reason
`completed`.

`200 OK` → task object.

Errors: `400 invalid task id`, `400 no fields to update`,
`400 text is required`, `400 text must be at most 200 characters`,
`400 invalid request body`, `401`,
`403 not the owner of this task`, `404 task not found`.

There is no endpoint to delete a task.

### `POST /tasks/{id}/start`

Start the task's focus timer. **Auth:** required (owner only).

Request: `{ "duration_minutes": 25 }`. Must be `25`, `50` or `90`.

Rules: the task must not be completed, the owner must currently be in the
task's room, and the user can't have another timer that is `running` or
`prompting` (one active timer per user). A timer that has `ended` can be
started again.

`200 OK` → task object (`timer_status: "running"`). The room receives
`timer_started`.

Errors:
- `400 duration_minutes must be one of 25, 50, 90`, `400 invalid request body`,
  `400 invalid task id`
- `401`, `403 not the owner of this task`, `404 task not found`
- `409 task is already completed`
- `409 join the task's room before starting a timer`
- `409 a timer is already running for this user`

### `POST /tasks/{id}/extend`

Answer the extension prompt with "more time". **Auth:** required (owner only).

Request: `{ "minutes": 15 }`. Must be `15` or `30`.

Only valid while `timer_status` is `"prompting"` and before
`prompt_deadline_at`. The timer runs again for `minutes` from now.

`200 OK` → task object (`timer_status: "running"`, `extended_count`
incremented). The room receives `task_extended`.

Errors:
- `400 minutes must be one of 15, 30`, `400 invalid request body`, `400 invalid task id`
- `401`, `403 not the owner of this task`, `404 task not found`
- `409 the task's timer is not waiting for an extension answer`
- `409 the extension prompt was already answered or has expired`
- `409 extension limit reached for this task`

### `POST /tasks/{id}/no-extend`

Answer the extension prompt with "no". **Auth:** required (owner only). No body.

The timer ends and the user is **removed from the room**, exactly as when the
prompt times out: their sockets get `kicked`, the room gets `user_left`.
They can join again later.

`200 OK` → task object (`timer_status: "ended"`).

Errors: `400 invalid task id`, `401`, `403 not the owner of this task`,
`404 task not found`,
`409 the task's timer is not waiting for an extension answer`,
`409 the extension prompt was already answered or has expired`.

## Timer flow

The full event sequence is in [ws-protocol.md](ws-protocol.md) (sections
`timer_started`, `timer_ended`, `task_extension_prompt`, `task_extended`,
`kicked`). In short:

1. Join the room (`POST /rooms/{id}/join`) and open its WebSocket.
2. `POST /tasks/{id}/start` with a duration → `timer_started` to the room.
3. When the time is up: `timer_ended` (reason `expired`) to the room, then
   `task_extension_prompt` to the owner only, with the deadline and allowed
   minutes.
4. The owner answers before the deadline:
   - `POST /tasks/{id}/extend` → `task_extended`, and the timer runs again
     (back to step 3 when it ends);
   - `POST /tasks/{id}/no-extend`, or no answer by the deadline → the timer
     ends and the user is removed from the room (`kicked`).
5. Completing the task (`PATCH /tasks/{id}` with `completed: true`) at any
   point ends the timer (`timer_ended`, reason `completed`).

Constants:

| What | Value | Configurable |
|---|---|---|
| Focus durations | 25, 50, 90 minutes | no |
| Extension lengths | 15, 30 minutes | no |
| Max extensions per task | 3 | `EXTENSION_MAX_COUNT` |
| Prompt timeout | 60 s | `EXTENSION_PROMPT_TIMEOUT` |
| Active timers per user | 1 (`running` or `prompting`) | no |

When the extension limit is reached, the prompt is still sent, but with
`extensions_left: 0` and an empty `allowed_minutes`. The only answers left are
`no-extend` or letting it time out.

## Chat

Text only. You must be **currently in the room** (joined and not left) to send
or read.

Message object:

```json
{
  "id": "3c9e…",
  "room_id": "0b6e…",
  "user_id": "6f1c…",
  "user_name": "Ada",
  "text": "Starting a 50-minute block",
  "created_at": "2026-10-22T14:03:11.482913Z"
}
```

`user_name` is the author's *current* name.

### `POST /rooms/{id}/messages`

**Auth:** required, in the room.

Request: `{ "text": "string" }`. Leading and trailing whitespace is trimmed,
then the length must be 1–2000 characters (Unicode code points).

`201 Created` → message object. The same message is broadcast to the room as
`chat_message` (including the sender's own sockets, so de-duplicate by `id`).

Errors: `400 invalid room id`, `400 text is required` (empty or only
whitespace), `400 text must be at most 2000 characters`,
`400 invalid request body`, `401`,
`403 join the room before using its chat` (not in the room, or the room
doesn't exist), `429 too many messages, slow down`: more than
`CHAT_RATE_LIMIT` (default 10) messages from this user within
`CHAT_RATE_WINDOW` (default 10 s), across all rooms. The `Retry-After`
header gives the seconds to wait. Rejected messages aren't stored or
broadcast. Invalid messages get their `400` first and don't count.

### `GET /rooms/{id}/messages`

History, newest first, with cursor pagination. **Auth:** required, in the room.

| Param | Default | Rules |
|---|---|---|
| `limit` | 50 | Positive integer. Values above 100 are capped to 100. |
| `before` | — | A message id from this room. Returns messages older than it. |

`200 OK`:

```json
{
  "items": [ { "id": "3c9e…", "room_id": "…", "user_id": "…", "user_name": "Ada", "text": "…", "created_at": "…" } ],
  "next_cursor": "a01b…"
}
```

- `items` is never `null` (`[]` when empty).
- `next_cursor` is the id to pass as `before` for the next (older) page, or
  `null` when there is nothing older.
- Usage: first load without `before`. To scroll up, request
  `?before=<next_cursor>` until `next_cursor` is `null`. New messages arrive
  via `chat_message` on the WebSocket.

Errors: `400 invalid room id`, `400 limit must be a positive integer`,
`400 before must be a message id from this room` (bad UUID, unknown id or a
message from another room), `401`, `403 join the room before using its chat`.

## Calls

Audio/video runs on LiveKit. The API only hands out a join token.

### `POST /rooms/{id}/call/token`

**Auth:** required, in the room. No body.

`200 OK`:

```json
{ "url": "wss://livekit.example.com", "token": "eyJhbGciOiJIUzI1NiIs..." }
```

Pass both to the LiveKit client SDK (`room.connect(url, token)`). The token:

- is valid for **1 hour** (for joining; an established connection isn't cut
  when it expires);
- joins the LiveKit room named by the room UUID, with identity = user id and
  name = user name;
- allows publishing and subscribing.

Request a new token for every connection attempt and don't cache it.

Errors: `400 invalid room id`, `401`,
`403 join the room before joining its call` (not in the room or unknown room),
`503 call service not configured` (the server has no LiveKit settings).

## WebSocket

`GET /rooms/{id}/ws?access_token=<access_token>`. See
[ws-protocol.md](ws-protocol.md) for the handshake, events and close codes.
Handshake failures are plain HTTP responses with the standard error body:
`400 invalid room id`, `403 origin not allowed`,
`401 missing or invalid access token`,
`403 join the room before connecting`.

## Health

### `GET /health`

**Auth:** none. For load balancers and monitoring.

`200 OK` when Postgres and Redis are reachable:

```json
{ "status": "ok", "checks": { "postgres": "ok", "redis": "ok" } }
```

`503 Service Unavailable` otherwise, same shape, with `"status": "degraded"`
and the failing check set to `"unavailable"`.

## Endpoint index

| Method | Path | Auth |
|---|---|---|
| GET | `/health` | — |
| POST | `/auth/register` | — |
| POST | `/auth/login` | — |
| POST | `/auth/refresh` | — |
| POST | `/auth/logout` | — |
| GET | `/auth/me` | Bearer |
| GET | `/auth/google` | — (browser) |
| GET | `/auth/google/callback` | — (browser) |
| GET | `/auth/github` | — (browser) |
| GET | `/auth/github/callback` | — (browser) |
| GET | `/profile` | Bearer |
| PATCH | `/profile` | Bearer |
| GET | `/rooms` | Bearer |
| GET | `/rooms/{id}` | Bearer |
| POST | `/rooms/{id}/join` | Bearer |
| POST | `/rooms/{id}/leave` | Bearer |
| GET | `/rooms/{id}/participants` | Bearer + in room |
| GET | `/rooms/{id}/ws` | `?access_token=` |
| POST | `/rooms/{id}/messages` | Bearer + in room |
| GET | `/rooms/{id}/messages` | Bearer + in room |
| POST | `/rooms/{id}/call/token` | Bearer + in room |
| POST | `/invites` | Bearer |
| GET | `/invites/{code}` | — |
| POST | `/rooms/{id}/tasks` | Bearer |
| GET | `/tasks/me` | Bearer |
| PATCH | `/tasks/{id}` | Bearer (owner) |
| POST | `/tasks/{id}/start` | Bearer (owner, in room) |
| POST | `/tasks/{id}/extend` | Bearer (owner) |
| POST | `/tasks/{id}/no-extend` | Bearer (owner) |
