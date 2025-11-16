# Tako Secrets Test API

This is a test project to verify that Tako's new secrets management system works correctly in a real deployment.

## What This Tests

- ✅ Secrets loaded from `.tako/secrets.production`
- ✅ Secrets passed to container via env file (not command line)
- ✅ Secret aliasing (STRIPE_KEY:STRIPE_SECRET_KEY)
- ✅ Secrets properly redacted in logs
- ✅ Secrets accessible in the running container
- ✅ Automatic cleanup of temporary env files

## Setup

1. **Initialize secrets**:
   ```bash
   tako secrets init
   ```

2. **Add secrets** (already done if you're reading this):
   ```bash
   tako secrets set DATABASE_URL=postgresql://... --env production
   tako secrets set JWT_SECRET=your_secret --env production
   tako secrets set API_KEY=sk_test_123 --env production
   tako secrets set STRIPE_SECRET_KEY=sk_live_456 --env production
   ```

3. **Set your server IP** in `.env`:
   ```bash
   SERVER_IP=your.server.ip
   ```

4. **Validate secrets**:
   ```bash
   tako secrets validate --env production
   ```

5. **Deploy**:
   ```bash
   tako deploy --env production
   ```

## Verification Endpoints

Once deployed, test these endpoints:

### 1. Health Check
```bash
curl https://secrets-test.YOUR_IP.sslip.io/health
```

Should return:
```json
{
  "status": "healthy",
  "timestamp": "2025-11-14T..."
}
```

### 2. Verify Secrets Loaded
```bash
curl https://secrets-test.YOUR_IP.sslip.io/api/verify-secrets
```

Should return:
```json
{
  "status": "success",
  "message": "All secrets loaded successfully!",
  "secrets": {
    "DATABASE_URL": true,
    "JWT_SECRET": true,
    "API_KEY": true,
    "STRIPE_KEY": true
  },
  "count": {
    "total": 4,
    "loaded": 4
  }
}
```

### 3. Main Info
```bash
curl https://secrets-test.YOUR_IP.sslip.io/
```

Should show secrets as "✅ Loaded (hidden)" without exposing actual values.

### 4. Database URL Info
```bash
curl https://secrets-test.YOUR_IP.sslip.io/api/database-info
```

Should parse and display database connection info without exposing credentials.

## Expected Behavior

### ✅ What Should Happen

1. **Secrets never in logs**: When deploying, you should see `[REDACTED]` or `po***ql` instead of actual values
2. **Secrets never in `ps aux`**: The secrets are in an env file, not command line args
3. **Secrets accessible in container**: The API endpoints should confirm all secrets are loaded
4. **Env file cleanup**: The temporary env file `/tmp/tako-*.env` should be deleted after container starts
5. **Aliasing works**: STRIPE_KEY should be available in the container (reading from STRIPE_SECRET_KEY)

### ❌ What Should NOT Happen

1. **Secrets exposed in deployment logs**
2. **Secrets visible in `docker inspect`** (well, they are in env, but not in command)
3. **Secrets committed to git** (`.tako/secrets*` should be gitignored)
4. **Temp env files left on server**

## Test Checklist

- [ ] Deploy succeeds without errors
- [ ] `/health` endpoint returns 200
- [ ] `/api/verify-secrets` shows all 4 secrets loaded
- [ ] Deployment logs show `[REDACTED]` instead of actual secrets
- [ ] `ps aux | grep docker` doesn't show secrets in command line
- [ ] Temp env file is deleted after deployment
- [ ] STRIPE_KEY alias works (reads from STRIPE_SECRET_KEY)

## Files Created

```
test-secrets/
├── index.js              # Express API
├── package.json          # Dependencies
├── Dockerfile            # Container image
├── tako.yaml             # Tako configuration
├── .env.example          # Example environment variables
├── .tako/
│   ├── .gitignore        # Auto-generated (ignores secrets*)
│   ├── secrets           # Common secrets
│   ├── secrets.staging   # Staging secrets
│   └── secrets.production # Production secrets (GITIGNORED!)
└── README.md             # This file
```

## Troubleshooting

### Secrets not loading?
```bash
# Check secrets are set
tako secrets list --env production

# Validate configuration
tako secrets validate --env production

# Check the secrets file
cat .tako/secrets.production
```

### Deployment fails?
```bash
# Deploy with verbose output
tako deploy --env production --verbose

# Check if secrets are being redacted in logs
# You should see [REDACTED] or partial values like po***ql
```

### API returns "NOT_SET"?
- Verify secrets were uploaded to the server
- Check container logs: `docker logs tako-secrets-test_production_api_1`
- Ensure the env file was created and loaded

## Security Notes

- ⚠️ This test API includes debug endpoints that confirm secrets are loaded
- ⚠️ In production, NEVER expose actual secret values via API
- ⚠️ The `.tako/secrets*` files are gitignored automatically
- ⚠️ Secrets are transmitted via SSH (encrypted)
- ⚠️ Temporary env files have 0600 permissions and are deleted after use
