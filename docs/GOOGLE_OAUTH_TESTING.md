# Testing Google OAuth2 Locally

**Last Updated**: January 2026

This guide walks you through testing the Google OAuth2 integration locally without affecting production or using real credentials in your codebase.

---

## Table of Contents

1. [Overview](#overview)
2. [Create a Test Google Cloud Project](#create-a-test-google-cloud-project)
3. [Configure OAuth Credentials](#configure-oauth-credentials)
4. [Set Up Local Environment](#set-up-local-environment)
5. [Run the Application](#run-the-application)
6. [Test the OAuth Flow](#test-the-oauth-flow)
7. [Verify Token Storage](#verify-token-storage)
8. [Troubleshooting](#troubleshooting)

---

## Overview

The Google OAuth2 integration allows PersonalCRM to access:
- **Gmail** (read-only) - for email sync
- **Google Calendar** (read-only) - for calendar sync
- **Google Contacts** (read-only) - for contact sync

For local testing, you'll create a separate Google Cloud project so you can safely test without affecting production.

---

## Create a Test Google Cloud Project

### 1.1 Create Project

1. Go to [Google Cloud Console](https://console.cloud.google.com)
2. Click the project dropdown (top-left) → **New Project**
3. Name it something like `PersonalCRM-Dev`
4. Click **Create**
5. Select the new project from the dropdown

### 1.2 Enable Required APIs

1. Go to **APIs & Services** → **Library**
2. Search for and enable each of these:
   - **Gmail API**
   - **Google Calendar API**
   - **People API** (for Contacts)

### 1.3 Configure OAuth Consent Screen

1. Go to **APIs & Services** → **OAuth consent screen**
2. Select **External** (unless you have a Google Workspace organization)
3. Fill in the required fields:
   - **App name**: `PersonalCRM Dev`
   - **User support email**: Your email
   - **Developer contact email**: Your email
4. Click **Save and Continue**
5. On the **Scopes** page, click **Add or Remove Scopes** and add:
   - `openid` (**Required** - OpenID Connect authentication)
   - `.../auth/userinfo.email` (**Required** - to get user email address)
   - `.../auth/userinfo.profile` (**Required** - to get user profile info)
   - `https://www.googleapis.com/auth/gmail.readonly`
   - `https://www.googleapis.com/auth/calendar.readonly`
   - `https://www.googleapis.com/auth/contacts.readonly`
6. Click **Save and Continue**
7. On **Test users**, add your Gmail address
8. Click **Save and Continue**

> **Note**: While in "Testing" status, only the test users you add can authorize the app.

> **Google Workspace Users**: If you selected "Internal" for your OAuth app, all users in your Workspace organization can connect automatically. If you selected "External", you must add each account as a test user.

---

## Configure OAuth Credentials

### 2.1 Create OAuth Client ID

1. Go to **APIs & Services** → **Credentials**
2. Click **Create Credentials** → **OAuth client ID**
3. Select **Web application**
4. Name: `PersonalCRM Local`
5. Under **Authorized redirect URIs**, add:
   ```
   http://localhost:8080/api/v1/auth/google/callback
   ```
6. Click **Create**
7. **Copy** the Client ID and Client Secret (you'll need these)

### 2.2 Generate Encryption Key

The OAuth tokens are encrypted at rest using AES-256-GCM. Generate a key:

```bash
# Generate a 32-byte (256-bit) hex key
openssl rand -hex 32
```

Save this key - you'll add it to your `.env` file.

---

## Set Up Local Environment

### 3.1 Update Your .env File

Add these variables to your local `.env` file:

```bash
# Google OAuth2 Configuration
GOOGLE_CLIENT_ID=your-client-id-here.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=your-client-secret-here
GOOGLE_REDIRECT_URL=http://localhost:8080/api/v1/auth/google/callback

# Token Encryption Key (32-byte hex string)
TOKEN_ENCRYPTION_KEY=your-64-char-hex-key-here

# Enable External Sync Feature
ENABLE_EXTERNAL_SYNC=true
```

### 3.2 Verify Environment

```bash
# Check your .env has the required variables
grep -E "GOOGLE_|TOKEN_ENCRYPTION|ENABLE_EXTERNAL" .env
```

You should see all four variables set.

---

## Run the Application

### 4.1 Start the Database

```bash
make docker-up
```

### 4.2 Run Migrations (Optional)

**Note**: `make dev` (step 4.3) automatically runs migrations on backend startup, so you can skip this step if using `make dev`.

If you want to run migrations manually:

```bash
make db-migrate
```

### 4.3 Start Backend and Frontend

```bash
# Option 1: Run both together
make dev

# Option 2: Run separately (useful for debugging)
# Terminal 1:
make dev-native  # or make api-run

# Terminal 2:
cd frontend && bun dev
```

### 4.4 Verify Services

- Backend: http://localhost:8080/health (not /api/v1/health)
- Frontend: http://localhost:3000

---

## Test the OAuth Flow

### 5.1 Navigate to Settings

1. Open http://localhost:3000/settings
2. Scroll to the **Google Accounts** section
3. You should see the "Connect Google Account" button

### 5.2 Start OAuth Flow

1. Click **Connect Google Account**
2. You'll be redirected to Google's consent screen
3. Select your test Google account
4. Review the permissions and click **Allow**
5. You'll be redirected back to the Settings page

### 5.3 Verify Success

After successful authorization:
- You should see `?auth=success&provider=google` in the URL
- The connected account should appear in the list showing:
  - Email address
  - Account name
  - Granted scopes (Gmail, Calendar, Contacts)

### 5.4 Test Disconnect

1. Click the **Disconnect** button next to the account
2. Confirm the disconnection
3. The account should be removed from the list

---

## Verify Token Storage

### 6.1 Check Database

You can verify tokens are stored (encrypted) in the database:

```bash
# Connect to the database
make db-shell

# Check oauth_credential table
SELECT id, provider, account_id, account_name,
       LENGTH(access_token_encrypted) as token_len,
       scopes, created_at
FROM oauth_credential;
```

You should see:
- `provider`: `google`
- `account_id`: Your Gmail address
- `token_len`: Non-zero (encrypted token data)
- `scopes`: Array of granted scopes

### 6.2 Verify Encryption

The tokens should be encrypted. To verify:

```sql
-- This should show encrypted binary data, not readable tokens
SELECT access_token_encrypted FROM oauth_credential LIMIT 1;
```

If you see readable token text (starting with `ya29.`), encryption is not working.

---

## Troubleshooting

### OAuth Flow Errors

**"Error 400: redirect_uri_mismatch"**
- Verify your redirect URI in Google Cloud Console exactly matches:
  ```
  http://localhost:8080/api/v1/auth/google/callback
  ```
- Check for trailing slashes, http vs https, port numbers

**"Access blocked: This app's request is invalid"**
- You may not have added yourself as a test user
- Go to OAuth consent screen → Test users → Add your email

**"?auth=error&message=invalid_state"**
- The CSRF state expired (10 minute timeout)
- Try the flow again - don't leave the Google consent screen open too long

**"?auth=error&message=exchange_failed"**
- Check backend logs: `make logs-backend` or check terminal output
- Common causes:
  - Invalid client secret
  - Clock skew between your machine and Google
  - Network issues

### Configuration Errors

**"google OAuth credentials not configured"**
- Missing `GOOGLE_CLIENT_ID` or `GOOGLE_CLIENT_SECRET` in `.env`
- Restart the backend after adding them

**"encryption key is required"**
- Missing `TOKEN_ENCRYPTION_KEY` in `.env`
- Generate one with `openssl rand -hex 32`

**"invalid encryption key: must be 32 bytes"**
- The key must be exactly 64 hex characters (32 bytes)
- Don't include `0x` prefix, just the hex string

### Google Accounts Section Not Visible

If you don't see the Google Accounts section in Settings:
- Verify `ENABLE_EXTERNAL_SYNC=true` in your `.env`
- Check the feature flag is being read correctly in the frontend

---

## Running Automated Tests

The OAuth implementation includes comprehensive tests that don't require real Google credentials:

```bash
cd backend

# Run handler unit tests (mocked OAuth service)
go test ./internal/api/handlers/... -v -run ".*OAuth.*"

# Run crypto tests (encryption/decryption)
go test ./internal/crypto/... -v

# Run all unit tests
go test ./... -short
```

For repository integration tests (requires database):

```bash
# Ensure database is running
make docker-up

# Run integration tests
DATABASE_URL="postgres://..." go test ./tests/... -v -run ".*OAuth.*"
```

---

## Next Steps

Once you've verified the OAuth flow works locally:
- See [GOOGLE_OAUTH_PRODUCTION.md](./GOOGLE_OAUTH_PRODUCTION.md) for deploying to your Pi
- The sync functionality will use these tokens to fetch data from Google APIs
