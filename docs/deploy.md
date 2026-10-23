# Deploying to a VPS

This runs the API with `docker-compose.prod.yml` on one Ubuntu server:

| Service | What it does | Exposed |
|---|---|---|
| `caddy` | Reverse proxy, automatic HTTPS (Let's Encrypt), WebSockets | ports 80, 443 (TCP + UDP) |
| `app` | The API (`Dockerfile`), listens on 8080 inside the network | no, only through Caddy |
| `migrate` | One-shot: runs `cmd/migrate up`, then exits; `app` waits for it | no |
| `postgres` | Postgres 16, data in the `postgres_data` volume | no |
| `redis` | Redis 7, data in the `redis_data` volume | no |

All configuration and secrets live in `.env.prod` on the server. It is
git-ignored and must never be committed.

**Migrations** run automatically on every `up`: the `migrate` service applies
pending goose migrations with the repo's `cmd/migrate`, and `app` starts only
if that succeeds. The same binary takes `status`, `version` and `down` (see
[Updating](#updating)).

In the commands below, `$DC` is short for:

```sh
DC="docker compose -f docker-compose.prod.yml --env-file .env.prod"
```

## 1. Prepare the server

Fresh Ubuntu 22.04 or 24.04, logged in as a sudo user.

```sh
# Docker Engine + Compose plugin (official repository)
sudo apt-get update
sudo apt-get install -y ca-certificates curl git
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo usermod -aG docker "$USER"   # log out and back in afterwards

# Firewall: SSH, HTTP (ACME challenge + redirect), HTTPS and HTTP/3
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 443/udp
sudo ufw enable
```

Check with `docker version` and `docker compose version`.

## 2. Point DNS at the server

Create an `A` record (and an `AAAA` record if the server has IPv6) for the API
hostname, e.g. `api.example.com`, pointing at the server's public IP. Wait until
`dig +short api.example.com` returns that IP. Caddy can only get a certificate
once DNS resolves and ports 80/443 are reachable.

## 3. Clone and configure

```sh
git clone https://github.com/WorkBuddy-org/Backend.git workbuddy
cd workbuddy
cp .env.prod.example .env.prod
chmod 600 .env.prod
nano .env.prod
```

Fill in every value marked **REQUIRED** (see the [checklist](#oauth-and-livekit-checklist)
below for OAuth and LiveKit):

- `DOMAIN`: the API hostname from step 2.
- `POSTGRES_PASSWORD`: generate one with `openssl rand -hex 24`, and put the
  **same** password into `DATABASE_URL`.
- `JWT_SECRET`: `openssl rand -hex 32`.
- `ENV=production` (leave as is).
- `GOOGLE_*`, `GITHUB_*`, `FRONTEND_CALLBACK_URL`, `WS_ALLOWED_ORIGINS`,
  `LIVEKIT_*`.

`POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` only take effect the
first time the `postgres_data` volume is created. Changing them later doesn't
change the existing database.

## 4. Start

```sh
$DC up -d --build
$DC ps                       # app "healthy", migrate "exited (0)", the rest "running"
curl https://api.example.com/health
# {"status":"ok","checks":{"postgres":"ok","redis":"ok"}}
```

The first request can take a few seconds while Caddy obtains the certificate.

## Updating

```sh
cd workbuddy
git pull
$DC up -d --build            # rebuilds the image, runs new migrations, restarts the app
docker image prune -f        # optional: remove old images
```

Before an update that contains migrations, take a [backup](#backups).
Migration commands:

```sh
$DC run --rm migrate status  # applied / pending migrations
$DC run --rm migrate         # apply pending ones (same as on every up)
$DC run --rm migrate down    # roll back the last one
```

## Logs

```sh
$DC logs -f app              # API logs
$DC logs -f caddy            # access log (JSON) and certificate events
$DC logs --tail 100 migrate  # last migration run
$DC ps                       # health/status of every service
```

The Caddy access log strips the `access_token`, `code` and `state` query
parameters and the `Location` response header (it carries tokens after OAuth
login). Caddy itself redacts `Authorization` and `Cookie`.

## Backups

Postgres holds all durable data. Redis only holds rate-limit counters,
presence and the timer index, which the app rebuilds from Postgres on start.

```sh
# Dump (custom format, compressed) to the current directory
$DC exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' > "workbuddy-$(date +%F-%H%M).dump"

# Restore into the running database (replaces objects that exist in the dump)
$DC stop app
$DC exec -T postgres sh -c 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists' < workbuddy-YYYY-MM-DD-HHMM.dump
$DC start app
```

Copy dumps off the server (e.g. `scp` or object storage) and test a restore
now and then. Daily cron example (`crontab -e`, after `mkdir backups`):

```
0 3 * * * cd /home/deploy/workbuddy && docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' > backups/workbuddy-$(date +\%F).dump
```

## OAuth and LiveKit checklist

Below, `https://api.example.com` is your `DOMAIN` and `https://app.example.com`
is the frontend.

### Google

1. Google Cloud Console → create or select a project → **APIs & Services → OAuth
   consent screen**: set it up as External, with app name, support email and
   the authorized domain `example.com`. Scopes: `openid`, `email`, `profile`.
   Publish the app (or add test users while it is in testing).
2. **Credentials → Create credentials → OAuth client ID**, type **Web
   application**.
3. **Authorized redirect URIs**: `https://api.example.com/auth/google/callback`.
4. Copy the values into `.env.prod`:
   - Client ID → `GOOGLE_CLIENT_ID`
   - Client secret → `GOOGLE_CLIENT_SECRET`
   - `GOOGLE_REDIRECT_URL=https://api.example.com/auth/google/callback`
     (must match the registered URI exactly)

### GitHub

1. GitHub → **Settings → Developer settings → OAuth Apps → New OAuth App**
   (under the organization's settings if the org should own it).
2. **Homepage URL**: `https://app.example.com`.
3. **Authorization callback URL**: `https://api.example.com/auth/github/callback`.
4. Register, then **Generate a new client secret**.
5. Copy the values into `.env.prod`:
   - Client ID → `GITHUB_CLIENT_ID`
   - Client secret → `GITHUB_CLIENT_SECRET`
   - `GITHUB_REDIRECT_URL=https://api.example.com/auth/github/callback`

### Frontend settings

- `FRONTEND_CALLBACK_URL`: the frontend page that receives
  `?access_token=…&refresh_token=…&expires_in=…` after OAuth login, e.g.
  `https://app.example.com/auth/callback`.
- `WS_ALLOWED_ORIGINS`: the frontend origin(s), comma-separated, e.g.
  `https://app.example.com`. With `ENV=production`, an empty value rejects
  every browser WebSocket.

### LiveKit Cloud

1. Sign in at cloud.livekit.io → **Create project**.
2. Project **Settings → API Keys → Create key**.
3. Copy the values into `.env.prod`:
   - Project URL (`wss://<project>.livekit.cloud`) → `LIVEKIT_URL`
   - API key → `LIVEKIT_API_KEY`
   - API secret → `LIVEKIT_API_SECRET` (shown once; it never leaves the server)
4. No redirect URLs are needed. If any `LIVEKIT_*` is empty, the API still
   starts but `POST /rooms/{id}/call/token` returns 503.

After editing `.env.prod`, apply it with `$DC up -d`, which recreates the
containers whose configuration changed.
