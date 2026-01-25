#!/bin/bash
# Setup development environment with all required dependencies
#
# This script installs and configures:
# 1. Go tools (golangci-lint, sqlc)
# 2. PostgreSQL 16 with pgvector extension
# 3. Frontend dependencies (bun install)
# 4. Playwright browsers
# 5. Git hooks
#
# Run this once when setting up a new development environment.

set -e
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo_step() { echo -e "${BLUE}==>${NC} $1"; }
echo_ok() { echo -e "${GREEN}✓${NC} $1"; }
echo_warn() { echo -e "${YELLOW}⚠${NC} $1"; }
echo_err() { echo -e "${RED}✗${NC} $1"; }

# Get required Go version from go.mod
get_required_go_version() {
    if [ -f "backend/go.mod" ]; then
        grep "^go " backend/go.mod | awk '{print $2}' | head -1
    else
        echo "1.24"  # fallback
    fi
}

REQUIRED_GO_VERSION=$(get_required_go_version)
GO_AVAILABLE=false

echo ""
echo "========================================"
echo "  Personal CRM - Dev Environment Setup"
echo "========================================"
echo ""

# Track what needs manual intervention
MANUAL_STEPS=()

#############################################
# 1. Check Go
#############################################
echo_step "Checking Go installation..."

if command -v go &> /dev/null; then
    GO_VERSION=$(go version | awk '{print $3}')
    echo_ok "Go installed: $GO_VERSION"
    GO_AVAILABLE=true
else
    echo_err "Go is not installed"
    MANUAL_STEPS+=("Install Go ${REQUIRED_GO_VERSION}+: https://go.dev/doc/install")
fi

#############################################
# 2. Install Go tools
#############################################
echo_step "Installing Go tools..."

GOPATH="${GOPATH:-$HOME/go}"
mkdir -p "$GOPATH/bin"

# golangci-lint v2 (installed via curl, not go install, for version compatibility)
if command -v golangci-lint &> /dev/null || [ -f "$GOPATH/bin/golangci-lint" ]; then
    LINT_VERSION=$("$GOPATH/bin/golangci-lint" --version 2>/dev/null | head -1 || golangci-lint --version 2>/dev/null | head -1)
    if [[ "$LINT_VERSION" == *"version 2"* ]]; then
        echo_ok "golangci-lint v2 already installed"
    else
        echo_warn "golangci-lint v1 found, upgrading to v2..."
        if curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b "$GOPATH/bin" latest; then
            echo_ok "golangci-lint v2 installed"
        else
            echo_err "Failed to install golangci-lint"
            MANUAL_STEPS+=("Install golangci-lint: https://golangci-lint.run/welcome/install/")
        fi
    fi
else
    echo "   Installing golangci-lint v2..."
    if curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b "$GOPATH/bin" latest; then
        echo_ok "golangci-lint v2 installed"
    else
        echo_err "Failed to install golangci-lint"
        MANUAL_STEPS+=("Install golangci-lint: https://golangci-lint.run/welcome/install/")
    fi
fi

# sqlc (requires Go)
if command -v sqlc &> /dev/null || [ -f "$GOPATH/bin/sqlc" ]; then
    echo_ok "sqlc already installed"
elif [ "$GO_AVAILABLE" = true ]; then
    echo "   Installing sqlc..."
    if go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest; then
        echo_ok "sqlc installed"
    else
        echo_err "Failed to install sqlc"
        MANUAL_STEPS+=("Install sqlc: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest")
    fi
else
    echo_warn "Skipped sqlc (Go not available)"
    MANUAL_STEPS+=("After installing Go, run: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest")
fi

#############################################
# 3. Check/Install PostgreSQL
#############################################
echo_step "Checking PostgreSQL..."

if [[ "$OSTYPE" == "darwin"* ]]; then
    # macOS - check via Homebrew
    if command -v psql &> /dev/null; then
        PG_VERSION=$(psql --version | awk '{print $3}' | cut -d. -f1)
        echo_ok "PostgreSQL $PG_VERSION installed"

        # Check for pgvector via Homebrew
        if brew list pgvector &> /dev/null 2>&1; then
            echo_ok "pgvector extension installed"
        else
            echo_warn "pgvector extension not found"
            MANUAL_STEPS+=("Install pgvector: brew install pgvector")
        fi
    else
        echo_warn "PostgreSQL not found"
        MANUAL_STEPS+=("Install PostgreSQL: brew install postgresql@16 pgvector")
    fi
else
    # Linux - use Debian/Ubuntu detection
    if command -v pg_ctlcluster &> /dev/null; then
        PG_VERSION=$(pg_lsclusters -h 2>/dev/null | grep -E "^[0-9]+" | head -1 | awk '{print $1}')
        echo_ok "PostgreSQL $PG_VERSION installed"

        # Check for pgvector
        if dpkg -l | grep -q "postgresql-${PG_VERSION}-pgvector"; then
            echo_ok "pgvector extension installed"
        else
            echo_warn "pgvector extension not found"
            MANUAL_STEPS+=("Install pgvector: sudo apt install postgresql-${PG_VERSION}-pgvector")
        fi
    elif command -v psql &> /dev/null; then
        echo_ok "PostgreSQL client installed (server may be external)"
    else
        echo_warn "PostgreSQL not found"
        MANUAL_STEPS+=("Install PostgreSQL: sudo apt install postgresql postgresql-16-pgvector")
    fi
fi

#############################################
# 4. Check Bun
#############################################
echo_step "Checking Bun..."

if command -v bun &> /dev/null; then
    BUN_VERSION=$(bun --version)
    echo_ok "Bun installed: v$BUN_VERSION"
else
    echo_err "Bun is not installed"
    MANUAL_STEPS+=("Install Bun: curl -fsSL https://bun.sh/install | bash")
fi

#############################################
# 5. Install frontend dependencies
#############################################
echo_step "Installing frontend dependencies..."

if [ -d "frontend" ] && command -v bun &> /dev/null; then
    cd frontend
    bun install --silent
    echo_ok "Frontend dependencies installed"
    cd "$PROJECT_ROOT"
else
    echo_warn "Skipped frontend deps (bun not available or frontend dir missing)"
fi

#############################################
# 6. Install Playwright browsers
#############################################
echo_step "Installing Playwright browsers..."

if [ -d "frontend" ] && command -v bun &> /dev/null; then
    cd frontend
    if bunx playwright --version &> /dev/null; then
        # Check if browsers are installed by trying to run a simple check
        if bunx playwright install --dry-run 2>&1 | grep -q "already installed"; then
            echo_ok "Playwright browsers already installed"
        else
            bunx playwright install chromium --with-deps 2>/dev/null || bunx playwright install chromium
            echo_ok "Playwright browsers installed"
        fi
    else
        echo_warn "Playwright not available yet"
    fi
    cd "$PROJECT_ROOT"
else
    echo_warn "Skipped Playwright (bun not available)"
fi

#############################################
# 7. Install git hooks
#############################################
echo_step "Installing git hooks..."

if [ -f "scripts/install-git-hooks.sh" ]; then
    bash scripts/install-git-hooks.sh
    echo_ok "Git hooks installed"
else
    echo_warn "Git hooks script not found"
fi

#############################################
# 8. Download Go modules
#############################################
echo_step "Downloading Go modules..."

if [ -d "backend" ] && [ "$GO_AVAILABLE" = true ]; then
    cd backend
    go mod download
    echo_ok "Go modules downloaded"
    cd "$PROJECT_ROOT"
else
    echo_warn "Skipped Go modules (go not available or backend dir missing)"
fi

#############################################
# 9. Create .env if missing
#############################################
echo_step "Checking .env file..."

if [ -f ".env" ]; then
    echo_ok ".env file exists"
else
    if [ -f ".env.example" ]; then
        cp .env.example .env
        echo_ok ".env created from .env.example (review and update values)"
    else
        echo_warn "No .env file (create from .env.example)"
    fi
fi

#############################################
# Summary
#############################################
echo ""
echo "========================================"
echo "  Setup Complete"
echo "========================================"
echo ""

if [ ${#MANUAL_STEPS[@]} -gt 0 ]; then
    echo -e "${YELLOW}Manual steps required:${NC}"
    for step in "${MANUAL_STEPS[@]}"; do
        echo "  • $step"
    done
    echo ""
fi

echo "Next steps:"
echo "  1. Review .env and update any values"
echo "  2. Start PostgreSQL: make postgres-native"
echo "  3. Start dev servers: make dev-native"
echo ""
echo "Or if using Docker:"
echo "  2. Start services: make docker-up"
echo "  3. Start dev servers: make dev"
echo ""
