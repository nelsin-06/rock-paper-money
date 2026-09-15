# Rock Paper Money

A rock-paper-scissors game with a React frontend and a Go backend.

## Stack

- **Frontend:** React 19, Vite 8, JavaScript, CSS
- **Frontend tests:** Vitest 5, Testing Library
- **Backend:** Go 1.26, standard library HTTP server, pgx v5, PostgreSQL
- **Backend tests:** Go testing package

## Prerequisites

- Go 1.26 or later
- Node.js 22 or later and npm
- PostgreSQL 14 or later
- A Supabase project with email/password authentication configured

Configure an asymmetric JWT signing key (ES256 or RS256) in Supabase Auth. The
backend verifies access tokens through the project's JWKS endpoint and does not
accept the legacy shared-secret HS256 configuration. Set the Supabase Site URL
and allowed redirect URLs to the frontend origin so confirmation links return to
the application.

## Run locally

Start PostgreSQL with your local installation, or use the optional Compose service:

```bash
docker compose up -d postgres
```

Create a separate test database once if you intend to run integration tests:

```bash
docker compose exec postgres createdb -U rock_paper_money rock_paper_money_test
```

Create the backend's local environment file, then use its startup script.
`DATABASE_URL` and `SUPABASE_URL` are required. `SUPABASE_JWT_AUDIENCE` defaults
to `authenticated`. Schema migrations are embedded in the binary and applied
safely during startup. Structured logs are appended to `LOG_FILE`, which defaults
to the ignored local file `backend/server.log` when started from `backend`.

```bash
cd backend
cp .env.example .env
./run-local.sh
```

In another terminal, start the frontend:

```bash
cd frontend
npm ci
cp .env.example .env.local
npm run dev
```

Open <http://localhost:5173>. The backend listens on <http://localhost:8080>.
Set `VITE_SUPABASE_URL` and `VITE_SUPABASE_PUBLISHABLE_KEY` in the frontend
environment. Vite must never receive `DATABASE_URL`, a database password, or a
Supabase service-role secret.

For hosted Supabase PostgreSQL, use the direct connection string as
`DATABASE_URL` with `sslmode=require`. A persistent direct connection supports
the backend's `LISTEN/NOTIFY` listener. The project-side Auth setup (`public.users`,
the `auth.users` synchronization trigger, and self-read RLS) is configured
separately in Supabase and is not duplicated by this repository's migrations.

## Tests

Run the backend tests:

```bash
cd backend
go test ./...
```

PostgreSQL integration tests skip automatically when `TEST_DATABASE_URL` is
unset or when `-short` is used. Run them explicitly with:

```bash
cd backend
TEST_DATABASE_URL='postgres://rock_paper_money:rock_paper_money@localhost:5432/rock_paper_money_test?sslmode=disable' go test ./internal/room/adapter/postgres
```

`GET /api/health` is process liveness. `GET /api/ready` checks PostgreSQL and
the required notification listener, returning `503` when the backend cannot
serve traffic consistently.

Every HTTP response includes a server-generated `X-Request-ID`, which also
appears in request and error logs. API errors use `status`, `code`, `message`,
and `meta { time, requestId }`. Allowlisted game-rule errors may also include
`rawError`; authentication, malformed-input, and internal errors never do.

Authenticated accounts receive a zero-balance coin wallet lazily on first use.
`GET /api/wallet` returns the caller's balance, and `POST /api/wallet/recharges`
adds a positive whole-coin amount to that same wallet. Recharge requests require
a unique `Idempotency-Key` header so retries cannot credit twice. Each player
stakes 50 coins per funded round. Wins and forfeits pay 75 coins to the winner
and 25 to the house; draws refund both stakes. `GET /api/analytics/rounds` is
read-only and available to every authenticated account.

Run the frontend tests:

```bash
cd frontend
npm test
```
