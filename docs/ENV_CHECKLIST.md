# 🔧 Environment Variable Prevention Checklist

## ✅ **Before Any Development Session:**

### **1. Verify .env File Exists**
```bash
ls -la .env
```
**Expected**: Should show `.env` file in project root

### **2. Test Environment Loading**
```bash
set -a && source ./.env && set +a && echo "DATABASE_URL: $DATABASE_URL"
```
**Expected**: Should show complete database URL

### **3. Test Make Commands**
```bash
# This should work without manual env loading
make api-run
```
**Expected**: API starts without "environment variable required" errors

### **4. Verify All Makefile Targets Load Env**
Check these commands work properly:
- `make dev` ✅
- `make api-run` ✅  
- `make test` (doesn't need env) ✅

## 🚨 **Warning Signs of Env Issues:**

1. **Error**: `DATABASE_URL environment variable is required`
   - **Fix**: Update Makefile target to source `.env`

2. **Error**: `Failed to connect to database`
   - **Check**: Database is running (`make docker-up`)
   - **Check**: Environment variables are loaded

3. **Error**: `Cannot connect to the Docker daemon`
   - **Fix**: Start Docker Desktop

## 🛠️ **Standard Environment Loading Pattern:**

For any new Makefile targets that need database access:
```makefile
target-name:
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && your-command-here
```

## 🧪 **Quick Environment Test:**
```bash
# Run this anytime you're unsure about env setup
./smoke-test.sh
```

## 📝 **Development Workflow:**

1. **Start Development**: `make dev` (should work without manual env loading)
2. **API Only**: `make api-run` (should work without manual env loading)  
3. **Full Test**: `./smoke-test.sh` (comprehensive system check)

**Never manually load environment variables** - all Makefile commands should handle this automatically.
