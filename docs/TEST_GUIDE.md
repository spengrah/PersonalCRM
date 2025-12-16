# Testing Guide

This document explains how to run tests for the Personal CRM project.

## Test Structure

All backend tests are located in `backend/tests/` and are organized as follows:

```
backend/tests/
├── unit_test.go          # Unit tests (health endpoint, etc.)
├── integration_test.go   # Integration tests (database operations)
└── (future API tests)    # API integration tests
```

## Test Commands

### Run Unit Tests (Fast)
```bash
make test-unit
```
- Runs with `-short` flag to skip integration tests
- Tests isolated components without external dependencies
- Currently includes: health endpoint tests

### Run Integration Tests
```bash
make test-integration
```
- Requires running PostgreSQL database
- Tests database operations and repository layer
- Requires `DATABASE_URL` environment variable to be set

### Run All Tests
```bash
make test-all
```
- Runs both unit and integration tests
- Includes frontend tests when they exist

### Individual Test Commands
```bash
# From project root
cd backend && go test ./tests/... -v           # All backend tests
cd backend && go test ./tests/... -v -short    # Unit tests only
cd frontend && npm test                        # Frontend tests (when implemented)
```

## Prerequisites for Integration Tests

1. **Docker Compose running**:
   ```bash
   make docker-up
   ```

2. **Environment variables set**:
   ```bash
   export DATABASE_URL="postgres://crm_user:crm_password@localhost:5432/personal_crm?sslmode=disable"
   ```

3. **Database migrations applied** (optional - tests handle this):
   ```bash
   # If needed, run migrations manually
   cd backend && go run cmd/crm-api/main.go
   ```

## Test Development Guidelines

### Unit Tests
- Should be fast and not require external dependencies
- Test individual functions and components in isolation
- Use mocks for database and external service dependencies
- Should run with `-short` flag

### Integration Tests
- Test components working together with real dependencies
- Use real database connections
- Clean up test data after each test
- Should be skipped when `DATABASE_URL` is not set

### API Tests
- Test HTTP endpoints end-to-end
- Use test database for data operations
- Test request/response cycles including validation
- Clean up created resources after tests

## Example Test Run

```bash
# 1. Start services
make docker-up

# 2. Run unit tests (fast)
make test-unit

# 3. Run integration tests (requires DB)
make test-integration

# 4. Build and verify API
make api-build
```

## Continuous Integration

The test suite is designed to work in CI environments:
- Unit tests run without external dependencies
- Integration tests can be skipped if `DATABASE_URL` is not available
- Tests clean up after themselves to avoid state leakage

## Test Coverage

Current test coverage includes:
- ✅ Health endpoint functionality
- ✅ Contact repository CRUD operations
- ✅ Database connection and basic queries
- ⏳ API endpoint testing (planned)
- ⏳ Validation testing (planned)
- ⏳ Error handling testing (planned)
