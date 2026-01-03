#!/bin/bash
# Start and configure PostgreSQL natively (without Docker)
# Use this when running inside a container where Docker is not available
#
# This script:
# 1. Starts the PostgreSQL cluster if not running
# 2. Creates the application user and database if they don't exist
# 3. Installs required extensions (uuid-ossp, vector)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Load environment variables from .env if present
if [ -f "$PROJECT_ROOT/.env" ]; then
    set -a
    source "$PROJECT_ROOT/.env"
    set +a
fi

# Set defaults
POSTGRES_USER="${POSTGRES_USER:-crm_user}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-crm_password}"
POSTGRES_DB="${POSTGRES_DB:-personal_crm}"
POSTGRES_PORT="${POSTGRES_PORT:-5432}"

echo "🐘 Setting up native PostgreSQL..."

# Check if PostgreSQL is installed
if ! command -v pg_ctlcluster &> /dev/null; then
    echo "❌ PostgreSQL is not installed. Install with: sudo apt install postgresql postgresql-16-pgvector"
    exit 1
fi

# Check cluster status and start if needed
CLUSTER_STATUS=$(pg_lsclusters -h | grep "^16.*main" | awk '{print $4}')

if [ "$CLUSTER_STATUS" != "online" ]; then
    echo "📦 Starting PostgreSQL cluster..."
    sudo pg_ctlcluster 16 main start

    # Wait for PostgreSQL to be ready
    echo "⏳ Waiting for PostgreSQL to be ready..."
    for i in {1..30}; do
        if pg_isready -h localhost -p "$POSTGRES_PORT" >/dev/null 2>&1; then
            break
        fi
        if [ $i -eq 30 ]; then
            echo "❌ PostgreSQL failed to start within 30 seconds"
            exit 1
        fi
        sleep 1
    done
fi

echo "✅ PostgreSQL is running"

# Check if user exists
USER_EXISTS=$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='$POSTGRES_USER';")

if [ "$USER_EXISTS" != "1" ]; then
    echo "👤 Creating user '$POSTGRES_USER'..."
    sudo -u postgres psql -c "CREATE USER $POSTGRES_USER WITH PASSWORD '$POSTGRES_PASSWORD';"
    sudo -u postgres psql -c "ALTER USER $POSTGRES_USER CREATEDB;"
else
    echo "✅ User '$POSTGRES_USER' already exists"
    # Update password to match .env
    echo "🔐 Updating password to match .env..."
    ESCAPED_PASSWORD="${POSTGRES_PASSWORD//\'/\'\'}"
    sudo -u postgres psql -c "ALTER USER $POSTGRES_USER WITH PASSWORD '$ESCAPED_PASSWORD';"
fi

# Check if database exists
DB_EXISTS=$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='$POSTGRES_DB';")

if [ "$DB_EXISTS" != "1" ]; then
    echo "📦 Creating database '$POSTGRES_DB'..."
    sudo -u postgres psql -c "CREATE DATABASE $POSTGRES_DB OWNER $POSTGRES_USER;"
else
    echo "✅ Database '$POSTGRES_DB' already exists"
fi

# Grant privileges
sudo -u postgres psql -c "GRANT ALL PRIVILEGES ON DATABASE $POSTGRES_DB TO $POSTGRES_USER;"

# Install required extensions
echo "🔌 Installing extensions..."
sudo -u postgres psql -d "$POSTGRES_DB" -c "CREATE EXTENSION IF NOT EXISTS \"uuid-ossp\";"
sudo -u postgres psql -d "$POSTGRES_DB" -c "CREATE EXTENSION IF NOT EXISTS vector;"

# Grant extension permissions to application user
sudo -u postgres psql -d "$POSTGRES_DB" -c "GRANT ALL ON SCHEMA public TO $POSTGRES_USER;"

# Verify extensions
echo "🔍 Verifying extensions..."
EXTENSIONS=$(sudo -u postgres psql -d "$POSTGRES_DB" -tAc "SELECT extname FROM pg_extension WHERE extname IN ('uuid-ossp', 'vector') ORDER BY extname;")
echo "   Installed extensions: $(echo $EXTENSIONS | tr '\n' ', ')"

# Test connection with application user
echo "🔐 Testing connection with application user..."
if PGPASSWORD="$POSTGRES_PASSWORD" psql -h localhost -p "$POSTGRES_PORT" -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT 1;" >/dev/null 2>&1; then
    echo "✅ Connection successful"
else
    echo "❌ Connection failed. Check pg_hba.conf for md5 authentication on localhost"
    echo "   You may need to add: host all all 127.0.0.1/32 md5"
    exit 1
fi

echo ""
echo "✅ PostgreSQL native setup complete!"
echo ""
echo "Connection details:"
echo "  Host:     localhost"
echo "  Port:     $POSTGRES_PORT"
echo "  Database: $POSTGRES_DB"
echo "  User:     $POSTGRES_USER"
echo ""
echo "DATABASE_URL=postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@localhost:$POSTGRES_PORT/$POSTGRES_DB?sslmode=disable"
