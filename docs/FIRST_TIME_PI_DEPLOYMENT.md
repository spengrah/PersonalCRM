# First-Time Raspberry Pi Deployment Guide

**Last Updated**: January 2026
**Target**: Raspberry Pi 4/5 running Raspberry Pi OS (Bullseye or newer)
**Estimated Time**: 30-45 minutes

This guide walks you through deploying PersonalCRM to your Raspberry Pi for the first time. The workflow builds on your Mac and deploys to the Pi via rsync, keeping production secrets isolated on the Pi.

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Architecture Overview](#architecture-overview)
3. [Part 1: Pi Prerequisites](#part-1-pi-prerequisites)
4. [Part 2: First-Time Setup](#part-2-first-time-setup)
5. [Part 3: Configure Production Secrets](#part-3-configure-production-secrets)
6. [Part 4: Deploy](#part-4-deploy)
7. [Part 5: Verification](#part-5-verification)
8. [Part 6: Tailscale Setup (Optional)](#part-6-tailscale-setup-optional)
9. [Part 7: HTTPS via Tailscale Serve (Optional)](#part-7-https-via-tailscale-serve-optional)
10. [Regular Deploys](#regular-deploys)
11. [Troubleshooting](#troubleshooting)

---

## Prerequisites

### Hardware
- **Raspberry Pi 4 or 5** (4GB+ RAM recommended)
- **MicroSD card** (32GB+ recommended)
- **Reliable power supply**
- **Network connection** (Ethernet or WiFi)

### Your Mac
- Git, Go 1.24+, Bun (for building)
- SSH configured for passwordless login to Pi
- Pi accessible via Tailscale (recommended) or local network

### Pi on Tailscale
The deploy script expects your Pi to be accessible as `raspberet` on your Tailnet. You can customize this with the `PI_HOST` environment variable.

---

## Architecture Overview

```
Local (Mac)                          Pi (raspberet)
-----------------                    -----------------
.env (gitignored, dev)               /srv/personalcrm/.env (prod secrets)
backend/bin/crm-api (x86)            /srv/personalcrm/backend/bin/crm-api (ARM64)
frontend/.next/standalone/           /srv/personalcrm/frontend/
```

**Key Points:**
- Builds happen on Mac (faster, cross-compile to ARM64)
- Production secrets stay on Pi only (never in git)
- Deploys use rsync over SSH (fast, incremental)
- Frontend uses Next.js standalone mode (no node_modules on Pi)

**What Gets Deployed:**

| Source | Destination | Notes |
|--------|-------------|-------|
| `backend/bin/crm-api` | `/srv/personalcrm/backend/bin/` | ARM64 binary |
| `backend/migrations/` | `/srv/personalcrm/backend/migrations/` | SQL files |
| `frontend/.next/standalone/` | `/srv/personalcrm/frontend/` | Self-contained Next.js |
| `frontend/.next/static/` | `/srv/personalcrm/frontend/.next/static/` | Static assets |
| `frontend/public/` | `/srv/personalcrm/frontend/public/` | Public assets |
| `infra/docker-compose.yml` | `/srv/personalcrm/infra/` | DB config |
| `infra/*.service` | `/etc/systemd/system/` | Systemd units |

**NOT deployed:** `.env` files (secrets stay on Pi)

---

## Part 1: Pi Prerequisites

SSH into your Pi and install the required software.

### 1.1 Update System

```bash
sudo apt update && sudo apt upgrade -y
sudo apt install -y curl wget git vim htop
```

### 1.2 Install Docker

```bash
curl -sSL https://get.docker.com | sh
sudo usermod -aG docker $USER

# Log out and back in for group changes
exit
```

### 1.3 Install Node.js

The standalone Next.js server requires Node.js (not Bun).

```bash
# Install Node.js 20 LTS
curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash -
sudo apt install -y nodejs

# Verify
node --version  # Should show v20.x.x
```

### 1.4 Install Tailscale (Recommended)

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up
# Follow the authentication URL
```

### 1.5 Verify Prerequisites

```bash
echo "=== Checking Prerequisites ==="
command -v docker >/dev/null && echo "Docker: OK" || echo "Docker: MISSING"
command -v node >/dev/null && echo "Node.js: $(node --version)" || echo "Node.js: MISSING"
command -v curl >/dev/null && echo "curl: OK" || echo "curl: MISSING"
echo "=== Check complete ==="
```

---

## Part 2: First-Time Setup

Run these commands **from your Mac** (in the PersonalCRM directory).

### 2.1 Configure SSH Access

Ensure you can SSH to your Pi without a password:

```bash
# Test connection (adjust hostname if not using Tailscale)
ssh raspberet 'echo "Connection OK"'

# If this prompts for a password, set up SSH keys:
ssh-copy-id raspberet
```

### 2.2 Run Pi Setup Script

```bash
make setup-pi
```

This creates:
- The `crm` service user
- Directory structure at `/srv/personalcrm`
- Proper permissions

---

## Part 3: Configure Production Secrets

### 3.1 Generate Secrets

On your Mac, generate secure secrets:

```bash
echo "SESSION_SECRET: $(openssl rand -base64 32)"
echo "API_KEY: $(openssl rand -hex 32)"
echo "POSTGRES_PASSWORD: $(openssl rand -base64 32 | tr -d '/+=')"
```

**Save these securely** (password manager recommended).

### 3.2 Create .env on Pi

SSH to your Pi and create the production environment file:

```bash
ssh raspberet
sudo nano /srv/personalcrm/.env
```

Use this template (replace `<GENERATED_*>` with your secrets):

```bash
# Database
POSTGRES_USER=crm_user
POSTGRES_PASSWORD=<GENERATED_POSTGRES_PASSWORD>
POSTGRES_DB=personal_crm
POSTGRES_PORT=5432
DATABASE_URL=postgres://crm_user:<GENERATED_POSTGRES_PASSWORD>@localhost:5432/personal_crm?sslmode=disable

# Server (ports are defined in systemd service files)
NODE_ENV=production

# Authentication
SESSION_SECRET=<GENERATED_SESSION_SECRET>
API_KEY=<GENERATED_API_KEY>

# CORS (use your Pi's hostname or IP)
CORS_ALLOW_ALL=false
FRONTEND_URL=http://raspberet:3001

# Logging
LOG_LEVEL=info

# CRM Settings
CRM_ENV=production

# Feature Flags
ENABLE_VECTOR_SEARCH=false
ENABLE_TELEGRAM_BOT=false
ENABLE_CALENDAR_SYNC=false
ENABLE_TIME_TRACKING=false
NEXT_PUBLIC_ENABLE_TIME_TRACKING=false

# Scheduler (for reminder notifications)
SCHEDULER_ENABLED=false
SCHEDULER_CRON="0 8 * * *"
```

### 3.3 Secure the Secrets File

```bash
sudo chown crm:crm /srv/personalcrm/.env
sudo chmod 600 /srv/personalcrm/.env
```

---

## Part 4: Deploy

### 4.1 First Deploy

From your Mac:

```bash
make deploy
```

This will:
1. Fetch `API_KEY` and `NEXT_PUBLIC_ENABLE_TIME_TRACKING` from Pi (for frontend build)
2. Build backend for ARM64
3. Build frontend (standalone mode, with production env vars injected)
4. rsync files to Pi
5. Install systemd services
6. Restart services
7. Verify health checks

> **Note:** Frontend `NEXT_PUBLIC_*` variables are fetched from the Pi and injected at build time. This keeps production secrets on the Pi only—no need to configure them on your Mac.

### 4.2 Expected Output

```
=== PersonalCRM Deploy ===
Target: raspberet:/srv/personalcrm

Checking connectivity to raspberet...
OK

=== Building for ARM64 ===
Fetching production config from raspberet...
Building backend for ARM64...
Building frontend...

=== Deploying to raspberet ===
Deploying backend binary...
Deploying migrations...
Deploying frontend (standalone)...
Deploying infrastructure files...
Deploying systemd service files...

=== Restarting services ===

=== Verifying deployment ===
Backend:  OK
Frontend: OK

=== Deploy complete ===
Access your CRM at: http://raspberet:3001
```

---

## Part 5: Verification

### 5.1 Check Service Status

```bash
ssh raspberet 'sudo systemctl status personalcrm.target'
```

All services should show `active (running)`.

### 5.2 Health Check

```bash
ssh raspberet 'curl -s http://localhost:8080/health | jq'
```

Should return status "healthy".

### 5.3 Test API Authentication

```bash
# Get API key from Pi
API_KEY=$(ssh raspberet 'grep "^API_KEY=" /srv/personalcrm/.env | cut -d= -f2')

# Test without auth (should fail)
ssh raspberet 'curl -s http://localhost:8080/api/v1/contacts'
# Expected: {"success":false,"error":{"code":"MISSING_API_KEY"...}}

# Test with auth (should succeed)
ssh raspberet "curl -s -H 'X-API-Key: $API_KEY' http://localhost:8080/api/v1/contacts"
# Expected: {"success":true,"data":[],...}
```

### 5.4 Access Frontend

Open in browser: `http://raspberet:3001`

---

## Part 6: Tailscale Setup (Optional)

If not already done:

### On Pi
```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up
```

### On Mac
```bash
brew install tailscale
tailscale up
```

### Enable MagicDNS
1. Go to [Tailscale admin console](https://login.tailscale.com/admin/dns)
2. Enable **MagicDNS**
3. Access your Pi as `raspberet` (or your chosen hostname)

---

## Part 7: HTTPS via Tailscale Serve (Optional)

This enables secure HTTPS access via `https://raspberet.<tailnet>.ts.net` from any device on your Tailnet (Mac, iPhone, etc.).

### Why This Setup?

- **HTTPS everywhere**: Automatic TLS certificates from Tailscale
- **Single URL**: No port numbers needed
- **Same-origin requests**: Frontend and API share the same origin, avoiding CORS/mixed-content issues
- **Works on mobile**: Access from iPhone/iPad on your Tailnet

### Architecture

```
https://raspberet.<tailnet>.ts.net/
    │
    ▼
Tailscale Serve (HTTPS termination)
    │
    ▼
Caddy (:80) - Path-based routing
    ├── /api/*  → http://127.0.0.1:8080  (backend)
    └── /*      → http://127.0.0.1:3001  (frontend)
```

### 7.1 Install Caddy

SSH to your Pi:

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update
sudo apt install caddy
```

### 7.2 Configure Caddy

```bash
sudo nano /etc/caddy/Caddyfile
```

Replace contents with:

```
:80 {
    handle /api/* {
        reverse_proxy localhost:8080
    }

    handle {
        reverse_proxy localhost:3001
    }
}
```

Restart Caddy:

```bash
sudo systemctl restart caddy
sudo systemctl enable caddy
```

### 7.3 Configure Tailscale Serve

```bash
# Clear any existing config
sudo tailscale serve reset

# Proxy HTTPS to Caddy
sudo tailscale serve --bg https / http://127.0.0.1:80
```

Verify the configuration:

```bash
sudo tailscale serve status
```

### 7.4 Verify HTTPS Access

From any device on your Tailnet:

```
https://raspberet.<tailnet>.ts.net
```

Replace `<tailnet>` with your actual Tailnet name (e.g., `tail3df4a6`).

### Notes

- **Direct HTTP still works**: `http://raspberet:3001` continues to function
- **No code changes needed**: The frontend uses same-origin requests by default
- **Caddy is lightweight**: ~40MB RAM, minimal CPU usage
- **Rollback**: Run `sudo tailscale serve reset` to disable HTTPS access

---

## Regular Deploys

After initial setup, deploying updates is simple:

```bash
# Pull latest code
git pull origin main

# Deploy
make deploy
```

The deploy script is idempotent and handles:
- Building new binaries
- Syncing only changed files
- Restarting services
- Health verification

### Check Status Remotely

```bash
ssh raspberet 'sudo systemctl status personalcrm.target'
ssh raspberet 'sudo journalctl -u personalcrm-backend -f'  # Follow logs
```

---

## Troubleshooting

### Service Won't Start

```bash
# Check logs
ssh raspberet 'sudo journalctl -u personalcrm-backend -n 50'
ssh raspberet 'sudo journalctl -u personalcrm-frontend -n 50'

# Check .env file exists and is readable
ssh raspberet 'sudo ls -la /srv/personalcrm/.env'
```

### Frontend "Cannot find module" Error

Ensure the standalone build was deployed correctly:

```bash
ssh raspberet 'ls -la /srv/personalcrm/frontend/server.js'
```

If missing, rebuild and redeploy:
```bash
make deploy
```

### Database Connection Failed

```bash
# Check database service
ssh raspberet 'sudo systemctl status personalcrm-database'
ssh raspberet 'docker ps | grep postgres'

# Check DATABASE_URL matches POSTGRES_PASSWORD
ssh raspberet 'grep -E "(DATABASE_URL|POSTGRES_PASSWORD)" /srv/personalcrm/.env'
```

### Permission Denied Errors

```bash
# Fix ownership
ssh raspberet 'sudo chown -R crm:crm /srv/personalcrm'
ssh raspberet 'sudo chmod 750 /srv/personalcrm'
ssh raspberet 'sudo chmod 600 /srv/personalcrm/.env'
```

### Deploy Script Can't Connect

```bash
# Test SSH connection
ssh raspberet 'echo OK'

# Check Tailscale status
tailscale status

# Use IP address if DNS not working
PI_HOST=100.x.x.x make deploy
```

---

## Environment Separation

| Environment | Location | Secrets | Purpose |
|-------------|----------|---------|---------|
| Development | `.env` (Mac, gitignored) | Dev values | Local development |
| Testing | `.env.example.testing` | Test values | CI/automated tests |
| Production | `/srv/personalcrm/.env` (Pi only) | Real secrets | Live deployment |

---

## Success Checklist

- [ ] Pi prerequisites installed (Docker, Node.js)
- [ ] SSH access configured (passwordless)
- [ ] `make setup-pi` completed
- [ ] Production secrets in `/srv/personalcrm/.env`
- [ ] Secrets file permissions set (600, owned by crm)
- [ ] `make deploy` completed successfully
- [ ] Health check returns "healthy"
- [ ] Frontend accessible at http://raspberet:3001
- [ ] API authentication working

---

**Congratulations!** Your PersonalCRM is now running on your Raspberry Pi with:
- Production-grade systemd services
- API key authentication
- Auto-start on boot
- Secure remote access via Tailscale
