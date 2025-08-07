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
	@cd backend && go run cmd/crm-api/main.go &
	@echo "Starting frontend development server..."
	@cd frontend && npm run dev

# Build
build:
	@echo "Building backend..."
	@cd backend && go build -o bin/crm-api cmd/crm-api/main.go
	@echo "Building frontend..."
	@cd frontend && npm run build

# Tests
test:
	@echo "Running backend tests..."
	@cd backend && go test ./...
	@echo "Running frontend tests..."
	@cd frontend && npm test

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
