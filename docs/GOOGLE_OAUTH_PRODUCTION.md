# Google OAuth2 Production Setup (Raspberry Pi)

**Last Updated**: January 2026

This guide walks you through setting up Google OAuth2 for your production PersonalCRM deployment on a Raspberry Pi.

---

## Table of Contents

1. [Overview](#overview)
2. [Prerequisites](#prerequisites)
3. [Create Production Google Cloud Project](#create-production-google-cloud-project)
4. [Configure OAuth for Production](#configure-oauth-for-production)
5. [Configure Pi Environment](#configure-pi-environment)
6. [Deploy and Test](#deploy-and-test)
7. [Security Considerations](#security-considerations)
8. [Troubleshooting](#troubleshooting)

---

## Overview

Setting up OAuth in production differs from local testing:
- You'll use your Pi's hostname or Tailscale domain for the redirect URI
- Secrets are stored only on the Pi (never in git)
- You may want a separate Google Cloud project for production

---

## Prerequisites

Before starting, ensure you have:

- [ ] PersonalCRM deployed to your Pi (see [FIRST_TIME_PI_DEPLOYMENT.md](./FIRST_TIME_PI_DEPLOYMENT.md))
- [ ] Pi accessible via Tailscale (recommended) or your network
- [ ] SSH access to the Pi
- [ ] A Google account for OAuth configuration

---

## Create Production Google Cloud Project

You can either:
- **Option A**: Use your existing test project (simpler, fine for personal use)
- **Option B**: Create a separate production project (better isolation)

### Option A: Use Existing Project

If using your test project, skip to [Configure OAuth for Production](#configure-oauth-for-production).

### Option B: Create New Project

1. Go to [Google Cloud Console](https://console.cloud.google.com)
2. Create a new project named `PersonalCRM-Prod`
3. Enable the same APIs:
   - Gmail API
   - Google Calendar API
   - People API
4. Configure OAuth consent screen (same as testing guide)

---

## Configure OAuth for Production

### 3.1 Determine Your Redirect URI

Your redirect URI depends on how you access your Pi:

**Option 1: Tailscale HTTPS (Recommended)**
```
https://<pi-hostname>.<tailnet-name>.ts.net/api/v1/auth/google/callback
```
Example: `https://raspberry-pi.tail1234.ts.net/api/v1/auth/google/callback`

**Option 2: Tailscale HTTP (Direct)**
```
http://<pi-tailscale-ip>:8080/api/v1/auth/google/callback
```
Example: `http://100.64.1.23:8080/api/v1/auth/google/callback`

**Option 3: Local Network**
```
http://<pi-hostname>.local:8080/api/v1/auth/google/callback
```
Example: `http://raspberrypi.local:8080/api/v1/auth/google/callback`

> **Recommendation**: Use Tailscale HTTPS for the best security and reliability.

### 3.2 Add Production Redirect URI

1. Go to **APIs & Services** → **Credentials**
2. Click on your OAuth client (or create a new one for production)
3. Under **Authorized redirect URIs**, add your production URI
4. Click **Save**

> **Tip**: You can have multiple redirect URIs. Keep your localhost one for testing.

### 3.3 Copy Credentials

Note down:
- **Client ID**: `xxx.apps.googleusercontent.com`
- **Client Secret**: `GOCSPX-xxx`

### 3.4 Generate Production Encryption Key

Generate a new key for production (don't reuse your dev key):

```bash
openssl rand -hex 32
```

Save this securely - you'll need it for the Pi configuration.

---

## Configure Pi Environment

### 4.1 SSH to Your Pi

```bash
ssh <pi-hostname>
```

### 4.2 Edit Production Environment

```bash
sudo nano /srv/personalcrm/.env
```

Add these variables:

```bash
# Google OAuth2 Configuration
GOOGLE_CLIENT_ID=your-production-client-id.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=your-production-client-secret
GOOGLE_REDIRECT_URL=https://<pi-hostname>.<tailnet>.ts.net/api/v1/auth/google/callback

# Token Encryption Key (32-byte hex, different from dev!)
TOKEN_ENCRYPTION_KEY=your-production-64-char-hex-key

# Enable External Sync
ENABLE_EXTERNAL_SYNC=true
```

Save and exit (Ctrl+X, Y, Enter).

### 4.3 Verify Permissions

```bash
# Ensure proper ownership and permissions
sudo chown crm:crm /srv/personalcrm/.env
sudo chmod 600 /srv/personalcrm/.env

# Verify
ls -la /srv/personalcrm/.env
# Should show: -rw------- crm crm
```

### 4.4 Restart Services

```bash
sudo systemctl restart personalcrm-backend
```

### 4.5 Verify Backend Started

```bash
# Check status
sudo systemctl status personalcrm-backend

# Check logs for any OAuth config errors
sudo journalctl -u personalcrm-backend -n 50 | grep -i oauth
```

---

## Deploy and Test

### 5.1 Run Database Migration

If this is a fresh deployment with the OAuth feature:

```bash
# From your Mac, deploy the latest code
make deploy

# Or manually run migrations on Pi
ssh <pi-hostname>
cd /srv/personalcrm/backend
./bin/crm-api migrate  # if supported, or migrations run on startup
```

The migration creates the `oauth_credential` table automatically on backend startup.

### 5.2 Access Settings Page

Open your browser and navigate to:
```
https://<pi-hostname>.<tailnet>.ts.net/settings
```

Or via Tailscale IP:
```
http://<pi-tailscale-ip>:3001/settings
```

### 5.3 Connect Google Account

1. Scroll to **Google Accounts** section
2. Click **Connect Google Account**
3. Complete the OAuth flow with Google
4. Verify you're redirected back with `?auth=success`

### 5.4 Verify Connection

After connecting, you should see:
- Your Google email address
- Account name
- List of granted scopes
- "Disconnect" button

---

## Security Considerations

### 6.1 Token Encryption

OAuth tokens are encrypted using AES-256-GCM before storage:
- **Access tokens**: Short-lived (~1 hour), encrypted
- **Refresh tokens**: Long-lived, encrypted
- **Encryption key**: Stored only in Pi's `.env` file

### 6.2 Secrets Management

| Secret | Location | Notes |
|--------|----------|-------|
| Client ID | Pi `.env` only | Never commit to git |
| Client Secret | Pi `.env` only | Never commit to git |
| Encryption Key | Pi `.env` only | Generate unique per environment |

### 6.3 Scope Limitations

The app requests read-only scopes only:
- `gmail.readonly` - Read emails, no send/delete
- `calendar.readonly` - Read events, no create/modify
- `contacts.readonly` - Read contacts, no modify

### 6.4 Token Revocation

Users can disconnect accounts anytime:
- Frontend: Click "Disconnect" in Settings
- This revokes the token with Google AND deletes from database

To manually revoke all Google access:
1. Go to [Google Account Security](https://myaccount.google.com/security)
2. Find "Third-party apps with account access"
3. Remove PersonalCRM

### 6.5 Network Security

With Tailscale HTTPS:
- All traffic is encrypted end-to-end
- No ports exposed to public internet
- Certificate managed by Tailscale

---

## Troubleshooting

### OAuth Flow Errors

**"redirect_uri_mismatch"**

The redirect URI doesn't match what's configured in Google Cloud Console.

```bash
# Check what's configured on Pi
ssh <pi-hostname> 'grep GOOGLE_REDIRECT_URL /srv/personalcrm/.env'
```

Ensure it exactly matches one of the URIs in your OAuth client settings.

**"invalid_state" after OAuth**

The CSRF state expired or was lost:
- States expire after 10 minutes
- If you have multiple backend instances, state is per-instance (in-memory)
- Try the flow again

**Google shows "This app isn't verified"**

For personal use, this is fine - click "Advanced" → "Go to PersonalCRM (unsafe)".

For a smoother experience:
1. Go to OAuth consent screen in Google Cloud Console
2. Click "Publish App" (if you want to skip the warning)
3. Or stay in Testing mode and add your accounts as test users

### Connection Errors

**"Failed to exchange code"**

Check backend logs:
```bash
sudo journalctl -u personalcrm-backend -n 100 | grep -i "oauth\|exchange\|google"
```

Common causes:
- Network connectivity from Pi to Google APIs
- Clock skew (NTP not synced)
- Invalid client secret

**Test network connectivity:**
```bash
# From Pi
curl -I https://oauth2.googleapis.com/token
```

**Fix clock skew:**
```bash
sudo timedatectl set-ntp true
timedatectl status
```

### Configuration Errors

**"google OAuth credentials not configured"**

Backend can't read the credentials:
```bash
# Verify .env has the variables
ssh <pi-hostname> 'grep GOOGLE /srv/personalcrm/.env'

# Check file permissions
ssh <pi-hostname> 'ls -la /srv/personalcrm/.env'

# Restart backend
sudo systemctl restart personalcrm-backend
```

**"create token encryptor: encryption key is required"**

Missing or invalid encryption key:
```bash
# Check the key exists and is 64 characters
ssh <pi-hostname> 'grep TOKEN_ENCRYPTION_KEY /srv/personalcrm/.env | wc -c'
# Should output: 83 (64 chars + "TOKEN_ENCRYPTION_KEY=" + newline)
```

### Database Errors

**Check migration ran:**
```bash
ssh <pi-hostname>

# Connect to database
docker exec -it personalcrm-postgres psql -U crm -d personalcrm

# Check table exists
\dt oauth_credential

# Check for data
SELECT id, provider, account_id FROM oauth_credential;
```

---

## Maintenance

### Rotating the Encryption Key

If you need to rotate the encryption key:

1. **Export existing tokens** (they'll need re-encryption)
2. **Generate new key**: `openssl rand -hex 32`
3. **Update `.env`** with new key
4. **Users will need to re-authenticate** (old tokens won't decrypt)

### Viewing Connected Accounts

```bash
# From Pi, check database
docker exec -it personalcrm-postgres psql -U crm -d personalcrm \
  -c "SELECT id, account_id, account_name, created_at FROM oauth_credential;"
```

### Manually Revoking an Account

```bash
# Get account ID from database
docker exec -it personalcrm-postgres psql -U crm -d personalcrm \
  -c "SELECT id, account_id FROM oauth_credential;"

# Delete by ID (tokens are already encrypted, safe to delete row)
docker exec -it personalcrm-postgres psql -U crm -d personalcrm \
  -c "DELETE FROM oauth_credential WHERE id = 'uuid-here';"
```

---

## Related Documentation

- [GOOGLE_OAUTH_TESTING.md](./GOOGLE_OAUTH_TESTING.md) - Local testing guide
- [DEPLOYMENT.md](./DEPLOYMENT.md) - General deployment information
- [FIRST_TIME_PI_DEPLOYMENT.md](./FIRST_TIME_PI_DEPLOYMENT.md) - Initial Pi setup
