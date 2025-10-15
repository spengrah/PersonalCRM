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

## Desktop App (macOS)

Build and run the native desktop app (no browser):

Prereqs:
- Rust toolchain (cargo)
- Node.js 18+
- Go 1.22+

Steps:
```bash
# 1) Build backend binary
cd "backend" && go build -o bin/crm-api ./cmd/crm-api

# 2) Build desktop UI
cd "../desktop-ui" && npm install && npm run build

# 3) Run desktop app in dev
cd "../desktop/src-tauri" && cargo run
```

Package a .app and DMG:
```bash
# Build optimized binary and bundle app
cd "desktop-ui" && npm run build
cd "../desktop/src-tauri" && cargo build --release
npx -y @tauri-apps/cli@latest build
```

Artifacts:
- App: `desktop/src-tauri/target/release/bundle/macos/Personal CRM.app`
- DMG: `desktop/src-tauri/target/release/bundle/dmg/Personal CRM_*.dmg`

Notes:
- The app loads environment variables from the project `.env` when launching the backend.
- The backend binds to 127.0.0.1 on a free port and is shut down when the app exits.

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
├── desktop-ui/         # Vite + React desktop UI
├── desktop/            # Tauri app (Rust)
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
