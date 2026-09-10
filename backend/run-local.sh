#!/usr/bin/env bash

set -euo pipefail

backend_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
env_file="$backend_root/.env"

if [[ ! -f "$env_file" ]]; then
    printf 'Missing %s. Create it with: cp .env.example .env\n' "$env_file" >&2
    exit 1
fi

set -a
# shellcheck disable=SC1090
source "$env_file"
set +a

cd "$backend_root"
exec go run ./cmd/server
