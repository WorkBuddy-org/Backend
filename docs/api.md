# WorkBuddy REST API

The contract for the frontend. It is written from the handlers in
`internal/*/handler.go`, the router in `internal/httpapi/router.go` and their
tests. `internal/httpapi/docs_routes_test.go` fails CI when a route here and
a route in the router don't match. Real-time events are in
[ws-protocol.md](ws-protocol.md).

- [Basics](#basics)
- [Authentication](#authentication) (cookies, refresh, CORS, CSRF)
- [Errors](#errors) (format and code table)
- Endpoints: [Health](#health) · [Auth](#auth) · [Profile](#profile) ·
  [Rooms](#rooms) · [Tasks](#tasks) · [Chat](#chat) · [Calls](#calls) ·
  [Invites](#invites) · [WebSocket](#websocket)
- [Timer flow](#timer-flow)
- [Endpoint index](#endpoint-index)

## Basics

**Base URL.** No `/api` or version prefix; paths below are relative to the
server root.

| Environment | Base URL |
|---|---|
| Local | `http://localhost:8080` (port from `PORT`) |
| Production | `https://api.<domain>` (behind Caddy, see [deploy.md](deploy.md)) |

**JSON.** Request and response bodies are JSON
(`Content-Type: application/json`). Request bodies are strict: unknown
fields, wrong JSON types and malformed JSON are rejected with
`400 VALIDATION_ERROR`. Bodies are read up to 1 MiB. Endpoints marked
"no body" ignore any body sent.

**IDs** are UUID strings. A path `{id}` that isn't a UUID gives
`400 VALIDATION_ERROR` with `details.fields = {"id": "INVALID_FORMAT"}`.

**Timestamps** are RFC 3339 strings in UTC. REST responses carry the
database's microsecond precision (`"2026-10-22T14:03:11.482913Z"`);
WebSocket payloads use milliseconds.

**Optional fields.** Fields marked *omitted when empty* are left out of the
JSON instead of being `null`.

**Text lengths** are counted in Unicode code points after trimming leading
and trailing whitespace, except `password` (bytes, not trimmed).

## Authentication

The browser frontend is authenticated by **HttpOnly cookies** set by the
API. JavaScript never sees the tokens. Every call must send cookies:

```js
fetch(`${API}/rooms`, { credentials: "include" })
```

| Cookie | Path | Max-Age | Holds |
|---|---|---|---|
| `access_token` | `/` | `JWT_ACCESS_TTL` (15 min) | JWT access token |
| `refresh_token` | `/auth` | `JWT_REFRESH_TTL` (30 days) | Opaque refresh token. Only sent to `/auth/*` |

Both cookies are `HttpOnly` and `SameSite=Lax`. They are `Secure` in
production (`COOKIE_SECURE`) and carry `Domain=COOKIE_DOMAIN`, e.g.
`.example.com`, so that `app.example.com` and `api.example.com` share them.
With an empty `COOKIE_DOMAIN` they are host-only, which is right for
`localhost`. In development the frontend at `http://localhost:3000` calls
`http://localhost:8080`; the two are the same site, so `Lax` cookies flow.

| Endpoint | Cookies |
|---|---|
| `POST /auth/register`, `POST /auth/login`, OAuth callback | set both |
| `POST /auth/refresh` | rotate both on success; clear both on `401` |
| `POST /auth/logout` | clear both |

**Protected endpoints** take the access token from the `access_token`
cookie. When there is no such cookie they fall back to the header (for tools
and tests):

```
Authorization: Bearer <access_token>
```

| Situation | Response |
|---|---|
| No token, malformed token, bad signature | `401 UNAUTHORIZED` |
| Genuine token past its expiry | `401 TOKEN_EXPIRED` |

The WebSocket handshake reads the same cookie, with `?access_token=` as a
fallback ([ws-protocol.md](ws-protocol.md#1-connecting)).

### Refresh flow

1. Call the API with `credentials: "include"`.
2. On `401` with `TOKEN_EXPIRED`, or with `UNAUTHORIZED` once the browser
   has already dropped the expired `access_token` cookie (its Max-Age equals
   the token lifetime), call `POST /auth/refresh` **once**, then retry the
   original request.
3. `204` means new cookies are set. Refresh tokens rotate: the old one is
   revoked, and replaying it gives `401`.
4. If refresh answers `401`, the cookies are already cleared: show the login
   screen.

Run only one refresh at a time (queue other requests behind it). Two parallel
refreshes with the same cookie make the second one fail with `401`.

### CORS

Origins allowed to call the API from a browser come from
`CORS_ALLOWED_ORIGINS` (comma-separated, exact `scheme://host[:port]`). When
it is empty, development allows `http://localhost:3000` and other
environments allow none.

| Setting | Value |
|---|---|
| `Access-Control-Allow-Credentials` | `true` |
| Allowed methods | `GET`, `POST`, `PATCH`, `DELETE`, `OPTIONS` |
| Allowed request headers | `Content-Type`, `Authorization` |
| Exposed response headers | `Retry-After` |
| Preflight cache | 300 s |

### CSRF rule

A `POST`, `PATCH`, `PUT` or `DELETE` that is authenticated by a cookie is
rejected with `403 FORBIDDEN` ("origin not allowed") when it carries an
`Origin` header that is not in `CORS_ALLOWED_ORIGINS`. This includes
`POST /auth/refresh` and `POST /auth/logout`, which use the refresh cookie.
Requests without an `Origin` header and requests authenticated by
`Authorization: Bearer` are not checked. The WebSocket has its own origin
allow-list, `WS_ALLOWED_ORIGINS`.

## Errors

Every error response, on every endpoint (unknown routes and panics included),
has this body:

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "invalid email address",
    "details": { "fields": { "email": "INVALID_FORMAT" } }
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `error.code` | string | Stable, machine-readable. **Branch on this.** |
| `error.message` | string | English debug text. Safe to log; may change; don't match on it. |
| `error.details` | object | Optional, omitted when empty. Only `VALIDATION_ERROR` uses it today, as `details.fields`. |
| `error.details.fields` | object | Request field → reason. Keys are body fields, query parameters, or `id` for the path id. |

Field reasons:

| Reason | Meaning |
|---|---|
| `REQUIRED` | Missing or empty after trimming |
| `INVALID_FORMAT` | Wrong shape (not an email, not a UUID) |
| `INVALID_VALUE` | Not one of the allowed values, or out of range |
| `INVALID_TYPE` | Wrong JSON type, e.g. a string for a number |
| `INVALID_LENGTH` | Length outside min..max |
| `TOO_SHORT` | Below the minimum length |
| `TOO_LONG` | Above the maximum length |
| `UNKNOWN_FIELD` | The endpoint doesn't accept this field |

Error codes:

| Code | Status | When |
|---|---|---|
| `VALIDATION_ERROR` | 400 | Malformed JSON, missing body, unknown fields, wrong types, empty/invalid fields, values outside enums, empty `PATCH` body, bad path id or query parameter |
| `UNAUTHORIZED` | 401 | Access token missing, malformed or wrongly signed; refresh token missing, unknown, expired or revoked |
| `TOKEN_EXPIRED` | 401 | Access token present and genuine, but expired. Refresh and retry |
| `INVALID_CREDENTIALS` | 401 | Wrong email or password at login (or an OAuth-only account) |
| `FORBIDDEN` | 403 | Signed in but not allowed: not a room participant, not the task owner, request `Origin` not allowed (CSRF rule, WebSocket origin), OAuth denied or bad OAuth state |
| `NOT_FOUND` | 404 | Resource or route doesn't exist |
| `METHOD_NOT_ALLOWED` | 405 | Route exists, but not for this method |
| `EMAIL_EXISTS` | 409 | Register or OAuth sign-in with an email that is already taken |
| `EXTEND_LIMIT_REACHED` | 409 | `POST /tasks/{id}/extend` when the task has no extensions left |
| `CONFLICT` | 409 | Other state conflicts: task completed, a timer already running, no extension prompt pending, prompt expired |
| `RATE_LIMITED` | 429 | Register, login or chat limit hit. Always has `Retry-After` (whole seconds) |
| `SERVICE_UNAVAILABLE` | 503 | Feature not configured on the server (an OAuth provider, LiveKit) |
| `INTERNAL` | 500, 502 | Unexpected failure. Generic message; details only in server logs. `502` when an OAuth provider call fails |
| `ROOM_FULL` | 409 | Reserved, not returned yet |
| `ALREADY_RESERVED` | 409 | Reserved, not returned yet |
| `ROOM_ENDED` | 409 | Reserved, not returned yet |
| `INVITE_EXPIRED` | — | Reserved, not returned yet (invites don't expire; see [`GET /invites/{code}`](#get-invitescode)) |
| `INVITE_INVALID` | — | Reserved, not returned yet |
| `FILE_TOO_LARGE` | — | Reserved for uploads, not returned yet |
| `UNSUPPORTED_FILE_TYPE` | — | Reserved for uploads, not returned yet |

The error tables below list each endpoint's own errors. Every endpoint can
also return `500 INTERNAL`. Every endpoint with **Auth: required** can also
return `401 UNAUTHORIZED` / `401 TOKEN_EXPIRED`. Every cookie-authenticated
`POST`/`PATCH`/`DELETE` can also return `403 FORBIDDEN` from the
[CSRF rule](#csrf-rule). Body errors (`400 VALIDATION_ERROR` with
`UNKNOWN_FIELD` / `INVALID_TYPE`, "invalid request body", "request body is
required") apply to every endpoint with a request body.

---

## Health

### `GET /health`

| | |
|---|---|
| Auth | none |

For load balancers and monitoring. **Not** in the error format.

`200 OK`, or `503 Service Unavailable` with `"status": "degraded"` and the
failing check set to `"unavailable"`:

```json
{ "status": "ok", "checks": { "postgres": "ok", "redis": "ok" } }
```

| Field | Type |
|---|---|
| `status` | `"ok"` \| `"degraded"` |
| `checks.postgres`, `checks.redis` | `"ok"` \| `"unavailable"` |

---

## Auth

User object (register, login, `GET /auth/me`):

| Field | Type |
|---|---|
| `id` | UUID |
| `email` | string |
| `name` | string (may be `""`) |

### `POST /auth/register`

| | |
|---|---|
| Auth | none |
| Rate limit | 5 requests per client IP per 10 min (`REGISTER_RATE_LIMIT`, `REGISTER_RATE_WINDOW`). Every request counts, failed ones included |

Request body:

| Field | Type | Required | Rules |
|---|---|---|---|
| `email` | string | yes | Trimmed and lower-cased; must contain `@` with text on both sides, no whitespace |
| `password` | string | yes | At least 8 bytes |
| `name` | string | no | Display name, not validated (may be empty) |

`201 Created`, both auth cookies set:

```json
{ "user": { "id": "6f1c2a4e-…", "email": "ada@example.com", "name": "Ada" } }
```

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"email": "INVALID_FORMAT"}` ("invalid email address"); `{"password": "TOO_SHORT"}` ("password must be at least 8 characters") |
| 409 | `EMAIL_EXISTS` | The email belongs to any existing account |
| 429 | `RATE_LIMITED` | Over the limit; `Retry-After` gives the seconds until the window resets |

The client IP is the connection address, or the first `X-Forwarded-For`
value behind a proxy with `TRUSTED_PROXY_HEADER` set. If Redis is down,
requests are not limited.

### `POST /auth/login`

| | |
|---|---|
| Auth | none |
| Rate limit | 5 attempts per email per 15 min; a successful login resets the counter |

Request body:

| Field | Type | Required |
|---|---|---|
| `email` | string | yes |
| `password` | string | yes |

`200 OK`, both auth cookies set, body as for register:

```json
{ "user": { "id": "6f1c2a4e-…", "email": "ada@example.com", "name": "Ada" } }
```

| Status | Code | When |
|---|---|---|
| 401 | `INVALID_CREDENTIALS` | Unknown email, OAuth-only account or wrong password. These are deliberately indistinguishable |
| 429 | `RATE_LIMITED` | Over the limit; `Retry-After` set |

### `POST /auth/refresh`

| | |
|---|---|
| Auth | `refresh_token` cookie |
| Body | none (ignored) |

Rotates the refresh token (the old one is revoked) and sets both cookies
again. `204 No Content`.

| Status | Code | When |
|---|---|---|
| 401 | `UNAUTHORIZED` | No refresh cookie, or the token is unknown, expired or revoked. Both cookies are cleared |
| 403 | `FORBIDDEN` | Foreign `Origin` ([CSRF rule](#csrf-rule)) |

### `POST /auth/logout`

| | |
|---|---|
| Auth | `refresh_token` cookie, optional |
| Body | none (ignored) |

Revokes the cookie's refresh token and clears both cookies. Always
`204 No Content`, including without a cookie or with an already revoked
token.

| Status | Code | When |
|---|---|---|
| 403 | `FORBIDDEN` | Foreign `Origin` |

### `GET /auth/me`

| | |
|---|---|
| Auth | required |

`200 OK`: the user object.

```json
{ "id": "6f1c2a4e-…", "email": "ada@example.com", "name": "Ada" }
```

| Status | Code | When |
|---|---|---|
| 404 | `NOT_FOUND` | The user was deleted |

### `GET /auth/google`

### `GET /auth/github`

| | |
|---|---|
| Auth | none |
| Call as | full-page navigation (`window.location`), not `fetch` |

Sets a 10-minute OAuth state cookie (`HttpOnly`, `Secure`, `SameSite=Lax`)
and answers `302` to the provider's consent page.

| Status | Code | When |
|---|---|---|
| 503 | `SERVICE_UNAVAILABLE` | That provider isn't configured on the server (JSON body) |

### `GET /auth/google/callback`

### `GET /auth/github/callback`

| | |
|---|---|
| Auth | OAuth state cookie |
| Called by | the provider's redirect (`GOOGLE_REDIRECT_URL` / `GITHUB_REDIRECT_URL`) |

Always answers `302 Found` to `FRONTEND_CALLBACK_URL`. **Tokens are never in
the URL.**

| Outcome | Redirect | Cookies |
|---|---|---|
| Signed in (user found, linked or created) | `…?status=ok` | both auth cookies set |
| Failed | `…?error=<CODE>` | none |

| `error` | Cause |
|---|---|
| `FORBIDDEN` | The user denied access at the provider, or the state cookie is missing or doesn't match |
| `VALIDATION_ERROR` | The provider sent no `code` |
| `EMAIL_EXISTS` | The email belongs to an account from another provider or to an email+password account |
| `SERVICE_UNAVAILABLE` | That provider isn't configured |
| `INTERNAL` | Code exchange, profile fetch or token issuing failed |

The frontend callback page reads `status` / `error`; on `status=ok` it can
call `GET /auth/me`. When `FRONTEND_CALLBACK_URL` itself is not configured
(never in production), the callback answers `500 INTERNAL` as JSON.

---

## Profile

Profile object:

```json
{
  "id": "6f1c2a4e-…",
  "email": "ada@example.com",
  "name": "Ada",
  "role": "Backend developer",
  "stack": "Go, Postgres",
  "timezone": "Europe/Berlin",
  "avatar_status": "none",
  "created_at": "2026-10-15T09:12:44.123456Z"
}
```

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `email` | string | |
| `name` | string | |
| `role` | string | *Omitted when empty* (never set); `""` after being cleared |
| `stack` | string | Same as `role` |
| `timezone` | string | IANA zone name; *omitted when empty* |
| `avatar_status` | `"none"` \| `"pending"` \| `"ready"` | There is no upload endpoint yet, so always `"none"` |
| `created_at` | timestamp | |

### `GET /profile`

| | |
|---|---|
| Auth | required |

`200 OK`: the profile object.

| Status | Code | When |
|---|---|---|
| 404 | `NOT_FOUND` | The user was deleted |

### `PATCH /profile`

| | |
|---|---|
| Auth | required |

Partial update. Absent or `null` fields are unchanged. At least one field
must be present. If any field is invalid, nothing is saved.

| Field | Type | Rules |
|---|---|---|
| `name` | string | 1–100 characters |
| `role` | string | 0–100 characters; `""` clears it |
| `stack` | string | 0–200 characters; `""` clears it |
| `timezone` | string | A valid IANA name, case-sensitive (`Europe/Berlin`, `UTC`). Offsets such as `+03:00`, `""` and `Local` are rejected |

`200 OK`: the full profile object.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | No field present ("no fields to update", no details); `{"name": "INVALID_LENGTH"}`; `{"role": "TOO_LONG"}`; `{"stack": "TOO_LONG"}`; `{"timezone": "INVALID_VALUE"}` |
| 404 | `NOT_FOUND` | The user was deleted |

---

## Rooms

Rooms are fixed and seeded by migrations. There are no create, update or
delete endpoints.

Room object:

```json
{
  "id": "0b6e5d4c-…",
  "name": "Deep Focus",
  "type": "focus",
  "tags": ["quiet", "solo"],
  "created_at": "2026-10-01T00:00:00Z"
}
```

| Field | Type |
|---|---|
| `id` | UUID |
| `name` | string |
| `type` | `"focus"` \| `"collab"` \| `"break"` |
| `tags` | string[] (may be `[]`) |
| `created_at` | timestamp |

Participation object:

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `user_id` | UUID | |
| `room_id` | UUID | |
| `joined_at` | timestamp | |
| `left_at` | timestamp | *Omitted when empty* (always, for an open participation) |
| `left_reason` | `"left_voluntarily"` \| `"kicked_no_extension"` \| `"kicked"` \| `"completed_no_extension"` | *Omitted when empty*. Today only the first two are written |

### `GET /rooms`

| | |
|---|---|
| Auth | required |

Query parameters, both optional:

| Param | Type | Meaning |
|---|---|---|
| `type` | string | Only rooms of this type; an unknown type gives `[]` |
| `tags` | comma-separated strings | Rooms having **any** of the tags, e.g. `tags=quiet,code` |

`200 OK`: an array of room objects, sorted by name (`[]` when nothing
matches). No pagination.

No errors of its own.

### `GET /rooms/{id}`

| | |
|---|---|
| Auth | required |

`200 OK`:

```json
{ "room": { "id": "0b6e5d4c-…", "name": "Deep Focus", "type": "focus", "tags": ["quiet"], "created_at": "…" } }
```

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}` |
| 404 | `NOT_FOUND` | No such room |

### `POST /rooms/{id}/join`

| | |
|---|---|
| Auth | required |
| Body | none |

**Idempotent**: if the user is already in the room, the open participation is
returned and nothing new is created. A user may be in several rooms at once.
Joining is required before opening the room WebSocket, using chat, starting a
task timer or getting a call token.

`200 OK`:

```json
{
  "room": { "id": "0b6e5d4c-…", "name": "Deep Focus", "type": "focus", "tags": ["quiet"], "created_at": "…" },
  "participation": {
    "id": "c1d2e3f4-…",
    "user_id": "6f1c2a4e-…",
    "room_id": "0b6e5d4c-…",
    "joined_at": "2026-10-22T14:03:11.482913Z"
  }
}
```

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}` |
| 404 | `NOT_FOUND` | No such room |

### `POST /rooms/{id}/leave`

| | |
|---|---|
| Auth | required |
| Body | none |

`204 No Content`. The user's sockets to this room are closed with `1000`, and
the rest of the room gets `user_left` with reason `left_voluntarily`.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}` |
| 404 | `NOT_FOUND` | Not currently in this room ("not currently in this room") |

### `GET /rooms/{id}/participants`

| | |
|---|---|
| Auth | required, and the caller must be in the room |

Users with an open participation (joined, not left or removed), earliest
joiner first. Always includes the caller.

`200 OK`:

```json
[
  { "user_id": "6f1c2a4e-…", "name": "Ada", "joined_at": "2026-10-22T14:03:11.482913Z" },
  { "user_id": "8a2d9b0c-…", "name": "Linus", "joined_at": "2026-10-22T14:05:40.019311Z" }
]
```

| Field | Type |
|---|---|
| `user_id` | UUID |
| `name` | string |
| `joined_at` | timestamp |

This is membership, not online status: a participant may have no open
socket. For "online now" use WebSocket presence. There is no avatar field.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}` |
| 403 | `FORBIDDEN` | The caller isn't in the room, or the room doesn't exist |

---

## Tasks

Task object:

```json
{
  "id": "9d2a7c6b-…",
  "room_id": "0b6e5d4c-…",
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

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `room_id` | UUID | |
| `session_id` | UUID | *Omitted when empty* (always, today) |
| `text` | string | 1–200 characters |
| `completed_at` | timestamp | *Omitted when empty* (not completed) |
| `created_at`, `updated_at` | timestamp | |
| `extended_count` | int | Extensions granted so far |
| `extra_minutes_used` | int | Sum of extension minutes |
| `timer_status` | `"idle"` \| `"running"` \| `"prompting"` \| `"ended"` | `prompting` = time is up, waiting for the extension answer |
| `timer_duration_minutes` | int | 25, 50 or 90; *omitted when empty* |
| `timer_started_at`, `timer_ends_at` | timestamp | The current run (after an extension: the extension's start and end); *omitted when empty* |
| `prompt_deadline_at` | timestamp | While `prompting`: when the prompt times out; *omitted when empty* |

### `POST /rooms/{id}/tasks`

| | |
|---|---|
| Auth | required. Being in the room is **not** required (only to start a timer) |

Request body:

| Field | Type | Required | Rules |
|---|---|---|---|
| `text` | string | yes | Trimmed, 1–200 characters |

`201 Created`: the task object, with `timer_status: "idle"`.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}`; `{"text": "REQUIRED"}`; `{"text": "TOO_LONG"}` |
| 404 | `NOT_FOUND` | No such room |

### `GET /tasks/me`

| | |
|---|---|
| Auth | required |

The caller's own tasks across all rooms, newest first.

| Param | Type | Default | Rules |
|---|---|---|---|
| `limit` | int | 50 | ≥ 1; values above 100 are capped to 100 |
| `offset` | int | 0 | ≥ 0; number of tasks to skip |

`200 OK`: a plain array of task objects (`[]` when none or past the end).
There is no total: a page shorter than `limit` is the last one. Tasks created
in the meantime shift offsets, so de-duplicate by `id`.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"limit": "INVALID_VALUE"}`; `{"offset": "INVALID_VALUE"}` |

### `PATCH /tasks/{id}`

| | |
|---|---|
| Auth | required, task owner only |

| Field | Type | Rules |
|---|---|---|
| `text` | string | Trimmed, 1–200 characters |
| `completed` | bool | `true` sets `completed_at` to now; `false` clears it |

Absent or `null` fields are unchanged; at least one must be present.
Completing a task whose timer is `running` or `prompting` ends the timer
(`timer_status: "ended"`), and the room gets `timer_ended` with reason
`completed`. Tasks can't be deleted.

`200 OK`: the task object.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}`; no field present ("no fields to update"); `{"text": "REQUIRED"}`; `{"text": "TOO_LONG"}` |
| 403 | `FORBIDDEN` | Not the owner |
| 404 | `NOT_FOUND` | No such task |

### `POST /tasks/{id}/start`

| | |
|---|---|
| Auth | required, task owner only, and currently in the task's room |

Request body:

| Field | Type | Required | Rules |
|---|---|---|---|
| `duration_minutes` | int | yes | `25`, `50` or `90` |

The task must not be completed, and the user can't have another timer that
is `running` or `prompting` (one active timer per user). A timer that has
`ended` can be started again.

`200 OK`: the task object (`timer_status: "running"`). The room gets
`timer_started`.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}`; `{"duration_minutes": "INVALID_VALUE"}` |
| 403 | `FORBIDDEN` | Not the owner; not in the task's room ("join the task's room before starting a timer") |
| 404 | `NOT_FOUND` | No such task |
| 409 | `CONFLICT` | Task completed; another timer of this user is active |

### `POST /tasks/{id}/extend`

| | |
|---|---|
| Auth | required, task owner only |

Answers the extension prompt with "more time". Only valid while
`timer_status` is `"prompting"` and before `prompt_deadline_at`.

| Field | Type | Required | Rules |
|---|---|---|---|
| `minutes` | int | yes | `15` or `30` |

`200 OK`: the task object (`timer_status: "running"`, `extended_count` + 1,
new `timer_ends_at` = now + `minutes`). The room gets `task_extended`.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}`; `{"minutes": "INVALID_VALUE"}` |
| 403 | `FORBIDDEN` | Not the owner |
| 404 | `NOT_FOUND` | No such task |
| 409 | `EXTEND_LIMIT_REACHED` | The task already has `EXTENSION_MAX_COUNT` extensions |
| 409 | `CONFLICT` | Not prompting; the prompt was already answered or has expired |

### `POST /tasks/{id}/no-extend`

| | |
|---|---|
| Auth | required, task owner only |
| Body | none |

Answers the prompt with "no". The timer ends and the user is **removed from
the room**, exactly as when the prompt times out: their sockets get `kicked`
and close with `4001`, the room gets `user_left`, and the participation closes
with `left_reason: "kicked_no_extension"`. They can join again.

`200 OK`: the task object (`timer_status: "ended"`).

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}` |
| 403 | `FORBIDDEN` | Not the owner |
| 404 | `NOT_FOUND` | No such task |
| 409 | `CONFLICT` | Not prompting; the prompt was already answered or has expired |

---

## Chat

Text only: no files, edits, deletes or read receipts. You must be **currently
in the room** to send or read.

Message object:

```json
{
  "id": "3c9e8d7f-…",
  "room_id": "0b6e5d4c-…",
  "user_id": "6f1c2a4e-…",
  "user_name": "Ada",
  "text": "Starting a 50-minute block",
  "created_at": "2026-10-22T14:03:11.482913Z"
}
```

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `room_id` | UUID | |
| `user_id` | UUID | Author |
| `user_name` | string | The author's **current** name (it is joined at read time) |
| `text` | string | 1–2000 characters |
| `created_at` | timestamp | |

### `POST /rooms/{id}/messages`

| | |
|---|---|
| Auth | required, in the room |
| Rate limit | `CHAT_RATE_LIMIT` (10) messages per user per `CHAT_RATE_WINDOW` (10 s), across all rooms |

| Field | Type | Required | Rules |
|---|---|---|---|
| `text` | string | yes | Trimmed, 1–2000 characters |

`201 Created`: the message object. The room gets `chat_message`, the sender's
own sockets included, so de-duplicate by `id`.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}`; `{"text": "REQUIRED"}`; `{"text": "TOO_LONG"}` |
| 403 | `FORBIDDEN` | Not in the room, or the room doesn't exist |
| 429 | `RATE_LIMITED` | Over the limit; `Retry-After` set. The message is not stored. Invalid messages fail with `400` first and don't count |

### `GET /rooms/{id}/messages`

| | |
|---|---|
| Auth | required, in the room |

History, newest first, cursor-paginated.

| Param | Type | Default | Rules |
|---|---|---|---|
| `limit` | int | 50 | ≥ 1; values above 100 are capped to 100 |
| `before` | UUID | — | A message id from this room; returns older messages |

`200 OK`:

```json
{
  "items": [ { "id": "3c9e8d7f-…", "room_id": "…", "user_id": "…", "user_name": "Ada", "text": "…", "created_at": "…" } ],
  "next_cursor": "a01b2c3d-…"
}
```

| Field | Type | Notes |
|---|---|---|
| `items` | message[] | Never `null` |
| `next_cursor` | UUID \| `null` | Pass as `before` for the next (older) page; `null` when there is nothing older |

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}`; `{"limit": "INVALID_VALUE"}`; `{"before": "INVALID_VALUE"}` (not a UUID, unknown id, or a message from another room) |
| 403 | `FORBIDDEN` | Not in the room |

---

## Calls

Audio and video run on LiveKit. The API only issues join tokens.

### `POST /rooms/{id}/call/token`

| | |
|---|---|
| Auth | required, in the room |
| Body | none |

`200 OK`:

```json
{ "url": "wss://livekit.example.com", "token": "eyJhbGciOiJIUzI1NiIs..." }
```

| Field | Type | Notes |
|---|---|---|
| `url` | string | LiveKit server URL |
| `token` | string | LiveKit JWT, valid 1 hour for joining. Joins the LiveKit room named by the room UUID, with identity = user id and name = user name; may publish and subscribe |

Pass both to `room.connect(url, token)`. Request a new token for every
connection attempt.

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}` |
| 403 | `FORBIDDEN` | Not in the room, or the room doesn't exist |
| 503 | `SERVICE_UNAVAILABLE` | LiveKit isn't configured on the server |

---

## Invites

Referral codes only. Registration doesn't require or consume them, and
checking a code doesn't mark it as used.

### `POST /invites`

| | |
|---|---|
| Auth | required |
| Body | none |

`201 Created`:

```json
{ "id": "…", "code": "a7Kp3XmQ9z", "created_by": "6f1c2a4e-…", "created_at": "2026-10-22T14:03:11.482913Z" }
```

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `code` | string | 10 characters from an alphabet without look-alikes (no `0 O 1 l I`) |
| `created_by` | UUID | |
| `created_at` | timestamp | |

No errors of its own.

### `GET /invites/{code}`

| | |
|---|---|
| Auth | none |

Always `200 OK`, for any code:

```json
{ "valid": false, "reason": "not_found" }
```

| Field | Type | Notes |
|---|---|---|
| `valid` | bool | |
| `reason` | `"not_found"` \| `"already_used"` | *Omitted when valid* |

---

## WebSocket

### `GET /rooms/{id}/ws`

| | |
|---|---|
| Auth | `access_token` cookie (fallback `?access_token=`), plus an open participation in the room |

The protocol, events and close codes are in [ws-protocol.md](ws-protocol.md).
A failed handshake is a plain HTTP response in the error format:

| Status | Code | When |
|---|---|---|
| 400 | `VALIDATION_ERROR` | `{"id": "INVALID_FORMAT"}` |
| 401 | `UNAUTHORIZED` / `TOKEN_EXPIRED` | No valid token / expired token |
| 403 | `FORBIDDEN` | `Origin` not in `WS_ALLOWED_ORIGINS`; not in the room |

---

## Timer flow

Event details are in [ws-protocol.md](ws-protocol.md#5-events).

1. Join the room (`POST /rooms/{id}/join`) and open its WebSocket.
2. `POST /tasks/{id}/start` → the room gets `timer_started`.
3. Time is up → the room gets `timer_ended` (reason `expired`), then the owner
   alone gets `task_extension_prompt` with the deadline and allowed minutes.
4. The owner answers before the deadline:
   - `POST /tasks/{id}/extend` → `task_extended`; the timer runs again and step
     3 repeats when it ends;
   - `POST /tasks/{id}/no-extend`, or no answer → the timer ends and the user
     is removed from the room (`kicked`, reason `kicked_no_extension`).
5. Completing the task (`PATCH /tasks/{id}` with `completed: true`) at any
   point ends the timer (`timer_ended`, reason `completed`).

| Constant | Value | Configurable |
|---|---|---|
| Focus durations | 25, 50, 90 min | no |
| Extension lengths | 15, 30 min | no |
| Max extensions per task | 3 | `EXTENSION_MAX_COUNT` |
| Prompt timeout | 60 s | `EXTENSION_PROMPT_TIMEOUT` |
| Active timers per user | 1 (`running` or `prompting`) | no |

At the limit the prompt is still sent, with `extensions_left: 0` and an empty
`allowed_minutes`. The only answers left are `no-extend` or letting the
prompt time out.

---

## Endpoint index

"Auth" means the `access_token` cookie or `Authorization: Bearer`.

| Method | Path | Auth |
|---|---|---|
| GET | `/health` | — |
| POST | `/auth/register` | — |
| POST | `/auth/login` | — |
| POST | `/auth/refresh` | refresh cookie |
| POST | `/auth/logout` | refresh cookie (optional) |
| GET | `/auth/me` | Auth |
| GET | `/auth/google` | — (browser navigation) |
| GET | `/auth/google/callback` | OAuth state cookie |
| GET | `/auth/github` | — (browser navigation) |
| GET | `/auth/github/callback` | OAuth state cookie |
| GET | `/profile` | Auth |
| PATCH | `/profile` | Auth |
| GET | `/rooms` | Auth |
| GET | `/rooms/{id}` | Auth |
| POST | `/rooms/{id}/join` | Auth |
| POST | `/rooms/{id}/leave` | Auth |
| GET | `/rooms/{id}/participants` | Auth + in room |
| POST | `/rooms/{id}/tasks` | Auth |
| GET | `/tasks/me` | Auth |
| PATCH | `/tasks/{id}` | Auth (owner) |
| POST | `/tasks/{id}/start` | Auth (owner, in room) |
| POST | `/tasks/{id}/extend` | Auth (owner) |
| POST | `/tasks/{id}/no-extend` | Auth (owner) |
| POST | `/rooms/{id}/messages` | Auth + in room |
| GET | `/rooms/{id}/messages` | Auth + in room |
| POST | `/rooms/{id}/call/token` | Auth + in room |
| POST | `/invites` | Auth |
| GET | `/invites/{code}` | — |
| GET | `/rooms/{id}/ws` | cookie or `?access_token=` + in room |
