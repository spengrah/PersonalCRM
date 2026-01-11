#!/bin/bash
# Ensure PostgreSQL is available for tests (Docker if available, otherwise native).

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "🔍 Ensuring PostgreSQL is available for tests..."

if command -v docker >/dev/null 2>&1; then
    if docker info >/dev/null 2>&1; then
        echo "🐳 Docker is available; starting database via Docker Compose..."
        (cd "$PROJECT_ROOT/infra" && docker compose up -d)
        bash "$PROJECT_ROOT/scripts/sync-postgres-auth.sh"
        exit 0
    fi

    echo "⚠️  Docker is installed but not available; falling back to native PostgreSQL..."
else
    echo "⚠️  Docker is not installed; falling back to native PostgreSQL..."
fi

bash "$PROJECT_ROOT/scripts/start-postgres-native.sh"
