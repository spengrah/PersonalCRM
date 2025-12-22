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
- Bun 1.0+ (for frontend)
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

## Raspberry Pi Deployment with Systemd

Deploy PersonalCRM as a systemd service on Raspberry Pi for automatic startup on boot.

### Prerequisites

1. **Raspberry Pi OS** (Bullseye or newer)
2. **Docker** and **Docker Compose**:
   ```bash
   curl -sSL https://get.docker.com | sh
   sudo usermod -aG docker $USER
   ```
3. **Go 1.22+**:
   ```bash
   wget https://go.dev/dl/go1.23.0.linux-arm64.tar.gz
   sudo tar -C /usr/local -xzf go1.23.0.linux-arm64.tar.gz
   export PATH=$PATH:/usr/local/go/bin
   ```
4. **Bun**:
   ```bash
   curl -fsSL https://bun.sh/install | bash
   sudo ln -s ~/.bun/bin/bun /usr/local/bin/bun
   ```

### Quick Installation

1. **Clone and configure**:
   ```bash
   git clone https://github.com/spengrah/PersonalCRM.git
   cd PersonalCRM
   cp env.example .env
   nano .env  # Edit with your settings
   ```

   **Required settings**:
   - `DATABASE_URL`: PostgreSQL connection string
   - `SESSION_SECRET`: Generate with `openssl rand -base64 32`
   - `POSTGRES_PASSWORD`: Set a secure password

2. **Run installation script**:
   ```bash
   sudo chmod +x infra/install-systemd.sh
   sudo infra/install-systemd.sh
   ```

3. **Start services**:
   ```bash
   sudo systemctl start personalcrm.target
   ```

4. **Verify**:
   ```bash
   sudo systemctl status personalcrm.target
   ```

### Service Management

**Start/stop services**:
```bash
sudo systemctl start personalcrm.target   # Start all
sudo systemctl stop personalcrm.target    # Stop all
sudo systemctl restart personalcrm-backend.service  # Restart specific service
```

**Check status**:
```bash
sudo systemctl status personalcrm.target
sudo systemctl status personalcrm-backend.service
```

**View logs**:
```bash
sudo journalctl -u personalcrm-backend -f  # Real-time backend logs
sudo journalctl -u personalcrm-frontend -f # Real-time frontend logs
sudo journalctl -u personalcrm-* -f        # All services
```

**Enable/disable auto-start**:
```bash
sudo systemctl enable personalcrm.target   # Enable
sudo systemctl disable personalcrm.target  # Disable
```

### Network Access

By default, the backend binds to `127.0.0.1` (localhost only). To access from other devices:

**Option 1: Reverse Proxy (Recommended)**

Install nginx:
```bash
sudo apt install nginx
```

Create `/etc/nginx/sites-available/personalcrm`:
```nginx
server {
    listen 80;
    server_name your-raspberry-pi.local;

    location / {
        proxy_pass http://localhost:3001;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_cache_bypass $http_upgrade;
    }

    location /api/ {
        proxy_pass http://localhost:8080/api/;
    }

    location /health {
        proxy_pass http://localhost:8080/health;
    }
}
```

Enable:
```bash
sudo ln -s /etc/nginx/sites-available/personalcrm /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl restart nginx
```

### Accessing Your CRM

After installation:
- **Frontend**: `http://your-raspberry-pi.local:3001`
- **Backend API**: `http://your-raspberry-pi.local:8080` (if network access configured)

Find your Pi's IP: `hostname -I`

### Troubleshooting

**Services won't start**:
```bash
sudo systemctl status personalcrm-backend
sudo journalctl -u personalcrm-backend -n 50
```

**Database connection errors**:
```bash
sudo systemctl restart personalcrm-database
docker logs crm-postgres
docker ps | grep crm-postgres
```

**Permission errors**:
```bash
sudo chown -R crm:crm /opt/personalcrm
sudo chmod -R 750 /opt/personalcrm/logs
```

**Port conflicts**:
```bash
sudo lsof -i :8080  # Check port 8080
sudo lsof -i :3001  # Check port 3001
```

### Updating

```bash
cd ~/PersonalCRM
git pull origin main
sudo ./infra/install-systemd.sh
sudo systemctl restart personalcrm.target
```

### Backup

**Backup database**:
```bash
docker exec crm-postgres pg_dump -U crm_user personal_crm > backup.sql
```

**Restore database**:
```bash
cat backup.sql | docker exec -i crm-postgres psql -U crm_user personal_crm
```

### Security

1. **Firewall**:
   ```bash
   sudo ufw enable
   sudo ufw allow 22/tcp    # SSH
   sudo ufw allow 3001/tcp  # Frontend
   ```

2. **HTTPS** with Let's Encrypt:
   ```bash
   sudo apt install certbot python3-certbot-nginx
   sudo certbot --nginx -d your-domain.com
   ```

3. **Secrets**:
   ```bash
   sudo chmod 600 /opt/personalcrm/.env
   ```

### Optional: Tailscale Integration

Access your PersonalCRM from anywhere in your tailnet without port forwarding or dynamic DNS.

**Prerequisites**: None (Tailscale is optional)

1. **Install Tailscale on Raspberry Pi**:
   ```bash
   curl -fsSL https://tailscale.com/install.sh | sh
   sudo tailscale up
   ```

2. **Enable MagicDNS** (in Tailscale admin console):
   - Go to [DNS settings](https://login.tailscale.com/admin/dns)
   - Enable MagicDNS
   - Your Pi will be accessible at `your-pi-name` from any device in your tailnet

3. **Configure nginx for Tailscale hostname** (if using reverse proxy):

   Edit `/etc/nginx/sites-available/personalcrm` and replace `your-raspberry-pi.local` with your Tailscale MagicDNS hostname:
   ```nginx
   server {
       listen 80;
       server_name your-pi-name;  # Your Tailscale MagicDNS hostname
       # ... rest of config
   }
   ```

4. **Access from any device in your tailnet**:
   - Frontend: `http://your-pi-name:3001`
   - Or via nginx: `http://your-pi-name`

**Benefits**:
- No port forwarding required
- Secure WireGuard encryption
- Access from MacBook, iPhone, or any device in your tailnet
- Persistent hostname via MagicDNS
- Works seamlessly with existing nginx reverse proxy

### Optional: Automated Updates with CI/CD

Deploy updates automatically via GitHub Actions when you push to main.

**Prerequisites**: Tailscale installed on your Pi (see above)

1. **Enable Tailscale SSH** on your Pi:
   ```bash
   sudo tailscale up --ssh
   ```

2. **Create dedicated deploy user** on your Pi (security best practice):
   ```bash
   # Create deploy-only user
   sudo useradd -m -s /bin/bash deploy

   # Grant minimal sudo permissions for deployment
   sudo tee /etc/sudoers.d/deploy << 'EOF'
   # Allow deploy user to restart services and run install script
   deploy ALL=(root) NOPASSWD: /usr/bin/systemctl restart personalcrm.target
   deploy ALL=(root) NOPASSWD: /usr/bin/systemctl start personalcrm.target
   deploy ALL=(root) NOPASSWD: /usr/bin/systemctl stop personalcrm.target
   deploy ALL=(root) NOPASSWD: /usr/bin/systemctl status personalcrm.target
   deploy ALL=(root) NOPASSWD: /opt/personalcrm/infra/install-systemd.sh
   EOF

   sudo chmod 440 /etc/sudoers.d/deploy

   # Allow deploy user to access the project directory
   sudo usermod -aG crm deploy
   ```

3. **Generate deployment key** for the deploy user:
   ```bash
   # Switch to deploy user
   sudo -u deploy bash

   # Generate SSH key
   ssh-keygen -t ed25519 -f ~/.ssh/deploy_key -N ""
   cat ~/.ssh/deploy_key.pub >> ~/.ssh/authorized_keys
   chmod 600 ~/.ssh/authorized_keys
   cat ~/.ssh/deploy_key  # Copy this for GitHub secrets

   # Exit back to your user
   exit
   ```

4. **Create Tailscale OAuth client**:
   - Go to [Tailscale admin console → Settings → OAuth clients](https://login.tailscale.com/admin/settings/oauth)
   - Generate a new OAuth client
   - Add tag `tag:ci` to the client
   - Copy the client ID and secret

5. **Add GitHub secrets** (Settings → Secrets and variables → Actions):
   - `TS_OAUTH_CLIENT_ID`: OAuth client ID from Tailscale
   - `TS_OAUTH_SECRET`: OAuth secret from Tailscale
   - `PI_HOSTNAME`: Your Pi's Tailscale hostname (e.g., `your-pi-name`)
   - `PI_DEPLOY_KEY`: Private key content from deploy user's `~/.ssh/deploy_key`

6. **Create workflow** `.github/workflows/deploy.yml`:
   ```yaml
   name: Deploy to Raspberry Pi

   on:
     push:
       branches: [main]

   jobs:
     deploy:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v4

         - name: Connect to Tailscale
           uses: tailscale/github-action@v2
           with:
             oauth-client-id: ${{ secrets.TS_OAUTH_CLIENT_ID }}
             oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
             tags: tag:ci

         - name: Setup SSH
           run: |
             mkdir -p ~/.ssh
             echo "${{ secrets.PI_DEPLOY_KEY }}" > ~/.ssh/deploy_key
             chmod 600 ~/.ssh/deploy_key

         - name: Deploy to Pi
           env:
             PI_HOSTNAME: ${{ secrets.PI_HOSTNAME }}
           run: |
             ssh -i ~/.ssh/deploy_key -o StrictHostKeyChecking=no deploy@$PI_HOSTNAME << 'EOF'
               cd ~/PersonalCRM
               git pull origin main
               sudo /opt/personalcrm/infra/install-systemd.sh
               sudo systemctl restart personalcrm.target
             EOF
   ```

7. **Test the deployment**:
   ```bash
   git commit -m "test: trigger deployment"
   git push origin main
   # Watch GitHub Actions tab for deployment status
   ```

**Security Considerations**:

- **Dedicated deploy user**: The `deploy` user has minimal sudo permissions - only what's needed for deployment
- **Principle of least privilege**: If the SSH key is compromised, attacker can only restart services and run the install script, not gain full system access
- **Key rotation**: Rotate the deploy SSH key periodically (quarterly recommended)
- **GitHub Environments** (optional): Add environment protection rules requiring manual approval for production deployments
- **Monitor logs**: Regularly check `/var/log/auth.log` for unauthorized access attempts

**Benefits**:
- Automatic deployments on push to main
- Secure deployment via Tailscale (no public SSH exposure)
- Zero-config remote access to Pi
- Deploy from anywhere in your tailnet
- No need for static IPs or port forwarding

**Note**: These sections are optional enhancements. PersonalCRM works perfectly fine without Tailscale or CI/CD - they simply provide additional convenience for remote access and automated deployments.

## Project Structure

```
personal-crm/
├── frontend/           # Next.js React application
├── backend/            # Go API server
├── desktop-ui/         # Vite + React desktop UI
├── desktop/            # Tauri app (Rust)
├── infra/              # Docker Compose & infrastructure
├── tests/              # E2E and integration tests
└── docs/               # Documentation
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

See [TEST_GUIDE.md](docs/TEST_GUIDE.md) for detailed testing information.

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

See `docs/PLAN.md` for detailed architecture and implementation roadmap.

## Contributing

This is a personal project, but issues and suggestions are welcome.

## License

MIT
