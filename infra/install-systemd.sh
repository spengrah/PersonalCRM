#!/bin/bash
# PersonalCRM Systemd Installation Script for Raspberry Pi
# Run with: sudo ./install-systemd.sh

set -e

INSTALL_DIR="/opt/personalcrm"
SERVICE_USER="crm"
SERVICE_GROUP="crm"
SYSTEMD_DIR="/etc/systemd/system"
PROJECT_DIR="$(pwd)"

echo "=== PersonalCRM Systemd Installation ==="
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo "Error: This script must be run as root (use sudo)"
   exit 1
fi

# Check prerequisites
echo "Checking prerequisites..."
command -v docker >/dev/null 2>&1 || { echo "Error: Docker is not installed"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "Error: Go is not installed"; exit 1; }
command -v bun >/dev/null 2>&1 || { echo "Error: Bun is not installed"; exit 1; }
echo "✓ Prerequisites satisfied"
echo ""

# Create service user
echo "Creating service user: $SERVICE_USER"
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --user-group --no-create-home --shell /bin/false "$SERVICE_USER"
    echo "✓ User created"
else
    echo "✓ User already exists"
fi
echo ""

# Add user to docker group
echo "Adding $SERVICE_USER to docker group..."
usermod -aG docker "$SERVICE_USER"
echo "✓ User added to docker group"
echo ""

# Create installation directory
echo "Creating installation directory: $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"/{backend/bin,backend/migrations,frontend,infra,logs}
echo "✓ Directory structure created"
echo ""

# Build backend
echo "Building backend..."
cd "$PROJECT_DIR/backend"
go build -o "$INSTALL_DIR/backend/bin/crm-api" ./cmd/crm-api/main.go
echo "✓ Backend built"
echo ""

# Build frontend
echo "Building frontend..."
cd "$PROJECT_DIR/frontend"
bun run build
echo "✓ Frontend built"
echo ""

# Copy files
echo "Copying application files..."
cp -r "$PROJECT_DIR/backend/migrations"/* "$INSTALL_DIR/backend/migrations/"
cp -r "$PROJECT_DIR/frontend"/* "$INSTALL_DIR/frontend/"
cp -r "$PROJECT_DIR/infra"/* "$INSTALL_DIR/infra/"
echo "✓ Files copied"
echo ""

# Copy environment file
echo "Setting up environment file..."
if [ -f "$PROJECT_DIR/.env" ]; then
    cp "$PROJECT_DIR/.env" "$INSTALL_DIR/.env"
    echo "✓ Environment file copied"
else
    echo "Warning: .env file not found"
    echo "Creating from .env.example..."
    cp "$PROJECT_DIR/.env.example" "$INSTALL_DIR/.env"
    echo "⚠  Please edit /opt/personalcrm/.env with your configuration"
fi
echo ""

# Set permissions
echo "Setting permissions..."
chown -R "$SERVICE_USER:$SERVICE_GROUP" "$INSTALL_DIR"
chmod 750 "$INSTALL_DIR"
chmod 600 "$INSTALL_DIR/.env"
chmod 755 "$INSTALL_DIR/backend/bin/crm-api"
chmod -R 750 "$INSTALL_DIR/logs"
echo "✓ Permissions set"
echo ""

# Install systemd service files
echo "Installing systemd service files..."
cp "$PROJECT_DIR/infra/personalcrm-database.service" "$SYSTEMD_DIR/"
cp "$PROJECT_DIR/infra/personalcrm-backend.service" "$SYSTEMD_DIR/"
cp "$PROJECT_DIR/infra/personalcrm-frontend.service" "$SYSTEMD_DIR/"
cp "$PROJECT_DIR/infra/personalcrm.target" "$SYSTEMD_DIR/"
echo "✓ Service files installed"
echo ""

# Reload systemd
echo "Reloading systemd daemon..."
systemctl daemon-reload
echo "✓ Systemd reloaded"
echo ""

# Enable services
echo "Enabling services for auto-start on boot..."
systemctl enable personalcrm-database.service
systemctl enable personalcrm-backend.service
systemctl enable personalcrm-frontend.service
systemctl enable personalcrm.target
echo "✓ Services enabled"
echo ""

echo "=== Installation Complete ==="
echo ""
echo "Next steps:"
echo "1. Edit configuration: sudo nano $INSTALL_DIR/.env"
echo "2. Start services: sudo systemctl start personalcrm.target"
echo "3. Check status: sudo systemctl status personalcrm.target"
echo "4. View logs: sudo journalctl -u personalcrm-backend -f"
echo ""
echo "Services will automatically start on boot."
echo ""
echo "Network Access:"
echo "  - Frontend: http://$(hostname -I | awk '{print $1}'):3001"
echo "  - Backend API: http://127.0.0.1:8080 (localhost only)"
echo "  - To allow network access to backend, see README.md"
echo ""
