# Personal CRM Makefile

.PHONY: help dev build test clean docker-up docker-down docker-reset test-cadence-ultra test-cadence-fast prod staging testing start stop restart status

# Default target
help:
	@echo "Available targets:"
	@echo ""
	@echo "🚀 Production Commands:"
	@echo "  start       - Start Personal CRM (production mode on port 3001)"
	@echo "  stop        - Stop Personal CRM"
	@echo "  restart     - Restart Personal CRM"
	@echo "  status      - Check CRM status"
	@echo ""
	@echo "Environment Management:"
	@echo "  testing     - Switch to testing environment (ultra-fast cadences)"
	@echo "  staging     - Switch to staging environment (fast cadences)" 
	@echo "  prod        - Switch to production environment (real cadences)"
	@echo ""
	@echo "Development:"
	@echo "  dev         - Start development servers (frontend and backend)"
	@echo "  build       - Build both frontend and backend"
	@echo "  test        - Run all tests"
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

# Development
dev:
	@echo "Starting development environment..."
	@make docker-up
	@echo "Starting backend server..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && cd backend && go run cmd/crm-api/main.go &
	@echo "Starting frontend development server..."
	@cd frontend && npm run dev

# Build
build:
	@echo "Building backend..."
	@cd backend && go build -o bin/crm-api cmd/crm-api/main.go
	@echo "Building frontend..."
	@cd frontend && npm run build

# Tests
test: test-unit test-integration

test-unit:
	@echo "Running unit tests..."
	@cd backend && go test ./tests/... -v -short

test-integration:
	@echo "Running integration tests..."
	@cd backend && go test ./tests/... -v

test-api:
	@echo "Running API tests..."
	@cd backend && go test ./tests/... -v

test-all:
	@echo "Running all backend tests..."
	@cd backend && go test ./tests/... -v
	@echo "Running frontend tests..."
	@cd frontend && npm test

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
	@cp env.testing .env
	@echo "✅ Testing environment active:"
	@echo "   - Weekly cadence: 2 minutes"  
	@echo "   - Monthly cadence: 10 minutes"
	@echo "   - Quarterly cadence: 30 minutes"
	@echo "   - Scheduler runs every 30 seconds"
	@echo ""
	@echo "Use 'make test-cadence-ultra' to validate all cadences quickly"

staging:
	@echo "Switching to STAGING environment (fast cadences)..."
	@cp env.staging .env
	@echo "✅ Staging environment active:"
	@echo "   - Weekly cadence: 10 minutes (1 week = 10 min)"
	@echo "   - Monthly cadence: 1 hour (1 month = 1 hour)"  
	@echo "   - Quarterly cadence: 3 hours (1 quarter = 3 hours)"
	@echo "   - Scheduler runs every 5 minutes"
	@echo ""
	@echo "Use 'make test-cadence-fast' to validate cadences in hours"

prod:
	@echo "Switching to PRODUCTION environment (real cadences)..."
	@cp env.production .env
	@echo "✅ Production environment active:"
	@echo "   - Weekly cadence: 7 days"
	@echo "   - Monthly cadence: 30 days"
	@echo "   - Quarterly cadence: 90 days"  
	@echo "   - Scheduler runs daily at 8 AM"
	@echo ""
	@echo "⚠️  CAUTION: Real-world timing active"

# Cadence testing commands
test-cadence-ultra:
	@echo "🚀 Starting ULTRA-FAST cadence testing..."
	@echo "This will test all reminder cadences in minutes!"
	@echo ""
	@make testing
	@make docker-up
	@echo "Starting backend with ultra-fast cadences..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && cd backend && go run cmd/crm-api/main.go &
	@echo ""
	@echo "⏱️  CADENCE TIMING (ultra-fast):"
	@echo "   - Weekly: 2 minutes"
	@echo "   - Monthly: 10 minutes" 
	@echo "   - Quarterly: 30 minutes"
	@echo "   - Scheduler: every 30 seconds"
	@echo ""
	@echo "💡 Add test contacts with different cadences and watch reminders generate!"

test-cadence-fast:
	@echo "🏎️  Starting FAST cadence testing..."
	@echo "This will test all reminder cadences in hours!"
	@echo ""
	@make staging
	@make docker-up
	@echo "Starting backend with fast cadences..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && cd backend && go run cmd/crm-api/main.go &
	@echo ""
	@echo "⏱️  CADENCE TIMING (fast):"
	@echo "   - Weekly: 10 minutes (1 week = 10 min)"
	@echo "   - Monthly: 1 hour (1 month = 1 hour)"
	@echo "   - Quarterly: 3 hours (1 quarter = 3 hours)" 
	@echo "   - Scheduler: every 5 minutes"
	@echo ""
	@echo "💡 Perfect for validating 3+ months of cadence behavior in 3 hours!"

# Clean
clean:
	@echo "Cleaning build artifacts..."
	@cd backend && rm -rf bin/
	@cd frontend && rm -rf .next/ out/

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
	@echo "Starting CRM backend on port 8080..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && ./backend/bin/crm-api &
	@echo "Starting CRM frontend on port 3001..."
	@cd frontend && PORT=3001 npm run start &
	@sleep 3
	@echo ""
	@echo "✅ Personal CRM is running!"
	@echo "🌐 Frontend: http://localhost:3001"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo "📖 API Docs: http://localhost:8080/swagger/index.html"
	@echo ""
	@echo "Use 'make stop' to stop the CRM"

stop:
	@echo "🛑 Stopping Personal CRM..."
	@pkill -f crm-api || true
	@pkill -f "next start" || true
	@make docker-down
	@echo "✅ Personal CRM stopped"

restart:
	@echo "🔄 Restarting Personal CRM..."
	@make stop
	@sleep 2
	@make start

status:
	@echo "📊 Personal CRM Status:"
	@echo ""
	@echo "Backend (port 8080):"
	@curl -s http://localhost:8080/health | jq -r '.status' 2>/dev/null && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Frontend (port 3001):"
	@curl -s http://localhost:3001 >/dev/null 2>&1 && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Database:"
	@docker ps --filter "name=crm-postgres" --format "table {{.Names}}\t{{.Status}}" | grep crm-postgres >/dev/null && echo "  ✅ Running" || echo "  ❌ Not running"
