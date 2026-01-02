# PersonalCRM Deployment

This document describes the deployment architecture and workflow for PersonalCRM.

## Overview

PersonalCRM uses a **build-on-Mac, deploy-to-Pi** workflow:

1. **Build locally** on your Mac (faster builds, cross-compilation)
2. **Deploy via rsync** to your Raspberry Pi over Tailscale
3. **Secrets stay on Pi** (never committed to git)

This approach provides:
- Fast deployment cycles (rsync only transfers changed files)
- Clean separation between dev and production environments
- Simplified Pi requirements (no Go/Bun needed, just Docker + Node.js)
- Smaller deployment size (~50MB vs ~300MB with node_modules)

## Architecture

```
Local (Mac)                          Pi (raspberet)
-----------------                    -----------------
.env (gitignored)                    /srv/personalcrm/.env (production secrets)
.env.example.testing (git)

backend/bin/crm-api (x86)            /srv/personalcrm/backend/bin/crm-api (ARM64)
frontend/.next/standalone/           /srv/personalcrm/frontend/
                                     └── server.js (standalone Next.js)
```

### Why `/srv/`?

The Filesystem Hierarchy Standard (FHS) recommends `/srv/` for "site-specific data served by the system" - appropriate for a service accessed from multiple devices.

### Why Standalone Mode?

Next.js `output: 'standalone'` creates a self-contained build that:
- Eliminates `node_modules` (~300MB → ~50MB deployment size)
- Avoids platform mismatch (macOS binaries in node_modules won't run on Linux ARM64)
- Is the official Next.js production deployment approach

## Deployment Commands

### First-Time Setup

```bash
# 1. Set up the Pi (creates user, directories)
make setup-pi

# 2. SSH to Pi and create secrets
ssh raspberet 'sudo nano /srv/personalcrm/.env'

# 3. Deploy
make deploy
```

### Regular Deploys

```bash
make deploy
```

This single command:
1. Builds backend for ARM64
2. Builds frontend in standalone mode
3. Rsyncs files to Pi
4. Installs/updates systemd services
5. Restarts services
6. Verifies health checks

### Skip Build (Quick Deploy)

If you've already built and just want to redeploy:

```bash
./scripts/deploy.sh --skip-build
```

### Custom Pi Hostname

```bash
PI_HOST=mypi.local make deploy
```

## What Gets Deployed

| Source | Destination | Notes |
|--------|-------------|-------|
| `backend/bin/crm-api` | `/srv/personalcrm/backend/bin/` | ARM64 binary |
| `backend/migrations/` | `/srv/personalcrm/backend/migrations/` | SQL migration files |
| `frontend/.next/standalone/` | `/srv/personalcrm/frontend/` | Standalone Next.js server |
| `frontend/.next/static/` | `/srv/personalcrm/frontend/.next/static/` | Static assets |
| `frontend/public/` | `/srv/personalcrm/frontend/public/` | Public assets |
| `infra/docker-compose.yml` | `/srv/personalcrm/infra/` | Database config |
| `infra/init-db.sql` | `/srv/personalcrm/infra/` | Database init script |
| `infra/*.service` | `/etc/systemd/system/` | Systemd units (via sudo) |
| `infra/*.target` | `/etc/systemd/system/` | Systemd target |

**NOT deployed:** `.env` files (production secrets stay on Pi only)

## Systemd Services

The deployment uses four systemd units:

| Service | Description |
|---------|-------------|
| `personalcrm.target` | Umbrella target to start/stop all services |
| `personalcrm-database.service` | PostgreSQL via Docker Compose |
| `personalcrm-backend.service` | Go API server |
| `personalcrm-frontend.service` | Next.js standalone server |

### Manage Services

```bash
# Start all services
ssh raspberet 'sudo systemctl start personalcrm.target'

# Stop all services
ssh raspberet 'sudo systemctl stop personalcrm.target'

# Restart all services
ssh raspberet 'sudo systemctl restart personalcrm.target'

# Check status
ssh raspberet 'sudo systemctl status personalcrm.target'

# View logs
ssh raspberet 'sudo journalctl -u personalcrm-backend -f'
ssh raspberet 'sudo journalctl -u personalcrm-frontend -f'
```

## Environment Separation

| Environment | Location | Purpose |
|-------------|----------|---------|
| Development | `.env` (Mac, gitignored) | Local development |
| Testing | `.env.example.testing` (git tracked) | CI/automated tests |
| Production | `/srv/personalcrm/.env` (Pi only) | Live deployment |

### Environment Files

- `.env` - Your local development config (gitignored)
- `.env.example` - Template for new developers
- `.env.example.testing` - Deterministic values for tests
- `.env.example.staging` - Fast cadences for staging
- `.env.example.production` - Template for production

## Pi Prerequisites

The Pi needs:
- **Docker** - For PostgreSQL
- **Node.js 20+** - For standalone Next.js server
- **curl** - For health checks
- **Tailscale** - For secure remote access (recommended)

The Pi does NOT need:
- Go (backend is cross-compiled)
- Bun (frontend is pre-built)
- npm/yarn (standalone mode)

## Security

### Secrets Management

Production secrets are:
- Stored only on the Pi at `/srv/personalcrm/.env`
- Owned by `crm:crm` with mode `600`
- Never committed to git
- Never transferred by the deploy script

### Network Security

- Backend binds to `127.0.0.1` by default (localhost only)
- Frontend binds to `0.0.0.0:3001` (accessible via Tailscale)
- Access is via Tailscale (encrypted, authenticated)
- No ports exposed to public internet

## Troubleshooting

### Deploy Script Fails to Connect

```bash
# Test SSH connection
ssh raspberet 'echo OK'

# Check Tailscale
tailscale status

# Use IP directly
PI_HOST=100.x.x.x make deploy
```

### Services Won't Start

```bash
# Check logs
ssh raspberet 'sudo journalctl -u personalcrm-backend -n 50'
ssh raspberet 'sudo journalctl -u personalcrm-frontend -n 50'

# Check .env exists
ssh raspberet 'ls -la /srv/personalcrm/.env'

# Check permissions
ssh raspberet 'sudo stat /srv/personalcrm/.env'
```

### Frontend Module Error

The standalone build must include server.js:

```bash
# Verify deployment
ssh raspberet 'ls -la /srv/personalcrm/frontend/server.js'

# Rebuild and redeploy if missing
make deploy
```

### Database Issues

```bash
# Check database container
ssh raspberet 'docker ps | grep postgres'

# Check database service
ssh raspberet 'sudo systemctl status personalcrm-database'

# View database logs
ssh raspberet 'sudo journalctl -u personalcrm-database -n 50'
```

## Related Documentation

- [FIRST_TIME_PI_DEPLOYMENT.md](./FIRST_TIME_PI_DEPLOYMENT.md) - Step-by-step first deployment guide
- [LOCAL_MACOS_DEPLOYMENT.md](./LOCAL_MACOS_DEPLOYMENT.md) - Running locally on macOS
