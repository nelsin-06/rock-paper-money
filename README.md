# Rock Paper Money

A rock-paper-scissors game with a React frontend and a Go backend.

## Stack

- **Frontend:** React 19, Vite 8, JavaScript, CSS
- **Frontend tests:** Vitest 5, Testing Library
- **Backend:** Go 1.26, standard library HTTP server, pgx v5, PostgreSQL
- **Backend tests:** Go testing package

## Prerequisites

- Go 1.26 or later
- Node.js and npm
- PostgreSQL 14 or later

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
`DATABASE_URL` is required; schema migrations are embedded in the binary and
applied safely during startup.

```bash
cd backend
cp .env.example .env
./run-local.sh
```

In another terminal, start the frontend:

```bash
cd frontend
npm ci
npm run dev
```

Open <http://localhost:5173>. The backend listens on <http://localhost:8080>.

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

Run the frontend tests:

```bash
cd frontend
npm test
```
