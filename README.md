# Personal CRM

A single-user, local-first customer relationship management system with AI-powered insights.

## Quick Start

1. **Clone and setup environment**:
   ```bash
   cp env.example .env
   # Edit .env with your configuration
   ```

2. **Start the development environment**:
   ```bash
   make docker-up
   make dev
   ```

3. **Access the application**:
   - Frontend: http://localhost:3000
   - Backend API: http://localhost:8080
   - Health check: http://localhost:8080/health

## Prerequisites

- Docker and Docker Compose
- Go 1.22+
- Node.js 18+
- Make

## Environment Variables

Copy `env.example` to `.env` and configure the following variables:

### Required
- `DATABASE_URL`: PostgreSQL connection string
- `SESSION_SECRET`: Secure random string for session encryption

### Optional
- `ANTHROPIC_API_KEY`: For AI features (Phase 2+)
- `TELEGRAM_BOT_TOKEN`: For Telegram bot integration
- `PORT`: API server port (default: 8080)

## Development Commands

```bash
# Start all services
make dev

# Build project
make build

# Run tests
make test

# Docker operations
make docker-up      # Start database
make docker-down    # Stop database
make docker-reset   # Reset database with fresh data

# Clean build artifacts
make clean
```

## Project Structure

```
personal-crm/
├── frontend/           # Next.js React application
├── backend/            # Go API server
├── infra/              # Docker Compose & infrastructure
├── tests/              # E2E and integration tests
├── docs/               # Documentation
└── PLAN.md            # Architecture and implementation plan
```

## Testing

```bash
# Unit tests (fast, no external dependencies)
make test-unit

# Integration tests (requires database)
make test-integration

# All backend tests
make test-all

# API-specific tests
make test-api

# Frontend tests (when implemented)
cd frontend && npm test

# E2E tests
npx playwright test
```

See [TEST_GUIDE.md](TEST_GUIDE.md) for detailed testing information.

## Smoke Testing

**🚀 Idiot-Proof Smoke Test**

Run the complete smoke test that handles everything automatically:

```bash
./smoke-test.sh
```

This script will:
1. ✅ Stop all running services
2. ✅ Start Docker, Backend API, and Frontend
3. ✅ Run database migrations
4. ✅ Test all endpoints
5. ✅ Create and cleanup test data
6. ✅ Generate detailed logs

**🐛 Debug & Share Logs**

If something goes wrong, collect all logs for debugging:

```bash
./share-logs.sh
```

This creates a comprehensive debug bundle with:
- System information and running processes
- API test results and Docker container status
- Environment variables and recent log files

## Database

The application uses PostgreSQL with the pgvector extension for vector similarity search. The database is managed via Docker Compose and includes automatic initialization.

## Development Phases

- **Phase 1**: Core CRM (Contacts, Notes, Reminders) - *In Progress*
- **Phase 2**: AI Agent v0 (Embeddings, RAG, Chat UI)
- **Phase 3**: AI Agent v1 (Advanced features, Graph view, Export)

See `PLAN.md` for detailed architecture and implementation roadmap.

## Contributing

This is a personal project, but issues and suggestions are welcome.

## License

MIT
