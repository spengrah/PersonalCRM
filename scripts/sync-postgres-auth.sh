#!/bin/bash
# Automatic database password synchronization script
# Ensures the PostgreSQL database password matches the .env configuration
#
# How it works:
# 1. Tests password from OUTSIDE the container (like the backend does)
# 2. If mismatch, uses local trust auth INSIDE container to update password
# 3. Verifies the fix worked from outside

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Load environment variables from .env
if [ ! -f "$PROJECT_ROOT/.env" ]; then
    echo "⚠️  .env file not found. Skipping password sync."
    exit 0
fi

set -a
source "$PROJECT_ROOT/.env"
set +a

# Validate required variables
if [ -z "$POSTGRES_USER" ] || [ -z "$POSTGRES_PASSWORD" ] || [ -z "$POSTGRES_DB" ]; then
    echo "❌ Missing required POSTGRES variables in .env"
    exit 1
fi

POSTGRES_PORT=${POSTGRES_PORT:-5432}
CONTAINER_NAME="crm-postgres"

echo "🔍 Checking database password synchronization..."

# Check if container exists
if ! docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    echo "✅ Container doesn't exist yet - will be created with correct password"
    exit 0
fi

# Check if container is running
if ! docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    echo "⚠️  Container exists but is not running"
    echo "   Starting container..."
    docker start "$CONTAINER_NAME"
    sleep 2
fi

# Wait for database to be ready (max 30 seconds)
echo "⏳ Waiting for database to be ready..."
for i in {1..30}; do
    if docker exec "$CONTAINER_NAME" pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB" >/dev/null 2>&1; then
        break
    fi
    if [ $i -eq 30 ]; then
        echo "❌ Database failed to become ready within 30 seconds"
        exit 1
    fi
    sleep 1
done

# Test password from OUTSIDE the container (this is how the backend connects)
# We use a temporary postgres container to test the connection via TCP
# This bypasses the "trust" auth that applies to local connections inside the container
CONNECTION_URL="postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@host.docker.internal:${POSTGRES_PORT}/${POSTGRES_DB}"

echo "🔐 Testing password from external connection..."
if docker run --rm --add-host=host.docker.internal:host-gateway \
    postgres:16-alpine psql "$CONNECTION_URL" -c "SELECT 1;" >/dev/null 2>&1; then
    echo "✅ Database password is already synchronized"
    exit 0
fi

echo "🔧 Password mismatch detected - updating database password..."

# Update password using local trust authentication (inside container)
# This works because pg_hba.conf has "trust" for local connections
# Escape single quotes in password for SQL
ESCAPED_PASSWORD="${POSTGRES_PASSWORD//\'/\'\'}"

if docker exec "$CONTAINER_NAME" psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
    -c "ALTER USER $POSTGRES_USER WITH PASSWORD '$ESCAPED_PASSWORD';" >/dev/null 2>&1; then
    echo "✅ Password updated successfully"
else
    echo "❌ Failed to update password. You may need to:"
    echo "   1. Stop the container: docker stop $CONTAINER_NAME"
    echo "   2. Remove the volume: docker volume rm infra_postgres_data"
    echo "   3. Restart: make start-local"
    echo "   Note: This will DELETE all data!"
    exit 1
fi

# Verify the password change worked (from outside the container)
if docker run --rm --add-host=host.docker.internal:host-gateway \
    postgres:16-alpine psql "$CONNECTION_URL" -c "SELECT 1;" >/dev/null 2>&1; then
    echo "✅ Password verification successful"
    echo "✅ Database password synchronized successfully"
else
    echo "❌ Password update failed verification"
    exit 1
fi
