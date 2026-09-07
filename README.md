# Rock Paper Money

A rock-paper-scissors game with a React frontend and a Go backend.

## Stack

- **Frontend:** React 19, Vite 8, JavaScript, CSS
- **Frontend tests:** Vitest 5, Testing Library
- **Backend:** Go 1.26, standard library HTTP server
- **Backend tests:** Go testing package

## Prerequisites

- Go 1.26 or later
- Node.js and npm

## Run locally

Start the backend from the project root:

```bash
cd backend
go run ./cmd/server
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

Run the frontend tests:

```bash
cd frontend
npm test
```
