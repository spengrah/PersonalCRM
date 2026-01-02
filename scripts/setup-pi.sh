#!/bin/bash
# PersonalCRM Pi Setup Script
# One-time setup for Raspberry Pi deployment
#
# Usage: ./scripts/setup-pi.sh
#
# This script:
# - Creates the 'crm' service user
# - Creates the directory structure at /srv/personalcrm
# - Sets appropriate permissions
#
# After running this, you need to:
# 1. Create the .env file with production secrets
# 2. Run 'make deploy' to deploy the application

set -e

PI_HOST="${PI_HOST:-raspberet}"
PI_DIR="/srv/personalcrm"

echo "=== PersonalCRM Pi Setup ==="
echo "Target: $PI_HOST"
echo ""

# Verify Pi is reachable
echo "Checking connectivity to $PI_HOST..."
if ! ssh -q -o ConnectTimeout=5 "$PI_HOST" exit; then
    echo "Error: Cannot connect to $PI_HOST"
    echo "Ensure the Pi is on your Tailnet and SSH is configured."
    exit 1
fi
echo "OK"
echo ""

echo "Setting up $PI_HOST for PersonalCRM..."
echo ""

ssh "$PI_HOST" << 'REMOTE'
set -e

PI_DIR="/srv/personalcrm"

echo "Creating service user: crm"
if ! id crm &>/dev/null; then
    sudo useradd --system --shell /bin/false crm
    echo "Created crm user"
else
    echo "User crm already exists"
fi

# Add crm user to docker group
sudo usermod -aG docker crm 2>/dev/null || true
echo "Added crm to docker group"

echo ""
echo "Creating directory structure at $PI_DIR..."
sudo mkdir -p "$PI_DIR"/{backend/bin,backend/migrations,frontend/.next/static,frontend/public,infra,logs}
sudo chown -R crm:crm "$PI_DIR"
sudo chmod -R 775 "$PI_DIR"
echo "Directory structure created"

echo ""
echo "Adding $USER to crm group for deploy access..."
sudo usermod -aG crm "$USER"
echo "Added $USER to crm group (re-login required for group to take effect)"

echo ""
echo "Verifying Node.js installation..."
if command -v node &>/dev/null; then
    echo "Node.js: $(node --version)"
else
    echo "WARNING: Node.js not found!"
    echo "Install it with: sudo apt install -y nodejs"
fi

echo ""
echo "Verifying Docker installation..."
if command -v docker &>/dev/null; then
    echo "Docker: $(docker --version)"
else
    echo "WARNING: Docker not found!"
    echo "Install it with: curl -sSL https://get.docker.com | sh"
fi
REMOTE

echo ""
echo "=== Setup complete ==="
echo ""
echo "Next steps:"
echo ""
echo "1. Create production secrets on Pi:"
echo "   ssh $PI_HOST 'sudo nano $PI_DIR/.env'"
echo ""
echo "   Use .env.example.production as a reference."
echo "   Required variables:"
echo "   - DATABASE_URL"
echo "   - POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB"
echo "   - API_KEY"
echo "   - SESSION_SECRET"
echo ""
echo "2. Set permissions on secrets file:"
echo "   ssh $PI_HOST 'sudo chown crm:crm $PI_DIR/.env && sudo chmod 600 $PI_DIR/.env'"
echo ""
echo "3. Run first deploy:"
echo "   make deploy"
echo ""
