# Local macOS Production Deployment Guide

**Last Updated**: December 2025
**Target**: macOS (tested on macOS 14+)
**Estimated Time**: 20-30 minutes

This guide walks you through deploying PersonalCRM locally on your macOS machine in production mode. This is useful for:
- Testing production deployment before moving to Pi
- Running a personal instance on your always-on macbook
- Troubleshooting deployment issues locally before remote deployment

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Quick Start](#quick-start)
3. [Part 1: Environment Setup](#part-1-environment-setup)
4. [Part 2: Database Setup](#part-2-database-setup)
5. [Part 3: Build Application](#part-3-build-application)
6. [Part 4: Deployment Options](#part-4-deployment-options)
7. [Part 5: Verification](#part-5-verification)
8. [Part 6: Managing Services](#part-6-managing-services)
9. [Troubleshooting](#troubleshooting)
10. [Migration to Pi](#migration-to-pi)

---

## Prerequisites

### Hardware
- macOS 14+ (Intel or Apple Silicon)
- 8GB+ RAM recommended
- 2GB+ free disk space

### Software
You need these installed on your Mac:

```bash
# Check if Docker Desktop is installed
docker --version

# Check if Go is installed (1.23+)
go version

# Check if Bun is installed
bun --version
```

**If any are missing**, install them:

#### Docker Desktop
```bash
# Download from https://www.docker.com/products/docker-desktop/
# Or use Homebrew:
brew install --cask docker

# Start Docker Desktop from Applications
```

#### Go 1.23+
```bash
# Using Homebrew:
brew install go@1.23

# Verify:
go version
# Should output: go version go1.23.x darwin/amd64 (or darwin/arm64)
```

#### Bun
```bash
# Install Bun:
curl -fsSL https://bun.sh/install | bash

# Add to PATH (should be automatic):
source ~/.bashrc  # or ~/.zshrc

# Verify:
bun --version
```

### Accounts
- GitHub access to spengrah/PersonalCRM repository
- Anthropic API key (optional, for AI features)

---

## Quick Start

For experienced users, here's the TL;DR:

```bash
# 1. Clone and navigate to project
cd ~/Workspaces/PersonalCRM  # or wherever you cloned it

# 2. Generate production secrets
SESSION_SECRET=$(openssl rand -base64 32)
API_KEY=$(openssl rand -hex 32)
POSTGRES_PASSWORD=$(openssl rand -base64 24)

# 3. Create production .env.local (safe from being overwritten)
cp .env.example.production .env.local
# Edit .env.local and replace SESSION_SECRET, API_KEY, POSTGRES_PASSWORD

# 4. Create frontend .env.local
echo "NEXT_PUBLIC_API_KEY=$API_KEY" > frontend/.env.local

# 5. Start services with your local config
make start-local

# 6. Access app
open http://localhost:3001
```

For detailed instructions, continue reading below.

---

## Part 1: Environment Setup

### 1.1 Navigate to Project

```bash
# Navigate to your PersonalCRM directory
cd ~/Workspaces/PersonalCRM  # adjust path as needed

# Ensure you're on latest main branch
git checkout main
git pull origin main
```

### 1.2 Generate Production Secrets

Generate secure secrets for production use:

```bash
# Generate SESSION_SECRET (for session encryption)
SESSION_SECRET=$(openssl rand -base64 32)
echo "SESSION_SECRET: $SESSION_SECRET"

# Generate API_KEY (for API authentication)
API_KEY=$(openssl rand -hex 32)
echo "API_KEY: $API_KEY"

# Generate PostgreSQL password
POSTGRES_PASSWORD=$(openssl rand -base64 24)
echo "POSTGRES_PASSWORD: $POSTGRES_PASSWORD"
```

**IMPORTANT**: Save these values immediately in your password manager with labels:
- `PersonalCRM Local - SESSION_SECRET`
- `PersonalCRM Local - API_KEY`
- `PersonalCRM Local - POSTGRES_PASSWORD`

### 1.3 Create Production Environment File

Create your production environment configuration in `.env.local` (this keeps your secrets safe from being overwritten by make targets):

```bash
# Copy production template to .env.local
cp .env.example.production .env.local

# Edit the file (use your preferred editor)
nano .env.local  # or: code .env.local, vim .env.local, etc.
```

**Update these critical values** in `.env.local`:

```bash
# Database - Update password
DATABASE_URL=postgres://crm_user:<YOUR_POSTGRES_PASSWORD>@localhost:5432/personal_crm?sslmode=disable
POSTGRES_PASSWORD=<YOUR_POSTGRES_PASSWORD>

# Server
PORT=8080
NODE_ENV=production
GIN_MODE=release

# Authentication & Session - Use your generated values
SESSION_SECRET=<YOUR_SESSION_SECRET>
API_KEY=<YOUR_API_KEY>

# CORS - For local deployment
CORS_ALLOW_ALL=false
FRONTEND_URL=http://localhost:3001

# Logging
LOG_LEVEL=info

# Feature Flags (optional)
ENABLE_VECTOR_SEARCH=false
ENABLE_TELEGRAM_BOT=false
ENABLE_CALENDAR_SYNC=false

# CRM Environment
CRM_ENV=production
```

**Save the file** (Ctrl+O, Enter, Ctrl+X in nano).

**Why `.env.local`?** Using `.env.local` instead of `.env` protects your production secrets from being accidentally overwritten when you use development make targets like `make dev`, `make testing`, or `make staging`.

### 1.4 Create Frontend Environment File

```bash
# Create frontend .env.local with your API key
cat > frontend/.env.local <<EOF
# API Authentication (must match backend API_KEY)
NEXT_PUBLIC_API_KEY=<YOUR_API_KEY>
EOF
```

Replace `<YOUR_API_KEY>` with the API key you generated earlier.

### 1.5 Validate Environment Configuration

Run a quick validation to ensure all required variables are set:

```bash
# Check that critical variables are not using dev values
echo "Checking environment configuration..."
grep "SESSION_SECRET=" .env.local | grep -v "dev-session" && echo "✓ SESSION_SECRET set" || echo "✗ SESSION_SECRET still has dev value"
grep "API_KEY=" .env.local | grep -v "dev-api-key" && echo "✓ API_KEY set" || echo "✗ API_KEY still has dev value"
grep "POSTGRES_PASSWORD=" .env.local | grep -v "crm_password" && echo "✓ POSTGRES_PASSWORD set" || echo "✗ POSTGRES_PASSWORD still has dev value"

# Verify frontend config
grep "NEXT_PUBLIC_API_KEY=" frontend/.env.local && echo "✓ Frontend API_KEY set" || echo "✗ Frontend API_KEY missing"

# Verify .env.local is gitignored (won't be committed)
git check-ignore .env.local && echo "✓ .env.local is gitignored" || echo "⚠️  Warning: .env.local not gitignored"
```

**All checks should pass** before proceeding.

---

## Part 2: Database Setup

### 2.1 Start PostgreSQL via Docker

The project uses Docker Compose for the database:

```bash
# Start database container
make docker-up

# Verify database is running
docker ps | grep crm-postgres
```

**Expected output**: You should see the `crm-postgres` container running.

### 2.2 Verify Database Connection

```bash
# Test database connection
docker exec -it crm-postgres psql -U crm_user -d personal_crm -c "SELECT version();"
```

**Expected output**: PostgreSQL version information.

If this fails, see [Troubleshooting](#troubleshooting).

---

## Part 3: Build Application

### 3.1 Build Backend

```bash
# Build Go backend binary
cd backend
go build -o bin/crm-api cmd/crm-api/main.go
cd ..

# Verify binary was created
ls -lh backend/bin/crm-api
```

**Expected**: Binary file around 20-40MB.

### 3.2 Build Frontend

```bash
# Install dependencies (if not already done)
cd frontend
bun install

# Build production frontend
bun run build
cd ..

# Verify build succeeded
ls -la frontend/.next/
```

**Expected**: `.next` directory containing the production build.

**Build time**: First build may take 1-2 minutes.

---

## Part 4: Deployment Options

You have three options for running the application locally:

### Option 1: Using Make Commands (Recommended)

The simplest approach using the provided Makefile:

```bash
# Start all services with your .env.local configuration
make start-local
```

This command will:
1. Verify `.env.local` exists (fails with helpful error if not)
2. Copy `.env.local` to `.env` (so the app can read it)
3. Build both backend and frontend
4. Start database container
5. Start backend on port 8080
6. Start frontend on port 3001
7. Run processes detached (continue after terminal closes)

**To stop**:
```bash
make stop
```

**To check status**:
```bash
make status
```

**Alternative: Using environment templates**

If you want to use the example environment files (for testing different cadence settings):

```bash
# These will overwrite .env (but not .env.local)
make testing    # Ultra-fast cadences for testing
make staging    # Fast cadences for staging
make dev        # Development mode

# To go back to your production config:
make start-local
```

### Option 2: Manual Process Management

If you want more control, start services manually:

```bash
# 1. Copy your config to .env (if not already done)
cp .env.local .env

# 2. Start database
make docker-up

# 3. Build application
make build

# 4. Create logs directory
mkdir -p logs

# 5. Start backend
./scripts/start-backend-prod.sh

# 6. Start frontend
./scripts/start-frontend-prod.sh
```

**To stop manually**:
```bash
# Stop backend
pkill -f crm-api

# Stop frontend
pkill -f "next start"

# Stop database
make docker-down
```

### Option 3: macOS LaunchAgents (Auto-start on Login)

For a more permanent deployment that starts on login, use LaunchAgents:

#### Create LaunchAgent for Backend

```bash
# Create LaunchAgents directory if it doesn't exist
mkdir -p ~/Library/LaunchAgents

# Create backend plist
cat > ~/Library/LaunchAgents/com.personalcrm.backend.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.personalcrm.backend</string>
    <key>ProgramArguments</key>
    <array>
        <string>$(pwd)/backend/bin/crm-api</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$(pwd)</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>DATABASE_URL</key>
        <string>postgres://crm_user:$(grep POSTGRES_PASSWORD .env | cut -d= -f2)@localhost:5432/personal_crm?sslmode=disable</string>
        <key>PORT</key>
        <string>8080</string>
        <key>NODE_ENV</key>
        <string>production</string>
    </dict>
    <key>StandardOutPath</key>
    <string>$(pwd)/logs/backend.log</string>
    <key>StandardErrorPath</key>
    <string>$(pwd)/logs/backend-error.log</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
EOF
```

#### Create LaunchAgent for Frontend

```bash
cat > ~/Library/LaunchAgents/com.personalcrm.frontend.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.personalcrm.frontend</string>
    <key>ProgramArguments</key>
    <array>
        <string>$(which bun)</string>
        <string>run</string>
        <string>start</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$(pwd)/frontend</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PORT</key>
        <string>3001</string>
    </dict>
    <key>StandardOutPath</key>
    <string>$(pwd)/logs/frontend.log</string>
    <key>StandardErrorPath</key>
    <string>$(pwd)/logs/frontend-error.log</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
EOF
```

#### Load LaunchAgents

```bash
# Load backend service
launchctl load ~/Library/LaunchAgents/com.personalcrm.backend.plist

# Load frontend service
launchctl load ~/Library/LaunchAgents/com.personalcrm.frontend.plist

# Verify they're running
launchctl list | grep personalcrm
```

#### Managing LaunchAgents

```bash
# Stop services
launchctl unload ~/Library/LaunchAgents/com.personalcrm.backend.plist
launchctl unload ~/Library/LaunchAgents/com.personalcrm.frontend.plist

# Start services
launchctl load ~/Library/LaunchAgents/com.personalcrm.backend.plist
launchctl load ~/Library/LaunchAgents/com.personalcrm.frontend.plist

# View logs
tail -f logs/backend.log
tail -f logs/frontend.log
```

---

## Part 5: Verification

### 5.1 Health Check

Verify the backend is running and healthy:

```bash
# Check backend health endpoint
curl http://localhost:8080/health

# Expected output (pretty-printed):
# {
#   "status": "healthy",
#   "timestamp": "2025-12-26T...",
#   "version": {...},
#   "components": {
#     "database": {"status": "healthy", "response_time": "..."}
#   },
#   "system": {...}
# }
```

If you get a JSON response with `"status": "healthy"`, your backend is running correctly.

### 5.2 Test API Authentication

```bash
# Try accessing API without authentication (should fail with 401)
curl -i http://localhost:8080/api/v1/contacts

# Expected: HTTP/1.1 401 Unauthorized

# Try with correct API key (should succeed)
API_KEY=$(grep "^API_KEY=" .env.local | cut -d= -f2)
curl -i -H "X-API-Key: $API_KEY" http://localhost:8080/api/v1/contacts

# Expected: HTTP/1.1 200 OK
# {"success":true,"data":[],"pagination":{...}}
```

### 5.3 Test Frontend

```bash
# Check frontend is serving
curl -I http://localhost:3001

# Expected: HTTP/1.1 200 OK
```

**In your browser**, navigate to:
```
http://localhost:3001
```

**Expected**: PersonalCRM application loads with the contacts interface.

### 5.4 Create Test Contact

**In the browser**:
1. Navigate to Contacts page
2. Click "Add Contact"
3. Fill in:
   - Name: "Test User"
   - Email: "test@example.com"
   - Cadence: "Weekly"
4. Save

**Expected**: Contact appears in the list without authentication errors.

If you see "401 Unauthorized" errors:
- Verify `frontend/.env.local` has correct `NEXT_PUBLIC_API_KEY`
- Rebuild frontend: `cd frontend && bun run build`
- Restart frontend process

### 5.5 Check API Documentation

```bash
# Open Swagger API docs in browser
open http://localhost:8080/swagger/index.html
```

**Expected**: Interactive API documentation loads.

---

## Part 6: Managing Services

### Using Make Commands

```bash
# Check status of all services
make status

# Stop all services
make stop

# Restart with your local production config
make stop && make start-local

# View logs
tail -f logs/backend.log
tail -f logs/frontend.log
```

### Switching Between Environments

```bash
# Use your production config (.env.local)
make start-local

# Switch to dev mode (uses .env.example)
make dev

# Switch to testing mode (ultra-fast cadences)
make testing

# Switch to staging mode (fast cadences)
make staging

# Go back to your production config
make stop
make start-local
```

### Manual Process Management

```bash
# Check what's running on ports
lsof -i :8080  # Backend
lsof -i :3001  # Frontend
lsof -i :5432  # Database

# View process tree
ps aux | grep -E "crm-api|next start|postgres"

# Stop specific process
kill <PID>  # Use PID from ps or lsof output

# Force stop
pkill -9 -f crm-api
```

### Database Management

```bash
# Start database
make docker-up

# Stop database
make docker-down

# Reset database (WARNING: deletes all data)
make docker-reset

# Access database shell
docker exec -it crm-postgres psql -U crm_user -d personal_crm

# Backup database
docker exec crm-postgres pg_dump -U crm_user personal_crm > backup_$(date +%Y%m%d_%H%M%S).sql

# Restore database
cat backup.sql | docker exec -i crm-postgres psql -U crm_user -d personal_crm
```

---

## Troubleshooting

### Backend Won't Start

**Symptom**: Backend process exits immediately or health check fails.

**Diagnosis**:
```bash
# Check backend logs
tail -n 100 logs/backend.log

# Common errors and solutions:
```

**Common Issues**:

1. **Database connection failed**
   ```
   Error: failed to connect to database
   ```
   - **Check**: Database is running: `docker ps | grep postgres`
   - **Check**: Password in `.env` matches database password
   - **Fix**: Restart database: `make docker-reset`

2. **Port already in use**
   ```
   Error: bind: address already in use
   ```
   - **Check**: What's using port 8080: `lsof -i :8080`
   - **Fix**: Kill the process or change `PORT` in `.env`

3. **Missing migrations**
   ```
   Error: failed to run migrations
   ```
   - **Check**: Migrations directory exists: `ls backend/migrations/`
   - **Fix**: Ensure you're running from project root

4. **Environment variables not loaded**
   ```
   Error: DATABASE_URL environment variable is required
   ```
   - **Check**: `.env` file exists in project root
   - **Fix**: Ensure you're using the startup scripts or make commands

### Frontend Won't Start

**Symptom**: Frontend returns 502 Bad Gateway or doesn't load.

**Diagnosis**:
```bash
# Check frontend logs
tail -n 100 logs/frontend.log

# Check if process is running
ps aux | grep "next start"

# Check frontend build
ls -la frontend/.next/
```

**Common Issues**:

1. **Build failed or missing**
   - **Fix**: Rebuild frontend: `cd frontend && bun run build`

2. **Port already in use**
   - **Check**: `lsof -i :3001`
   - **Fix**: Kill process or change port in startup script

3. **Dependencies not installed**
   - **Fix**: `cd frontend && bun install`

### API Returns 401 Unauthorized

**Symptom**: All API requests return 401, even in browser.

**Diagnosis**:
```bash
# Verify API_KEY is set in backend
grep "^API_KEY=" .env.local

# Verify .env has been created from .env.local
ls -la .env

# Verify frontend has matching key
grep "NEXT_PUBLIC_API_KEY=" frontend/.env.local

# Test API directly
API_KEY=$(grep "^API_KEY=" .env.local | cut -d= -f2)
curl -H "X-API-Key: $API_KEY" http://localhost:8080/api/v1/contacts
```

**Solutions**:
1. Ensure both `.env.local` and `frontend/.env.local` have the same API_KEY value
2. Rebuild frontend after changing API_KEY: `cd frontend && bun run build`
3. Restart both services: `make stop && make start-local`

### Database Connection Issues

**Symptom**: Backend reports database connection errors.

**Diagnosis**:
```bash
# Check if database container is running
docker ps | grep crm-postgres

# Check database logs
docker logs crm-postgres

# Try connecting manually
docker exec -it crm-postgres psql -U crm_user -d personal_crm -c "\dt"
```

**Solutions**:
1. **Container not running**: `make docker-up`
2. **Wrong credentials**: Verify `POSTGRES_PASSWORD` in `.env.local` matches what's in `.env` and `docker-compose.yml`
3. **Database not initialized**: `make docker-reset`
4. **`.env` not created**: Run `cp .env.local .env` or use `make start-local`

### High CPU or Memory Usage

**Symptom**: Mac becomes slow, fans spin up.

**Diagnosis**:
```bash
# Check process resource usage
ps aux | grep -E "crm-api|next|docker" | awk '{print $2, $3, $4, $11}'

# Check Docker stats
docker stats crm-postgres
```

**Solutions**:
1. **Frontend build running**: Normal during `bun run build`, wait for completion
2. **Multiple instances running**: Stop duplicates: `pkill -f crm-api`
3. **Database needs tuning**: Adjust PostgreSQL settings in `infra/docker-compose.yml`

### Can't Access from Browser

**Symptom**: Browser shows "Connection refused" or "Cannot connect".

**Diagnosis**:
```bash
# Check services are listening
lsof -i :3001  # Frontend should be here
lsof -i :8080  # Backend should be here

# Check firewall (rarely needed for localhost)
/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate
```

**Solutions**:
1. **Services not started**: Run `make start-local`
2. **Wrong URL**: Use `http://localhost:3001` (not 3000, not 8080)
3. **Browser cache**: Try incognito/private browsing mode

---

## Migration to Pi

Once you've verified everything works locally, you can deploy to your Raspberry Pi.

### Export Configuration

```bash
# Create a deployment package with your configuration
mkdir -p ~/personalcrm-deployment
cp .env.local ~/personalcrm-deployment/.env
cp frontend/.env.local ~/personalcrm-deployment/frontend-env.local

# Optional: Create a backup of your data
docker exec crm-postgres pg_dump -U crm_user personal_crm > ~/personalcrm-deployment/data-backup.sql
```

**IMPORTANT**: Store these files securely! They contain your API keys and secrets.

### Transfer to Pi

When you're ready to deploy to your Pi, follow the [FIRST_TIME_PI_DEPLOYMENT.md](./FIRST_TIME_PI_DEPLOYMENT.md) guide, but use the `.env` files you created locally instead of generating new secrets.

```bash
# Copy your config files to Pi
scp ~/personalcrm-deployment/.env pi@personalcrm.local:/tmp/
scp ~/personalcrm-deployment/frontend-env.local pi@personalcrm.local:/tmp/frontend-env.local

# Optional: Transfer data backup
scp ~/personalcrm-deployment/data-backup.sql pi@personalcrm.local:/tmp/
```

Then continue with the Pi deployment guide from Part 3 onwards.

### Key Differences: macOS vs Pi

| Aspect | macOS | Raspberry Pi |
|--------|-------|--------------|
| Process Management | Manual/LaunchAgents | systemd services |
| Auto-start on Boot | LaunchAgents | systemd enabled |
| Binary Architecture | darwin/amd64 or arm64 | linux/arm64 |
| Service User | Your user account | Dedicated `crm` user |
| Installation Path | Project directory | `/opt/personalcrm` |
| Log Management | Files in `logs/` | journalctl |
| Network Access | Localhost only | Tailscale recommended |

---

## Best Practices

### Security

1. **Keep secrets secure**: Never commit `.env` files to git
2. **Rotate API keys**: Change `API_KEY` every 90 days
3. **Use strong passwords**: All generated secrets should be 32+ characters
4. **Localhost only**: Don't expose backend (port 8080) to network

### Monitoring

```bash
# Create a health check script
cat > ~/check-crm-health.sh <<'EOF'
#!/bin/bash
echo "=== PersonalCRM Health Check ==="
echo ""
echo "Backend (port 8080):"
curl -s http://localhost:8080/health | grep -q "healthy" && echo "  ✓ Healthy" || echo "  ✗ Unhealthy"
echo ""
echo "Frontend (port 3001):"
curl -s -I http://localhost:3001 | head -1 | grep -q "200 OK" && echo "  ✓ Running" || echo "  ✗ Not running"
echo ""
echo "Database:"
docker ps | grep -q crm-postgres && echo "  ✓ Running" || echo "  ✗ Not running"
EOF

chmod +x ~/check-crm-health.sh

# Run anytime to check status
~/check-crm-health.sh
```

### Backups

Set up automated database backups:

```bash
# Create backup script
cat > ~/backup-crm.sh <<'EOF'
#!/bin/bash
BACKUP_DIR=~/crm-backups
mkdir -p $BACKUP_DIR
DATE=$(date +%Y%m%d_%H%M%S)
docker exec crm-postgres pg_dump -U crm_user personal_crm > $BACKUP_DIR/crm_backup_$DATE.sql
echo "Backup created: $BACKUP_DIR/crm_backup_$DATE.sql"

# Keep only last 7 backups
ls -t $BACKUP_DIR/crm_backup_*.sql | tail -n +8 | xargs rm -f
EOF

chmod +x ~/backup-crm.sh

# Run manually
~/backup-crm.sh

# Or schedule with cron (runs daily at 2 AM)
(crontab -l 2>/dev/null; echo "0 2 * * * ~/backup-crm.sh") | crontab -
```

### Updating

When you pull new code from the repository:

```bash
# Navigate to project
cd ~/Workspaces/PersonalCRM

# Pull latest changes
git pull origin main

# Stop services
make stop

# Restart with your local config (this rebuilds automatically)
make start-local

# Verify
make status
```

**Note**: Your `.env.local` file is preserved during updates since it's gitignored. Only update it if new environment variables are added to the project.

---

## Success Checklist

Mark each item as you complete it:

- [ ] All prerequisites installed (Docker, Go, Bun)
- [ ] Production secrets generated and saved securely
- [ ] `.env.local` file created with production configuration
- [ ] `frontend/.env.local` created with API key
- [ ] Verified `.env.local` is gitignored
- [ ] Database container running
- [ ] Backend built successfully
- [ ] Frontend built successfully
- [ ] Services started via `make start-local`
- [ ] Health check returns "healthy" status
- [ ] API authentication working (401 without key, 200 with key)
- [ ] Frontend accessible at http://localhost:3001
- [ ] Test contact created successfully
- [ ] Health check script created
- [ ] Backup script created (optional)
- [ ] Documented where secrets are stored

---

## Next Steps

### Enable Additional Features

After stable deployment, consider enabling:

1. **AI Features** (requires Anthropic API key):
   ```bash
   # Add to .env.local
   nano .env.local
   # Add these lines:
   # ANTHROPIC_API_KEY=your-key-here
   # ENABLE_VECTOR_SEARCH=true

   # Restart backend
   make stop && make start-local
   ```

2. **Telegram Bot** (optional):
   ```bash
   # Get token from @BotFather on Telegram
   # Add to .env.local
   nano .env.local
   # Add these lines:
   # TELEGRAM_BOT_TOKEN=your-token-here
   # ENABLE_TELEGRAM_BOT=true

   # Restart backend
   make stop && make start-local
   ```

### Performance Optimization

For better performance on macOS:

1. **Allocate more resources to Docker Desktop**:
   - Open Docker Desktop → Settings → Resources
   - Increase CPUs to 4-6
   - Increase Memory to 4-8GB

2. **Use production mode** (already configured):
   - `NODE_ENV=production` enables optimizations
   - `GIN_MODE=release` reduces logging overhead

### Remote Access (Optional)

To access from other devices on your network:

1. **Find your Mac's IP address**:
   ```bash
   ifconfig | grep "inet " | grep -v 127.0.0.1
   ```

2. **Access from other devices**:
   ```
   http://<your-mac-ip>:3001
   ```

3. **For secure remote access**, use Tailscale:
   - Install Tailscale on your Mac: https://tailscale.com/download/mac
   - Access via Tailscale hostname from any device

---

## Questions or Issues?

If you encounter problems not covered in this guide:

1. **Check logs**:
   ```bash
   tail -n 100 logs/backend.log
   tail -n 100 logs/frontend.log
   docker logs crm-postgres
   ```

2. **Run health check**:
   ```bash
   ~/check-crm-health.sh
   ```

3. **Review environment**:
   ```bash
   cat .env.local | grep -v "PASSWORD\|SECRET\|KEY"  # View config (redacted)
   ```

4. **Check GitHub issues**: https://github.com/spengrah/PersonalCRM/issues

5. **Refer to related docs**:
   - [ENV_CHECKLIST.md](./ENV_CHECKLIST.md) - Environment variable reference
   - [FIRST_TIME_PI_DEPLOYMENT.md](./FIRST_TIME_PI_DEPLOYMENT.md) - Pi deployment guide
   - [TEST_GUIDE.md](./TEST_GUIDE.md) - Testing procedures

---

**Congratulations!** 🎉

If you've completed all the steps and checks above, your PersonalCRM is now running locally on your Mac with:
- ✅ Production configuration
- ✅ API key authentication
- ✅ PostgreSQL database with migrations
- ✅ Production-optimized builds
- ✅ Health monitoring

You're now ready to test the deployment process before moving to your Raspberry Pi, or you can continue using it locally on your always-on macbook.

For migration to Pi, see the [Migration to Pi](#migration-to-pi) section above and follow the [FIRST_TIME_PI_DEPLOYMENT.md](./FIRST_TIME_PI_DEPLOYMENT.md) guide.
