# Personal CRM Makefile

.PHONY: help dev build test clean docker-up docker-down docker-reset

# Default target
help:
	@echo "Available targets:"
	@echo "  dev         - Start development servers (frontend and backend)"
	@echo "  build       - Build both frontend and backend"
	@echo "  test        - Run all tests"
	@echo "  clean       - Clean build artifacts"
	@echo "  docker-up   - Start Docker Compose services"
	@echo "  docker-down - Stop Docker Compose services"
	@echo "  docker-reset- Reset Docker volumes and restart"

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
