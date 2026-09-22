# Rock Paper Money

A rock-paper-scissors game with a React frontend and a Go backend.

## Stack

- **Frontend:** React 19, Vite 8, JavaScript, CSS
- **Frontend tests:** Vitest 5, Testing Library
- **Backend:** Go 1.26, standard library HTTP server, pgx v5, PostgreSQL, Redis Pub/Sub
- **Backend tests:** Go testing package

## Prerequisites

- Go 1.26 or later
- Node.js 22 or later and npm
- PostgreSQL 14 or later and Redis 8 or later
- A Supabase project with email/password authentication configured

Configure an asymmetric JWT signing key (ES256 or RS256) in Supabase Auth. The
backend verifies access tokens through the project's JWKS endpoint and does not
accept the legacy shared-secret HS256 configuration. Set the Supabase Site URL
and allowed redirect URLs to the frontend origin so confirmation links return to
the application.

## Run locally

Start the PostgreSQL authority and disposable Redis revision transport:

```bash
docker compose up -d postgres redis
docker compose ps
```

For a containerized same-origin development stack, provide real Supabase browser
values and start the application profile. Only the frontend origin is published;
its `/api` proxy carries both HTTP and WebSocket upgrade traffic to the backend.

```bash
SUPABASE_URL=https://your-project-ref.supabase.co \
VITE_SUPABASE_URL=https://your-project-ref.supabase.co \
VITE_SUPABASE_PUBLISHABLE_KEY=your-publishable-key \
docker compose --profile application up
```

Create a separate test database once if you intend to run integration tests:

```bash
docker compose exec postgres createdb -U rock_paper_money rock_paper_money_test
```

Create the backend's local environment file, then use its startup script.
`DATABASE_URL`, `SUPABASE_URL`, `APP_ORIGIN`, and `REDIS_ADDR` are required.
`APP_ORIGIN` is the exact browser origin used for cookie REST and WebSocket
checks. `SUPABASE_JWT_AUDIENCE` defaults to `authenticated`. Schema migrations
are embedded in the binary and applied safely during startup. Structured logs
are appended to `LOG_FILE`, which defaults to the ignored local file
`backend/server.log` when started from `backend`.

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

Open <http://localhost:5173>. Vite proxies HTTP and WebSocket requests under
`/api` to the backend at <http://localhost:8080>, so the browser sees one origin.
Production MUST route `/api` and `/api/rooms/{code}/socket` through the same TLS
origin; the proxy must preserve WebSocket upgrades so `https` pages use `wss`.
Set `VITE_SUPABASE_URL` and `VITE_SUPABASE_PUBLISHABLE_KEY` in the frontend
environment. Vite must never receive `DATABASE_URL`, a database password, or a
Supabase service-role secret.

For hosted Supabase PostgreSQL, use the direct connection string as
`DATABASE_URL` with `sslmode=require`. The project-side Auth setup
(`public.users`, the `auth.users` synchronization trigger, and self-read RLS) is
configured separately in Supabase and is not duplicated by this repository's
migrations. Redis is never authoritative and must not contain room snapshots,
moves, credentials, or financial data.

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

`GET /api/health` is process liveness. `GET /api/ready` reports PostgreSQL and
realtime health separately. PostgreSQL failure reports `unavailable`; Redis
failure reports PostgreSQL as `authoritative` and realtime as
`redis_unavailable`, with HTTP 503 in both cases. REST commits remain valid when
Redis is unavailable, while outbox rows remain retryable and socket delivery is
degraded. `GET /metrics` exposes bounded Prometheus text metrics for active
sockets, snapshot latency, revision gaps, backpressure eviction, auth closes,
deadline processing, outbox attempts, Redis recovery, cleanup, and receipt
outcomes.

Every HTTP response includes a server-generated `X-Request-ID`, which also
appears in request and error logs. API errors use `status`, `code`, `message`,
and `meta { time, requestId }`. Allowlisted game-rule errors may also include
`rawError`; authentication, malformed-input, and internal errors never do.
Worker and socket logs contain bounded operational identifiers and outcomes;
they MUST NOT contain cookies, tokens, CSRF values, moves, or protected room
payloads.

Database latency logs use `SLOW_QUERY_THRESHOLD` (default `250ms`) and
`SLOW_OPERATION_THRESHOLD` (default `2s`); both accept positive Go duration
values such as `400ms` or `3s`. Every protected REST operation emits one
`database operation` summary correlated by `request_id` and stable `operation`.
Its `duration_ms`, `query_count`, `query_duration_ms`, `pool_wait_ms`,
`lock_query_duration_ms`, `phases_ms`, and bounded `pool_*` fields separate
accumulated database time from pool pressure and known lock statements. Only
queries and pool acquisitions above the query threshold (or ending in error)
emit detail records. Query detail uses a safe semantic name for known locks or
a non-reversible `query_<hash>` fingerprint; SQL and arguments are never logged.

For example, this safe summary says the operation spent 620 ms acquiring pool
connections and 1.45 s executing known lock statements:

```json
{"msg":"database operation","request_id":"7c…","operation":"submit_move","status":204,"duration_ms":2480,"slow":true,"query_count":18,"query_duration_ms":2210,"pool_wait_ms":620,"lock_query_duration_ms":1450,"phases_ms":{"authentication":210,"room_lock":1200,"aggregate_load":430,"domain_mutation":0.1,"persistence_wallet_settlement":510,"outbox_receipt":120,"commit":10}}
```

Query duration is client-observed elapsed time. A lock-labeled query includes
the complete statement, including acquisition/wait, network RTT, and server
execution; these logs cannot split those components further. `pool_wait_ms` is
the complete client-observed pgx acquire duration and can also include pgx
connection validation or ping work, not only queueing. Phase totals can overlap
query totals and should not be added together.

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

Run the complete release gate from the repository root:

```bash
go -C backend test ./...
go -C backend test -race ./internal/realtime/... ./internal/worker/... ./internal/operations/...
go -C backend vet ./...
npm --prefix frontend test
npm --prefix frontend run lint
npm --prefix frontend run build
docker compose config --quiet
```

## Runtime lifecycle

Each backend instance starts deadline recovery, outbox dispatch, session and
receipt cleanup, Redis subscriptions, and authenticated WebSocket handling.
`SIGINT` or `SIGTERM` first stops HTTP admission, closes request-owned sockets,
cancels worker loops, waits for workers to exit, and only then closes Redis and
PostgreSQL clients. Deadline and outbox claims are durable, so another instance
or a restarted process resumes pending work.

### Failure drills

1. Run two backend instances against one PostgreSQL database and Redis service.
2. Connect players through both instances and commit a REST mutation. Confirm
   both sockets converge to the PostgreSQL revision.
3. Stop Redis. Confirm REST commits still succeed, readiness reports
   `redis_unavailable`, and outbox rows remain pending.
4. Restart Redis. Confirm exact room subscriptions recover, active rooms reload
   from PostgreSQL, and pending outbox work drains without duplicate effects.
5. Revoke a live browser session. Confirm no protected frame follows detection
   and the socket closes within 60 seconds.
6. Exercise a slow/non-reading client. Confirm bounded snapshot coalescing and a
   backpressure/write-deadline close without increased business effects.
7. Stop a worker process with due deadlines and restart it alongside another
   instance. Confirm move/reconnect/deadline races produce one settlement and
   one immutable posting/history result.
8. Send `SIGTERM` under socket and worker load. Confirm the process exits within
   the shutdown bound and pending durable work is recovered by a healthy
   instance.

## Rollout and rollback

Roll out in authority order: back up immutable wallet, ledger, round history,
and settlement records; verify unresolved escrow is zero; apply migrations with
workers disabled; deploy session/seat/receipt paths; provision Redis; enable
deadline, outbox, and cleanup workers; enable same-origin WSS; then deploy the
browser bundle. Public SSE, room-token headers, REST presence heartbeats,
process-local expiry timers, and PostgreSQL room notifications are no longer
available.

To roll back, stop new WebSocket traffic first, then disable deadline, outbox,
Redis subscription, and cleanup workers before changing application routing.
Retain pending outbox rows and additive schemas when removal could lose durable
evidence. Never reverse wallet postings, ledger/history rows, permanent
settlement keys, or committed command results. Disposable room reset remains
guarded by the zero-escrow check.
