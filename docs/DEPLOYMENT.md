# PersonalCRM Deployment

> ⚠️ **Outdated — native rsync path retired.** This document describes the
> legacy build-on-Mac + rsync-to-Pi flow (systemd **system** units, local
> binaries), which has been replaced. Prod now runs rootless Podman Quadlets
> and deploys via promotion: merge to `develop`, then `make promote`
> fast-forwards `main`, which triggers the prod deploy on the self-hosted
> runner (`.github/workflows/deploy-prod.yml` → `scripts/deploy-artifact.sh`).
> The `scripts/deploy.sh` / `scripts/deploy-all.sh` scripts and the
> `make deploy` / `deploy-pi` / `deploy-all` targets no longer exist. The
> sections below are retained for historical context only.

This document describes the (legacy) deployment architecture and workflow for PersonalCRM.

> **Note:** This guide uses `<pi-hostname>` as a placeholder for your Pi's hostname.
> Replace it with your actual hostname. The remaining Pi helper scripts
> (`setup-pi.sh`, `backup-db.sh`, `restore-db.sh`) use the `PI_HOST` env var
> (default: `raspberry-pi`).

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
Local (Mac)                          Pi (<pi-hostname>)
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
ssh <pi-hostname> 'sudo nano /srv/personalcrm/.env'

# 3. Deploy via the promotion flow (see banner above): merge to develop,
#    then `make promote`.
```

### Regular Deploys

Deploys now run through the promotion flow: merge to `develop`, then
`make promote` fast-forwards `main`, which triggers the prod deploy on the
self-hosted runner. The runner (via `scripts/deploy-artifact.sh`):

1. Pulls the prebuilt `:<sha>` backend/frontend images from GHCR
2. Runs migrations (`crm-admin --migrate`) against the live DB
3. Rewrites the Quadlet `Image=` pins and restarts the containers
4. Verifies health checks (rolling back on failure)

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
ssh <pi-hostname> 'sudo systemctl start personalcrm.target'

# Stop all services
ssh <pi-hostname> 'sudo systemctl stop personalcrm.target'

# Restart all services
ssh <pi-hostname> 'sudo systemctl restart personalcrm.target'

# Check status
ssh <pi-hostname> 'sudo systemctl status personalcrm.target'

# View logs
ssh <pi-hostname> 'sudo journalctl -u personalcrm-backend -f'
ssh <pi-hostname> 'sudo journalctl -u personalcrm-frontend -f'
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
- `.env.example.staging` - Staging config (production cadence semantics)
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

- Backend binds to `0.0.0.0:8080` (needed for Caddy reverse proxy)
- Frontend binds to `0.0.0.0:3001` (accessible via Tailscale)
- Access is via Tailscale (encrypted, authenticated)
- No ports exposed to public internet

### Same-Origin Requests (Optional HTTPS)

For HTTPS access via Tailscale Serve, the frontend can use same-origin (relative) API requests:
- Frontend calls `/api/v1/...` instead of `http://host:8080/api/v1/...`
- Caddy reverse proxy routes `/api/*` to backend, `/*` to frontend
- Eliminates CORS and mixed-content issues

See [FIRST_TIME_PI_DEPLOYMENT.md Part 7](./FIRST_TIME_PI_DEPLOYMENT.md#part-7-https-via-tailscale-serve-optional) for setup instructions.

## Database Pool Configuration

The backend uses Pi-optimized connection pool defaults that work well for single-user deployments:

| Setting | Default | Description |
|---------|---------|-------------|
| `DB_MAX_CONNS` | 15 | Maximum pool size (must be >= `RIVER_WORKER_CONCURRENCY` + 3) |
| `DB_MIN_CONNS` | 2 | Minimum idle connections (keeps connections warm) |
| `DB_MAX_CONN_IDLE_TIME` | 5m | Idle connection timeout |
| `DB_MAX_CONN_LIFETIME` | 30m | Maximum connection age |
| `DB_HEALTH_CHECK_PERIOD` | 30s | Health check interval |

These defaults are suitable for a Raspberry Pi 4/5. You typically don't need to change them, but they can be customized in `/srv/personalcrm/.env` if needed.

`DB_MAX_CONNS` must exceed `RIVER_WORKER_CONCURRENCY` by at least 3 or the backend will refuse to start. River itself uses ~3 internal connections (leader election, notifier, job completer), and web request traffic needs at least one free connection beyond that — without the headroom, job workers will starve HTTP requests.

For high-memory systems, you might increase `DB_MAX_CONNS`:
```bash
# Example for 8GB+ systems
DB_MAX_CONNS=25
DB_MIN_CONNS=3
```

## Worker Queue Configuration

The backend uses [River](https://riverqueue.com) for background jobs. See issue #180 and `.ai/spec/event-bus-foundation.md`.

| Setting | Default | Description |
|---------|---------|-------------|
| `RIVER_WORKER_CONCURRENCY` | 10 | Maximum concurrent job workers; `DB_MAX_CONNS` must be at least this + 3 |

## Troubleshooting

### Deploy Script Fails to Connect

```bash
# Test SSH connection
ssh <pi-hostname> 'echo OK'

# Check Tailscale
tailscale status

# Use IP directly with a Pi helper script (e.g. backup-db.sh)
PI_HOST=100.x.x.x ./scripts/backup-db.sh
```

### Services Won't Start

```bash
# Check logs
ssh <pi-hostname> 'sudo journalctl -u personalcrm-backend -n 50'
ssh <pi-hostname> 'sudo journalctl -u personalcrm-frontend -n 50'

# Check .env exists
ssh <pi-hostname> 'ls -la /srv/personalcrm/.env'

# Check permissions
ssh <pi-hostname> 'sudo stat /srv/personalcrm/.env'
```

### Frontend Module Error

The standalone build must include server.js:

```bash
# Verify deployment
ssh <pi-hostname> 'ls -la /srv/personalcrm/frontend/server.js'

# Re-promote if missing (rebuilds the image and redeploys via the runner)
make promote
```

### Database Issues

```bash
# Check database container
ssh <pi-hostname> 'docker ps | grep postgres'

# Check database service
ssh <pi-hostname> 'sudo systemctl status personalcrm-database'

# View database logs
ssh <pi-hostname> 'sudo journalctl -u personalcrm-database -n 50'
```

## Related Documentation

- [FIRST_TIME_PI_DEPLOYMENT.md](./FIRST_TIME_PI_DEPLOYMENT.md) - Step-by-step first deployment guide
- [LOCAL_MACOS_DEPLOYMENT.md](./LOCAL_MACOS_DEPLOYMENT.md) - Running locally on macOS
