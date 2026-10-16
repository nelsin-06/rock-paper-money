#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: ./script-start.sh [--backend] [--frontend] [--redis] [--postgres]
       ./script-start.sh --help

Start development services in the background using Docker Compose.
Without service flags, start all four services with Compose dependency checks.
Combine service flags to start ONLY those services, without starting dependencies.

Options:
  --backend   Start the backend.
  --frontend  Start the frontend.
  --redis     Start Redis.
  --postgres  Start PostgreSQL.
  --help      Show this help without starting services.

Prerequisites:
  Docker with the Compose plugin and a running Docker daemon.
  Configure backend/.env and frontend/.env as required by compose.yaml,
  including Supabase values. Compose may require both files for any selection.
  The backend needs its configured PostgreSQL database and Redis available.
  The frontend needs the backend available for API requests.
  Selected-only startup does not start missing dependencies; start prerequisites
  separately or select them explicitly. Existing unselected services are left alone.
  Configuration, images, ports, and volumes come from the existing compose.yaml;
  selecting local PostgreSQL does not override the backend's DATABASE_URL.

Examples:
  ./script-start.sh
  ./script-start.sh --postgres --redis
  ./script-start.sh --backend --frontend
EOF
}

services=()
show_help=false
for argument in "$@"; do
    case "$argument" in
        --backend|--frontend|--redis|--postgres)
            services+=("${argument#--}")
            ;;
        --help) show_help=true ;;
        *)
            printf 'Error: invalid argument: %s\nUse --help for usage.\n' "$argument" >&2
            exit 2
            ;;
    esac
done

if "$show_help"; then
    usage
    exit 0
fi

options=()
if ((${#services[@]} == 0)); then
    services=(postgres redis backend frontend)
else
    options+=(--no-deps)
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec docker compose --file "$script_dir/compose.yaml" --profile application \
    up --detach "${options[@]}" "${services[@]}"
