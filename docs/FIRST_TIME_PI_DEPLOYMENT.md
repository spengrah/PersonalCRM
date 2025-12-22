# First-Time Raspberry Pi Deployment Guide

**Last Updated**: December 2025
**Target**: Raspberry Pi 4/5 running Raspberry Pi OS (Bullseye or newer)
**Estimated Time**: 45-60 minutes

This guide walks you through deploying PersonalCRM to your Raspberry Pi for the first time, from a fresh Pi to a running, secured application accessible via Tailscale.

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Part 1: Prepare Your Local Machine (macOS)](#part-1-prepare-your-local-machine-macos)
3. [Part 2: Prepare Your Raspberry Pi](#part-2-prepare-your-raspberry-pi)
4. [Part 3: Generate Secrets and Configure](#part-3-generate-secrets-and-configure)
5. [Part 4: Deploy to Pi](#part-4-deploy-to-pi)
6. [Part 5: Verification and Testing](#part-5-verification-and-testing)
7. [Part 6: Tailscale Setup (Optional)](#part-6-tailscale-setup-optional)
8. [Troubleshooting](#troubleshooting)
9. [Next Steps](#next-steps)

---

## Prerequisites

### Hardware
- **Raspberry Pi 4 or 5** (4GB+ RAM recommended)
- **MicroSD card** (32GB+ recommended)
- **Reliable power supply** (official Pi power supply recommended)
- **Network connection** (Ethernet recommended for initial setup)

### Software (Your Mac)
- Git installed
- SSH client (built into macOS)
- OpenSSL (built into macOS)
- Text editor (nano, vim, VS Code, etc.)

### Accounts
- GitHub account with access to spengrah/PersonalCRM repository
- Tailscale account (optional, for remote access)

---

## Part 1: Prepare Your Local Machine (macOS)

### 1.1 Clone the Repository (if not already done)

```bash
# Clone the repository
cd ~/Projects  # or wherever you keep your code
git clone https://github.com/spengrah/PersonalCRM.git
cd PersonalCRM

# Checkout latest main branch
git checkout main
git pull origin main
```

### 1.2 Generate Production Secrets

You'll need to generate secure secrets for production. Keep these in a secure location (password manager recommended).

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

**⚠️ IMPORTANT**: Save these values immediately! You'll need them in the next steps.

**Recommended**: Store in your password manager with these labels:
- `PersonalCRM Production - SESSION_SECRET`
- `PersonalCRM Production - API_KEY`
- `PersonalCRM Production - POSTGRES_PASSWORD`

### 1.3 Create Production Environment File

Create a production environment file locally that you'll copy to the Pi:

```bash
# Create .env from the production template
cp .env.example.production .env

# Edit the file
nano .env
```

**Update these critical values** (replace `<generated-*>` with your actual generated secrets):

```bash
# Database
DATABASE_URL=postgres://crm_user:<POSTGRES_PASSWORD>@localhost:5432/personal_crm?sslmode=disable
POSTGRES_PASSWORD=<POSTGRES_PASSWORD>

# Server
PORT=8080
NODE_ENV=production
HOST=127.0.0.1

# Authentication & Session
SESSION_SECRET=<SESSION_SECRET>
API_KEY=<API_KEY>

# CORS (localhost only for Tailscale deployment)
CORS_ALLOW_ALL=false
FRONTEND_URL=http://localhost:3001

# Logging
LOG_LEVEL=info

# Feature Flags (keep disabled for first deployment)
ENABLE_VECTOR_SEARCH=false
ENABLE_TELEGRAM_BOT=false
ENABLE_CALENDAR_SYNC=false

# CRM Environment
CRM_ENV=production
```

**Save the file** (Ctrl+O, Enter, Ctrl+X in nano)

### 1.4 Create Frontend Environment File

```bash
# Create frontend .env.local
cat > frontend/.env.local <<EOF
# API Authentication (must match backend API_KEY)
NEXT_PUBLIC_API_KEY=<API_KEY>
EOF
```

Replace `<API_KEY>` with the same API key you generated earlier.

---

## Part 2: Prepare Your Raspberry Pi

### 2.1 Initial Pi Setup

If you haven't already set up your Pi:

1. **Flash Raspberry Pi OS** to your SD card using [Raspberry Pi Imager](https://www.raspberrypi.com/software/)
   - Choose "Raspberry Pi OS (64-bit)" (Lite or Desktop)
   - Configure hostname: `personalcrm` (or your preference)
   - Enable SSH
   - Set username/password
   - Configure WiFi (if not using Ethernet)

2. **Boot your Pi** and find its IP address:
   ```bash
   # On your Mac, find the Pi on your network
   ping personalcrm.local
   # Or check your router's DHCP leases
   ```

### 2.2 SSH into Your Pi

```bash
# From your Mac
ssh pi@personalcrm.local
# Or use the IP address directly
ssh pi@192.168.1.XXX
```

**All commands in sections 2.3-2.6 are run ON THE PI via SSH**

### 2.3 Update System

```bash
# Update package lists and upgrade
sudo apt update && sudo apt upgrade -y

# Install essential tools
sudo apt install -y curl wget git vim nano htop
```

### 2.4 Install Docker and Docker Compose

```bash
# Install Docker
curl -sSL https://get.docker.com | sh

# Add your user to docker group
sudo usermod -aG docker $USER

# Install Docker Compose (if not included)
sudo apt install -y docker-compose

# Verify installation
docker --version
docker-compose --version

# Log out and back in for group changes to take effect
exit
```

**SSH back in** after logging out:
```bash
ssh pi@personalcrm.local
```

### 2.5 Install Go 1.23

```bash
# Download Go 1.23.x for ARM64 (check https://go.dev/dl/ for latest 1.23.x)
cd ~
wget https://go.dev/dl/go1.23.4.linux-arm64.tar.gz

# Extract to /usr/local
sudo tar -C /usr/local -xzf go1.23.4.linux-arm64.tar.gz

# Add to PATH
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# Verify installation
go version
# Should output: go version go1.23.4 linux/arm64
```

### 2.6 Install Bun

```bash
# Install Bun
curl -fsSL https://bun.sh/install | bash

# Add to PATH (should be automatic, but verify)
source ~/.bashrc

# Create system-wide symlink
sudo ln -s ~/.bun/bin/bun /usr/local/bin/bun

# Verify installation
bun --version
# Should output: 1.2.x or newer
```

### 2.7 Verify All Prerequisites

```bash
# Run this verification command
echo "=== Checking Prerequisites ==="
command -v docker >/dev/null 2>&1 && echo "✓ Docker installed" || echo "✗ Docker missing"
command -v docker-compose >/dev/null 2>&1 && echo "✓ Docker Compose installed" || echo "✗ Docker Compose missing"
command -v go >/dev/null 2>&1 && echo "✓ Go installed" || echo "✗ Go missing"
command -v bun >/dev/null 2>&1 && echo "✓ Bun installed" || echo "✗ Bun missing"
command -v git >/dev/null 2>&1 && echo "✓ Git installed" || echo "✗ Git missing"
echo "=== Check complete ==="
```

**All items should show ✓**. If any show ✗, revisit that installation step.

---

## Part 3: Generate Secrets and Configure

### 3.1 Copy Environment Files to Pi

**On your Mac**, copy the production environment files to your Pi:

```bash
# From your PersonalCRM directory on Mac
scp .env pi@personalcrm.local:/tmp/
scp frontend/.env.local pi@personalcrm.local:/tmp/
```

### 3.2 Clone Repository on Pi

**On the Pi** (via SSH):

```bash
# Clone the repository to your home directory
cd ~
git clone https://github.com/spengrah/PersonalCRM.git
cd PersonalCRM

# Checkout latest main
git checkout main
git pull origin main
```

### 3.3 Set Up Environment Files

**On the Pi**:

```bash
# Copy production environment file
cd ~/PersonalCRM
cp /tmp/.env .env

# Verify critical values are set
echo "Checking critical environment variables..."
grep "SESSION_SECRET=" .env | grep -v "dev-session" && echo "✓ SESSION_SECRET set" || echo "✗ SESSION_SECRET still has dev value"
grep "API_KEY=" .env | grep -v "dev-api-key" && echo "✓ API_KEY set" || echo "✗ API_KEY still has dev value"
grep "POSTGRES_PASSWORD=" .env | grep -v "crm_password" && echo "✓ POSTGRES_PASSWORD set" || echo "✗ POSTGRES_PASSWORD still has dev value"
```

**If any show ✗**, edit `.env` and update with your generated secrets:
```bash
nano .env
# Update the values, save and exit
```

### 3.4 Validate Environment Configuration

**IMPORTANT**: Use the config package to validate your environment file and identify any missing variables:

```bash
# Test config validation by attempting to build
cd ~/PersonalCRM/backend

# Set environment to production for validation
export NODE_ENV=production

# Run the app with validation (it will fail fast if config is invalid)
source ../.env
go run cmd/crm-api/main.go --help 2>&1 | head -20

# Or use this validation script
cat > ~/validate-env.sh <<'EOF'
#!/bin/bash
cd ~/PersonalCRM
source .env

echo "=== Environment Validation ==="
echo ""

# Check all required production variables
MISSING=()

# Required in production
[[ -z "$DATABASE_URL" ]] && MISSING+=("DATABASE_URL")
[[ "$NODE_ENV" == "production" && -z "$SESSION_SECRET" ]] && MISSING+=("SESSION_SECRET")
[[ "$NODE_ENV" == "production" && -z "$API_KEY" ]] && MISSING+=("API_KEY")
[[ -z "$POSTGRES_PASSWORD" ]] && MISSING+=("POSTGRES_PASSWORD")

# Check for dev values in production
[[ "$NODE_ENV" == "production" && "$SESSION_SECRET" == *"dev-session"* ]] && echo "⚠  SESSION_SECRET still has dev value"
[[ "$NODE_ENV" == "production" && "$API_KEY" == *"dev-api-key"* ]] && echo "⚠  API_KEY still has dev value"
[[ "$POSTGRES_PASSWORD" == "crm_password" ]] && echo "⚠  POSTGRES_PASSWORD still has default value"

# Report missing
if [ ${#MISSING[@]} -ne 0 ]; then
    echo "✗ Missing required variables:"
    for var in "${MISSING[@]}"; do
        echo "  - $var"
    done
    echo ""
    echo "Please add these to .env before deploying"
    exit 1
else
    echo "✓ All required variables present"
fi

# Check recommended variables
echo ""
echo "Recommended settings:"
[[ "$CORS_ALLOW_ALL" == "false" ]] && echo "✓ CORS_ALLOW_ALL=false (secure)" || echo "⚠  Consider setting CORS_ALLOW_ALL=false"
[[ "$LOG_LEVEL" == "info" || "$LOG_LEVEL" == "warn" ]] && echo "✓ LOG_LEVEL appropriate for production" || echo "⚠  Consider LOG_LEVEL=info for production"
[[ "$NODE_ENV" == "production" ]] && echo "✓ NODE_ENV=production" || echo "⚠  Should set NODE_ENV=production"

echo ""
echo "=== Validation complete ==="
EOF

chmod +x ~/validate-env.sh
~/validate-env.sh
```

**Expected output**: All required variables present with no missing items.

**If validation fails**:
1. The script will tell you exactly which variables are missing
2. Edit `.env` to add the missing variables:
   ```bash
   nano .env
   ```
3. Run validation again until it passes

### 3.5 Set Up Frontend Environment

```bash
# Copy frontend environment file
cp /tmp/.env.local frontend/.env.local

# Verify API key is set
grep "NEXT_PUBLIC_API_KEY=" frontend/.env.local | grep -v "dev-api-key" && echo "✓ Frontend API_KEY set" || echo "✗ Frontend API_KEY still has dev value"
```

**If it shows ✗**, edit and update:
```bash
nano frontend/.env.local
# Update NEXT_PUBLIC_API_KEY to match your backend API_KEY
```

### 3.6 Final Pre-Deployment Check

**Before running the installer**, verify your configuration one more time:

```bash
cd ~/PersonalCRM

# Run the validation script
~/validate-env.sh

# Compare with example to ensure nothing is missed
echo ""
echo "=== Comparing with .env example ==="
diff -u <(grep "^[A-Z]" .env | cut -d= -f1 | sort) <(grep "^[A-Z]" .env | cut -d= -f1 | sort) || true

# Check that frontend has API key
echo ""
echo "=== Frontend Configuration ==="
[[ -f frontend/.env.local ]] && echo "✓ frontend/.env.local exists" || echo "✗ frontend/.env.local missing"
grep -q "NEXT_PUBLIC_API_KEY=" frontend/.env.local 2>/dev/null && echo "✓ NEXT_PUBLIC_API_KEY set" || echo "✗ NEXT_PUBLIC_API_KEY missing"
```

**All checks should pass** before proceeding to deployment.

---

## Part 4: Deploy to Pi

### 4.1 Run the Installation Script

**On the Pi**:

```bash
cd ~/PersonalCRM

# Make the install script executable
chmod +x infra/install-systemd.sh

# Run the installation (requires sudo)
sudo infra/install-systemd.sh
```

**What this script does**:
1. Creates `crm` service user
2. Creates `/opt/personalcrm` directory structure
3. Builds backend binary
4. Builds frontend static files
5. Copies files to `/opt/personalcrm`
6. Copies environment files with proper permissions
7. Installs systemd service files
8. Reloads systemd daemon

**Expected output**: Should complete with "Installation complete!" message

### 4.2 Start Services

```bash
# Enable services to start on boot
sudo systemctl enable personalcrm.target

# Start all services
sudo systemctl start personalcrm.target
```

### 4.3 Check Service Status

```bash
# Check overall target status
sudo systemctl status personalcrm.target

# Check individual services
sudo systemctl status personalcrm-database.service
sudo systemctl status personalcrm-backend.service
sudo systemctl status personalcrm-frontend.service
```

**Expected**: All services should show `active (running)` in green.

**If any service fails**, check logs:
```bash
# View backend logs
sudo journalctl -u personalcrm-backend -n 50 --no-pager

# View frontend logs
sudo journalctl -u personalcrm-frontend -n 50 --no-pager

# View database logs
sudo journalctl -u personalcrm-database -n 50 --no-pager
```

See [Troubleshooting](#troubleshooting) section if services fail to start.

---

## Part 5: Verification and Testing

### 5.1 Health Check

**On the Pi**:

```bash
# Check backend health endpoint (should be publicly accessible)
curl http://localhost:8080/health

# Expected output: JSON with status "healthy"
# {
#   "status": "healthy",
#   "timestamp": "2024-12-21T...",
#   "version": {...},
#   "components": {
#     "database": {"status": "healthy", "response_time": "..."}
#   },
#   "system": {...}
# }
```

**If health check fails**, see [Troubleshooting](#troubleshooting).

### 5.2 Test API Authentication

**On the Pi**:

```bash
# Try accessing API without authentication (should fail)
curl -i http://localhost:8080/api/v1/contacts

# Expected: HTTP/1.1 401 Unauthorized
# {"success":false,"error":{"code":"MISSING_API_KEY","message":"..."}}

# Try with correct API key (should succeed)
API_KEY=$(grep "^API_KEY=" /opt/personalcrm/.env | cut -d= -f2)
curl -i -H "X-API-Key: $API_KEY" http://localhost:8080/api/v1/contacts

# Expected: HTTP/1.1 200 OK
# {"success":true,"data":[],"pagination":{...}}
```

**If authentication doesn't work**, verify:
1. API_KEY is set in `/opt/personalcrm/.env`
2. Backend service restarted after setting API_KEY
3. Check backend logs for errors

### 5.3 Test Frontend

**On the Pi**:

```bash
# Check frontend is serving
curl -I http://localhost:3001

# Expected: HTTP/1.1 200 OK
```

**From your Mac**, access the frontend:

```bash
# Find your Pi's IP address
ping personalcrm.local
# Note the IP address (e.g., 192.168.1.50)
```

Open browser and navigate to: `http://192.168.1.50:3001`

**Expected**: PersonalCRM login or home page loads

**If frontend doesn't load**:
- Check frontend service status: `sudo systemctl status personalcrm-frontend`
- Check frontend logs: `sudo journalctl -u personalcrm-frontend -n 50`
- Verify frontend build: `ls -la /opt/personalcrm/frontend/.next/`

### 5.4 Create Test Contact

**In your browser** (at `http://192.168.1.50:3001`):

1. Navigate to Contacts page
2. Click "Add Contact"
3. Fill in:
   - Name: "Test User"
   - Email: "test@example.com"
   - Cadence: "Weekly"
4. Save

**Expected**: Contact appears in the list, no authentication errors

**If you see authentication errors**:
- Verify `frontend/.env.local` has correct `NEXT_PUBLIC_API_KEY`
- Frontend was rebuilt after setting the key
- Check browser console for errors (F12 → Console tab)

---

## Part 6: Tailscale Setup (Optional)

Tailscale provides secure remote access to your Pi without port forwarding or exposing services to the public internet.

### 6.1 Install Tailscale on Pi

**On the Pi**:

```bash
# Install Tailscale
curl -fsSL https://tailscale.com/install.sh | sh

# Start Tailscale
sudo tailscale up

# Follow the authentication URL shown in the terminal
# Open it in your browser and approve the device
```

### 6.2 Enable MagicDNS

1. Go to [Tailscale admin console](https://login.tailscale.com/admin/dns)
2. Enable **MagicDNS**
3. Your Pi will be accessible at: `personalcrm` (or whatever you named it)

### 6.3 Install Tailscale on Your Mac

```bash
# Download from https://tailscale.com/download/mac
# Or use Homebrew:
brew install tailscale

# Start Tailscale
sudo tailscale up
```

### 6.4 Access PersonalCRM via Tailscale

**From your Mac** (or any device on your Tailscale network):

```bash
# Ping your Pi via Tailscale
ping personalcrm

# Access the application
open http://personalcrm:3001
```

**Expected**: PersonalCRM loads via Tailscale (secure, encrypted connection)

### 6.5 Update Frontend CORS (Optional)

If accessing via Tailscale hostname, you may want to update CORS settings:

**On the Pi**:

```bash
# Edit .env
sudo nano /opt/personalcrm/.env

# Update FRONTEND_URL to Tailscale hostname
FRONTEND_URL=http://personalcrm:3001

# Save and restart backend
sudo systemctl restart personalcrm-backend.service
```

---

## Troubleshooting

### Service Won't Start

**Symptom**: Service shows `failed` status

**Diagnosis**:
```bash
# Check service status
sudo systemctl status personalcrm-backend.service

# View detailed logs
sudo journalctl -u personalcrm-backend -n 100 --no-pager
```

**Common issues**:

1. **Missing environment variables**
   - Check `/opt/personalcrm/.env` exists and has all required values
   - Verify `API_KEY`, `SESSION_SECRET`, `DATABASE_URL` are set

2. **Database connection failed**
   - Check database service is running: `sudo systemctl status personalcrm-database`
   - Verify PostgreSQL password in DATABASE_URL matches POSTGRES_PASSWORD
   - Check database logs: `sudo journalctl -u personalcrm-database -n 50`

3. **Port already in use**
   - Check what's using port 8080: `sudo lsof -i :8080`
   - Change PORT in `.env` if needed

4. **Binary not found or permissions**
   - Verify binary exists: `ls -la /opt/personalcrm/backend/bin/crm-api`
   - Should be executable: `-rwxr-xr-x`

### API Returns 401 Unauthorized

**Symptom**: All API requests return 401, even with API key

**Diagnosis**:
```bash
# Check API_KEY is set
grep "API_KEY=" /opt/personalcrm/.env

# Check frontend has matching key
grep "NEXT_PUBLIC_API_KEY=" /opt/personalcrm/frontend/.env.local

# Test API directly
API_KEY=$(grep "^API_KEY=" /opt/personalcrm/.env | cut -d= -f2)
curl -H "X-API-Key: $API_KEY" http://localhost:8080/api/v1/contacts
```

**Solutions**:
1. Ensure API_KEY is not empty or still has dev value
2. Frontend and backend keys must match exactly
3. Rebuild frontend after changing API key:
   ```bash
   cd ~/PersonalCRM/frontend
   bun run build
   sudo cp -r .next /opt/personalcrm/frontend/
   sudo systemctl restart personalcrm-frontend
   ```

### Database Migrations Fail

**Symptom**: Backend starts but can't connect to database

**Diagnosis**:
```bash
# Check database container is running
docker ps | grep postgres

# Check database logs
sudo journalctl -u personalcrm-database -n 50

# Try connecting to database manually
docker exec -it crm-postgres psql -U crm_user -d personal_crm
```

**Solutions**:
1. Verify POSTGRES_PASSWORD in `.env` matches in docker-compose
2. Check DATABASE_URL has correct password
3. Ensure database had time to initialize (wait 10 seconds after starting)
4. Check migrations directory exists: `ls -la /opt/personalcrm/backend/migrations/`

### Frontend Shows Connection Error

**Symptom**: Frontend loads but can't fetch data

**Diagnosis**:
1. Check browser console (F12 → Console tab)
2. Look for CORS errors or network errors
3. Check API base URL

**Solutions**:
1. Verify backend is running: `curl http://localhost:8080/health`
2. Check CORS settings in `.env`:
   ```bash
   CORS_ALLOW_ALL=false
   FRONTEND_URL=http://localhost:3001
   ```
3. If accessing via Tailscale, update FRONTEND_URL to Tailscale hostname
4. Restart backend after CORS changes

### Can't Access from Mac

**Symptom**: Can access on Pi (localhost) but not from Mac

**Diagnosis**:
```bash
# On Pi, check what address services bind to
sudo netstat -tlnp | grep :8080
sudo netstat -tlnp | grep :3001
```

**Solutions**:

1. **If binding to 127.0.0.1** (localhost only):
   - This is default and secure
   - Access via Tailscale instead (recommended)
   - OR change HOST in `.env` to `0.0.0.0` (less secure):
     ```bash
     sudo nano /opt/personalcrm/.env
     # Change: HOST=0.0.0.0
     sudo systemctl restart personalcrm-backend
     ```

2. **Firewall blocking**:
   ```bash
   # Check if UFW is active
   sudo ufw status

   # If active, allow ports
   sudo ufw allow 8080
   sudo ufw allow 3001
   ```

3. **Use Tailscale** (recommended):
   - Secure, encrypted access
   - No firewall configuration needed
   - See [Part 6](#part-6-tailscale-setup-optional)

### Out of Memory or High CPU

**Symptom**: Pi becomes slow or services crash

**Diagnosis**:
```bash
# Check memory usage
free -h

# Check CPU usage
htop

# Check service resource usage
systemd-cgtop
```

**Solutions**:
1. Pi 4 with 2GB RAM may struggle - 4GB+ recommended
2. Increase swap space:
   ```bash
   sudo dphys-swapfile swapoff
   sudo nano /etc/dphys-swapfile
   # Change CONF_SWAPSIZE=2048
   sudo dphys-swapfile setup
   sudo dphys-swapfile swapon
   ```
3. Review resource limits in service files if needed

---

## Next Steps

### Monitoring and Maintenance

1. **Set up log rotation** (prevent disk fill):
   ```bash
   sudo nano /etc/systemd/journald.conf
   # Uncomment and set:
   # SystemMaxUse=100M
   sudo systemctl restart systemd-journald
   ```

2. **Set up automatic updates**:
   ```bash
   sudo apt install unattended-upgrades
   sudo dpkg-reconfigure -plow unattended-upgrades
   ```

3. **Monitor service health**:
   ```bash
   # Create a simple health check script
   cat > ~/check-health.sh <<'EOF'
   #!/bin/bash
   echo "=== PersonalCRM Health Check ==="
   echo ""
   echo "Services:"
   systemctl is-active personalcrm-database && echo "✓ Database" || echo "✗ Database"
   systemctl is-active personalcrm-backend && echo "✓ Backend" || echo "✗ Backend"
   systemctl is-active personalcrm-frontend && echo "✓ Frontend" || echo "✗ Frontend"
   echo ""
   echo "API Health:"
   curl -s http://localhost:8080/health | jq -r '.status' | grep -q "healthy" && echo "✓ API healthy" || echo "✗ API unhealthy"
   EOF
   chmod +x ~/check-health.sh
   ```

### Backup Strategy

See `docs/BACKUP_STRATEGY.md` (if exists) or create one with:
- Database backup schedule (pg_dump)
- Environment file backup
- Recovery procedures

### API Key Rotation

Rotate your API key every 90 days:

```bash
# Generate new key
NEW_API_KEY=$(openssl rand -hex 32)
echo "New API_KEY: $NEW_API_KEY"

# Update backend .env
sudo nano /opt/personalcrm/.env
# Update API_KEY=<new-key>

# Update frontend .env.local
sudo nano /opt/personalcrm/frontend/.env.local
# Update NEXT_PUBLIC_API_KEY=<new-key>

# Rebuild frontend
cd ~/PersonalCRM/frontend
bun run build
sudo cp -r .next /opt/personalcrm/frontend/

# Restart services
sudo systemctl restart personalcrm-backend
sudo systemctl restart personalcrm-frontend

# Verify
curl -H "X-API-Key: $NEW_API_KEY" http://localhost:8080/api/v1/contacts
```

### Updates and Upgrades

When new features are released:

```bash
# On Pi, pull latest code
cd ~/PersonalCRM
git pull origin main

# Rebuild and redeploy
sudo infra/install-systemd.sh

# Restart services
sudo systemctl restart personalcrm.target
```

### Enable Additional Features

After stable deployment, consider enabling:

1. **Telegram Bot** (if desired):
   - Get bot token from @BotFather on Telegram
   - Add to `.env`: `TELEGRAM_BOT_TOKEN=...` and `ENABLE_TELEGRAM_BOT=true`
   - Restart backend

2. **Vector Search** (if using AI features):
   - Add to `.env`: `ENABLE_VECTOR_SEARCH=true`
   - Ensure ANTHROPIC_API_KEY is set
   - Restart backend

---

## Success Checklist

Mark each item as you complete it:

- [ ] Pi hardware ready and booted
- [ ] All prerequisites installed (Docker, Go, Bun)
- [ ] Production secrets generated and saved securely
- [ ] Repository cloned on Pi
- [ ] Environment files configured with production secrets
- [ ] Installation script completed successfully
- [ ] All services running (`systemctl status personalcrm.target` shows active)
- [ ] Health check returns "healthy" status
- [ ] API authentication working (401 without key, 200 with key)
- [ ] Frontend accessible in browser
- [ ] Test contact created successfully
- [ ] Tailscale installed and configured (if using)
- [ ] Can access via Tailscale from other devices
- [ ] Monitoring/health check script created
- [ ] Documented where secrets are stored
- [ ] Calendar reminder set for API key rotation (90 days)

---

## Questions or Issues?

If you encounter problems not covered in this guide:

1. Check service logs: `sudo journalctl -u personalcrm-backend -n 100`
2. Review GitHub issues: https://github.com/spengrah/PersonalCRM/issues
3. Check systemd service files: `/etc/systemd/system/personalcrm-*.service`
4. Verify all environment variables are set correctly
5. Ensure all prerequisites are properly installed

**Common support resources**:
- Raspberry Pi Forums: https://forums.raspberrypi.com/
- Tailscale Documentation: https://tailscale.com/kb/
- Docker Documentation: https://docs.docker.com/

---

**Congratulations!** 🎉

If you've completed all the steps and checks above, your PersonalCRM is now running on your Raspberry Pi with:
- ✅ Production-grade systemd services
- ✅ API key authentication
- ✅ Secure database with migrations
- ✅ Auto-start on boot
- ✅ Optional secure remote access via Tailscale

Enjoy your self-hosted, privacy-focused CRM!
