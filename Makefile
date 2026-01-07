# Personal CRM Makefile

.PHONY: help setup dev build test clean docker-up docker-down docker-reset test-cadence-ultra test-cadence-fast prod staging testing start start-local stop restart reload status dev-stop dev-restart dev-api-stop dev-api-start dev-api-restart ci-build-backend ci-build-frontend ci-build ci-test test-e2e deploy setup-pi dev-native postgres-native sqlc

# Go build cache (workspace-local by default; override via env).
GOCACHE ?= $(CURDIR)/.gocache
export GOCACHE

# Default target
help:
	@echo "Available targets:"
	@echo ""
	@echo "🔧 Setup:"
	@echo "  setup       - Setup development environment (install deps + git hooks)"
	@echo ""
	@echo "🚀 Production Commands:"
	@echo "  start       - Start Personal CRM (production mode on port 3001)"
	@echo "  start-local - Start with .env.local (preserves your production secrets)"
	@echo "  stop        - Stop Personal CRM"
	@echo "  restart     - Restart Personal CRM (full stop/start)"
	@echo "  reload      - Rebuild and restart apps (keeps database running)"
	@echo "  status      - Check CRM status"
	@echo ""
	@echo "Environment Management:"
	@echo "  testing     - Switch to testing environment (ultra-fast cadences)"
	@echo "  staging     - Switch to staging environment (fast cadences)" 
	@echo "  prod        - Switch to production environment (real cadences)"
	@echo ""
	@echo "Development:"
	@echo "  dev         - Start development servers (uses Docker for PostgreSQL)"
	@echo "  dev-native  - Start dev servers with native PostgreSQL (no Docker)"
	@echo "  build       - Build both frontend and backend"
	@echo "  sqlc        - Regenerate sqlc code from SQL queries"
	@echo "  test        - Run all tests (backend + frontend)"
	@echo "  clean       - Clean build artifacts"
	@echo ""
	@echo "Docker:"
	@echo "  docker-up   - Start Docker Compose services"
	@echo "  docker-down - Stop Docker Compose services"
	@echo "  docker-reset- Reset Docker volumes and restart"
	@echo ""
	@echo "Cadence Testing:"
	@echo "  test-cadence-ultra - Test all cadences in minutes (testing env)"
	@echo "  test-cadence-fast  - Test all cadences in hours (staging env)"
	@echo ""
	@echo "Raspberry Pi Deployment:"
	@echo "  setup-pi - One-time Pi setup (create user, directories)"
	@echo "  deploy   - Build and deploy to Pi (requires setup-pi first)"

# Setup development environment
setup:
	@echo "Setting up development environment..."
	@echo ""
	@echo "→ Installing frontend dependencies..."
	@cd frontend && bun install
	@echo "✓ Frontend dependencies installed"
	@echo ""
	@echo "→ Installing git hooks..."
	@./scripts/install-git-hooks.sh
	@echo ""
	@echo "✓ Development environment setup complete!"
	@echo ""
	@echo "Next steps:"
	@echo "  1. Copy .env.example to .env and configure"
	@echo "  2. Start database: make docker-up"
	@echo "  3. Start dev server: make dev"

# Create logs directory
logs:
	@mkdir -p logs

# Development
dev:
	@echo "Starting development environment..."
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting backend server..."
	@bash scripts/start-backend.sh
	@echo "✅ Backend server started (logs: logs/backend-dev.log, PID: $$(cat logs/backend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo "Starting frontend development server..."
	@bash scripts/start-frontend-dev.sh
	@echo "✅ Frontend dev server started (logs: logs/frontend-dev.log, PID: $$(cat logs/frontend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo ""
	@echo "🌐 Frontend: http://localhost:3000"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo ""
	@echo "💡 Both servers are running detached and will continue after you close this terminal"
	@echo "   Use 'make dev-stop' to stop both servers"
	@echo ""
	@echo "📋 To view logs:"
	@echo "   tail -f logs/backend-dev.log"
	@echo "   tail -f logs/frontend-dev.log"
	@echo ""
	@echo "Press Ctrl+C to exit (servers will keep running)"
	@tail -f logs/frontend-dev.log logs/backend-dev.log 2>/dev/null || sleep infinity

# Development helpers
dev-stop:
	@echo "Stopping development servers (backend and frontend dev)..."
	@# Kill backend by port (go run creates binary named 'main', not 'crm-api')
	@lsof -ti:8080 | xargs kill -9 2>/dev/null || true
	@pkill -f "next dev" || true
	@pkill -f "node.*next" || true
	@if [ -f logs/frontend-dev.pid ]; then kill $$(cat logs/frontend-dev.pid) 2>/dev/null || true; fi
	@if [ -f logs/backend-dev.pid ]; then kill $$(cat logs/backend-dev.pid) 2>/dev/null || true; fi
	@echo "✅ Dev servers stopped (if they were running)"

dev-restart:
	@echo "🔄 Restarting development environment..."
	@make dev-stop
	@sleep 1
	@make dev

dev-api-stop:
	@echo "Stopping backend dev server..."
	@pkill -f crm-api || true
	@# Wait briefly for port 8080 to be released
	@for i in 1 2 3 4 5; do \
	  if lsof -ti tcp:8080 >/dev/null 2>&1; then \
	    sleep 0.4; \
	  else \
	    break; \
	  fi; \
	done
	@echo "✅ Backend dev server stopped (if it was running) and port freed"

dev-api-start:
	@echo "Starting backend dev server..."
	@make docker-up
	@make logs
	@bash scripts/start-backend.sh
	@echo "✅ Backend dev server started (logs: logs/backend-dev.log, PID: $$(cat logs/backend-dev.pid 2>/dev/null || echo 'unknown'))"

dev-api-restart:
	@echo "🔄 Restarting backend dev server..."
	@make dev-api-stop
	@sleep 1
	@make dev-api-start

# Native PostgreSQL (for containerized development without Docker-in-Docker)
postgres-native:
	@bash scripts/start-postgres-native.sh

# Development with native PostgreSQL (no Docker required)
# Use this when running inside a container where Docker is not available
dev-native: postgres-native
	@echo "Starting development environment (native PostgreSQL)..."
	@make logs
	@echo "Starting backend server..."
	@bash scripts/start-backend.sh
	@echo "✅ Backend server started (logs: logs/backend-dev.log, PID: $$(cat logs/backend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo "Starting frontend development server..."
	@bash scripts/start-frontend-dev.sh
	@echo "✅ Frontend dev server started (logs: logs/frontend-dev.log, PID: $$(cat logs/frontend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo ""
	@echo "🌐 Frontend: http://localhost:3000"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo ""
	@echo "💡 Both servers are running detached and will continue after you close this terminal"
	@echo "   Use 'make dev-stop' to stop both servers"
	@echo ""
	@echo "📋 To view logs:"
	@echo "   tail -f logs/backend-dev.log"
	@echo "   tail -f logs/frontend-dev.log"
	@echo ""
	@echo "Press Ctrl+C to exit (servers will keep running)"
	@tail -f logs/frontend-dev.log logs/backend-dev.log 2>/dev/null || sleep infinity

test-e2e: docker-up
	@echo "Running Playwright E2E tests..."
	@ENV_FILE=$${ENV_FILE:-.env.example.testing}; \
	set -a; . "$$ENV_FILE"; set +a; \
	bash scripts/sync-postgres-auth.sh && \
	if [ -f frontend/.env.local ]; then mv frontend/.env.local frontend/.env.local.bak; fi; \
	echo "NEXT_PUBLIC_API_KEY=$$API_KEY" > frontend/.env.local; \
	echo "NEXT_PUBLIC_API_URL=http://localhost:8080" >> frontend/.env.local; \
	cd frontend && NEXT_PUBLIC_API_KEY=$$API_KEY NEXT_PUBLIC_API_URL=http://localhost:8080 bunx playwright test --project=chromium; \
	EXIT_CODE=$$?; \
	rm -f frontend/.env.local; \
	if [ -f frontend/.env.local.bak ]; then mv frontend/.env.local.bak frontend/.env.local; fi; \
	exit $$EXIT_CODE

# Build
build:
	@echo "Building backend..."
	@cd backend && go build -o bin/crm-api cmd/crm-api/main.go
	@echo "Building frontend..."
	@cd frontend && bun run build

# Tests
test: test-unit test-integration test-frontend

test-unit:
	@echo "Running backend unit tests..."
	@cd backend && go test ./tests/... -v -short

test-integration:
	@echo "Running backend integration tests..."
	@cd backend && go test ./tests/... -v

test-frontend:
	@echo "Running frontend tests..."
	@cd frontend && bun run test

test-api:
	@echo "Running API tests..."
	@cd backend && go test ./tests/... -v

# CI/CD targets
ci-build-backend:
	@echo "Building backend for ARM64..."
	@cd backend && GOOS=linux GOARCH=arm64 go build -o bin/crm-api cmd/crm-api/main.go

ci-build-frontend:
	@echo "Building frontend..."
	@cd frontend && bun run build

ci-build: ci-build-backend ci-build-frontend

# Linting
GOLANGCI_LINT := $(shell which golangci-lint 2>/dev/null || echo $$(go env GOPATH)/bin/golangci-lint)

lint:
	@echo "Running golangci-lint..."
	@cd backend && $(GOLANGCI_LINT) run ./...

lint-fix:
	@echo "Running golangci-lint with auto-fix..."
	@cd backend && $(GOLANGCI_LINT) run --fix ./...

ci-test: lint test-unit test-integration test-frontend
	@echo "✅ All CI tests passed"

# Code generation
sqlc:
	@echo "Generating sqlc code from SQL queries..."
	@cd backend && ~/go/bin/sqlc generate
	@echo "✅ sqlc code generated"

# API specific commands
api-docs:
	@echo "Generating API documentation..."
	@cd backend && ~/go/bin/swag init -g cmd/crm-api/main.go --output ./docs

api-build:
	@echo "Building API server..."
	@cd backend && go build -o bin/crm-api cmd/crm-api/main.go

api-run: api-build
	@echo "Starting API server..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && ./backend/bin/crm-api

# Environment switching
testing:
	@echo "Switching to TESTING environment (ultra-fast cadences)..."
	@cp .env.example.testing .env
	@echo "✅ Testing environment active:"
	@echo "   - Weekly cadence: 2 minutes"
	@echo "   - Monthly cadence: 10 minutes"
	@echo "   - Quarterly cadence: 30 minutes"
	@echo "   - Reminder scheduler: every 30 seconds"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "Use 'make test-cadence-ultra' to validate all cadences quickly"

staging:
	@echo "Switching to STAGING environment (fast cadences)..."
	@cp .env.example.staging .env
	@echo "✅ Staging environment active:"
	@echo "   - Weekly cadence: 10 minutes (1 week = 10 min)"
	@echo "   - Monthly cadence: 1 hour (1 month = 1 hour)"
	@echo "   - Quarterly cadence: 3 hours (1 quarter = 3 hours)"
	@echo "   - Reminder scheduler: every 5 minutes"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "Use 'make test-cadence-fast' to validate cadences in hours"

prod:
	@echo "Switching to PRODUCTION environment (real cadences)..."
	@cp .env.example.production .env
	@echo "✅ Production environment active:"
	@echo "   - Weekly cadence: 7 days"
	@echo "   - Monthly cadence: 30 days"
	@echo "   - Quarterly cadence: 90 days"
	@echo "   - Reminder scheduler: daily at 8 AM"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "⚠️  CAUTION: Real-world timing active"

# Cadence testing commands
test-cadence-ultra:
	@echo "🚀 Starting ULTRA-FAST cadence testing..."
	@echo "This will test all reminder cadences in minutes!"
	@echo ""
	@make testing
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting backend with ultra-fast cadences..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && cd backend && nohup go run cmd/crm-api/main.go > ../logs/backend-testing.log 2>&1 & echo $$! > ../logs/backend-testing.pid && cd ../.. && bash -c "disown %1" 2>/dev/null || true
	@echo ""
	@echo "⏱️  CADENCE TIMING (ultra-fast):"
	@echo "   - Weekly: 2 minutes"
	@echo "   - Monthly: 10 minutes"
	@echo "   - Quarterly: 30 minutes"
	@echo "   - Reminder scheduler: every 30 seconds"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "📋 Logs: logs/backend-testing.log"
	@echo "💡 Add test contacts with different cadences and watch reminders generate!"
	@echo "💡 Process will continue running after you close this terminal"

test-cadence-fast:
	@echo "🏎️  Starting FAST cadence testing..."
	@echo "This will test all reminder cadences in hours!"
	@echo ""
	@make staging
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting backend with fast cadences..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && cd backend && nohup go run cmd/crm-api/main.go > ../logs/backend-staging.log 2>&1 & echo $$! > ../logs/backend-staging.pid && cd ../.. && bash -c "disown %1" 2>/dev/null || true
	@echo ""
	@echo "⏱️  CADENCE TIMING (fast):"
	@echo "   - Weekly: 10 minutes (1 week = 10 min)"
	@echo "   - Monthly: 1 hour (1 month = 1 hour)"
	@echo "   - Quarterly: 3 hours (1 quarter = 3 hours)"
	@echo "   - Reminder scheduler: every 5 minutes"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "📋 Logs: logs/backend-staging.log"
	@echo "💡 Perfect for validating 3+ months of cadence behavior in 3 hours!"
	@echo "💡 Process will continue running after you close this terminal"

# Clean
clean:
	@echo "Cleaning build artifacts..."
	@cd backend && rm -rf bin/
	@cd frontend && rm -rf .next/ out/

clean-logs:
	@echo "Cleaning log files..."
	@rm -rf logs/*.log logs/*.pid
	@echo "✅ Logs cleaned"

# Docker operations
docker-up:
	@echo "Starting Docker services..."
	@cd infra && docker compose up -d

docker-down:
	@echo "Stopping Docker services..."
	@cd infra && docker compose down

docker-reset:
	@echo "Resetting Docker environment..."
	@cd infra && docker compose down -v
	@cd infra && docker compose up -d

# Production Commands
start:
	@echo "🚀 Starting Personal CRM..."
	@make prod
	@make build
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting CRM backend on port 8080..."
	@bash scripts/start-backend-prod.sh
	@echo "Starting CRM frontend on port 3001..."
	@bash scripts/start-frontend-prod.sh
	@sleep 3
	@echo ""
	@echo "✅ Personal CRM is running!"
	@echo "🌐 Frontend: http://localhost:3001"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo "📖 API Docs: http://localhost:8080/swagger/index.html"
	@echo ""
	@echo "📋 Logs:"
	@echo "   Backend:  logs/backend.log"
	@echo "   Frontend: logs/frontend.log"
	@echo ""
	@echo "💡 Processes will continue running after you close this terminal"
	@echo "   Use 'make stop' to stop the CRM"

start-local:
	@echo "🚀 Starting Personal CRM with local production config..."
	@if [ ! -f .env.local ]; then \
		echo "❌ Error: .env.local not found!"; \
		echo ""; \
		echo "Create .env.local with your production secrets first:"; \
		echo "  1. Generate secrets: openssl rand -base64 32"; \
		echo "  2. Copy template: cp .env.example.production .env.local"; \
		echo "  3. Edit .env.local with your secrets"; \
		exit 1; \
	fi
	@echo "📋 Using configuration from .env.local"
	@cp .env.local .env
	@make build
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting CRM backend on port 8080..."
	@bash scripts/start-backend-prod.sh
	@echo "Starting CRM frontend on port 3001..."
	@bash scripts/start-frontend-prod.sh
	@sleep 3
	@echo ""
	@echo "✅ Personal CRM is running with local production config!"
	@echo "🌐 Frontend: http://localhost:3001"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo "📖 API Docs: http://localhost:8080/swagger/index.html"
	@echo ""
	@echo "📋 Logs:"
	@echo "   Backend:  logs/backend.log"
	@echo "   Frontend: logs/frontend.log"
	@echo ""
	@echo "💡 Processes will continue running after you close this terminal"
	@echo "   Use 'make stop' to stop the CRM"

stop:
	@echo "🛑 Stopping Personal CRM..."
	@# Kill backend by port and name (prod uses compiled crm-api binary)
	@lsof -ti:8080 | xargs kill -9 2>/dev/null || true
	@pkill -f crm-api || true
	@# Kill frontend by port (process is 'next-server', not 'next start')
	@lsof -ti:3001 | xargs kill -9 2>/dev/null || true
	@make docker-down
	@echo "✅ Personal CRM stopped"

restart:
	@echo "🔄 Restarting Personal CRM..."
	@make stop
	@sleep 2
	@make start

reload:
	@echo "🔄 Rebuilding and reloading Personal CRM..."
	@echo "Building..."
	@make build
	@echo "Restarting backend..."
	@bash scripts/start-backend-prod.sh
	@echo "Restarting frontend..."
	@bash scripts/start-frontend-prod.sh
	@echo ""
	@echo "✅ Personal CRM reloaded!"
	@echo "🌐 Frontend: http://localhost:3001"
	@echo "🔧 Backend:  http://localhost:8080"

status:
	@echo "📊 Personal CRM Status:"
	@echo ""
	@echo "Backend (port 8080):"
	@curl -s http://localhost:8080/health | jq -r '.status' 2>/dev/null && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Frontend Dev (port 3000):"
	@curl -s http://localhost:3000 >/dev/null 2>&1 && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Frontend Prod (port 3001):"
	@curl -s http://localhost:3001 >/dev/null 2>&1 && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Database:"
	@docker ps --filter "name=crm-postgres" --format "table {{.Names}}\t{{.Status}}" | grep crm-postgres >/dev/null && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@if [ -d logs ]; then \
		echo "📋 Recent Log Files:"; \
		ls -lh logs/*.log 2>/dev/null | tail -5 || echo "  No log files found"; \
	fi

# Raspberry Pi Deployment
deploy:
	@./scripts/deploy.sh

setup-pi:
	@./scripts/setup-pi.sh
