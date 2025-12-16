# 🧪 Human Smoke Test Guide

## Prerequisites
- ✅ Docker Desktop running (check the whale icon in your menu bar)
- ✅ Terminal open in project directory

## Step-by-Step Test

### 🐳 Step 1: Start Database
```bash
make docker-up
```
**✅ Expected:** Should see "PostgreSQL started successfully"

### 🔧 Step 2: Start Backend API
```bash
make api-build && make api-run
```
**✅ Expected:** Should see "Server starting on :8080"

### 🎨 Step 3: Start Frontend (in new terminal)
```bash
cd frontend && npm run dev -- --port 3000
```
**✅ Expected:** Should see "Ready in XXXms" and "Local: http://localhost:3000"

## 🧪 Manual Tests

### Test 1: Health Check
**URL:** http://localhost:8080/health
**✅ Expected:** `{"status":"ok"}`

### Test 2: API Documentation
**URL:** http://localhost:8080/swagger/index.html
**✅ Expected:** Interactive API documentation page

### Test 3: Frontend Dashboard
**URL:** http://localhost:3000
**✅ Expected:** CRM dashboard with navigation

### Test 4: Create a Contact
1. Go to: http://localhost:3000/contacts/new
2. Fill out the form:
   - Full Name: "John Doe"
   - Email: "john@example.com"
   - Phone: "+1-555-0123"
   - Cadence: "monthly"
3. Click "Create Contact"
**✅ Expected:** Contact created successfully, redirected to contact list

### Test 5: View Contacts
**URL:** http://localhost:3000/contacts
**✅ Expected:** See "John Doe" in the contacts table

### Test 6: View Contact Details
1. Click on "John Doe" in the contacts table
**✅ Expected:** Contact detail page with all information

### Test 7: Dashboard Reminders
**URL:** http://localhost:3000/dashboard
**✅ Expected:** Dashboard shows reminder statistics

### Test 8: Reminders Page
**URL:** http://localhost:3000/reminders
**✅ Expected:** Reminders table (may be empty initially)

### Test 9: API Contact Creation
```bash
curl -X POST http://localhost:8080/api/v1/contacts \
  -H "Content-Type: application/json" \
  -d '{
    "full_name": "Jane Smith",
    "email": "jane@example.com",
    "phone": "+1-555-0124",
    "cadence": "weekly"
  }'
```
**✅ Expected:** JSON response with created contact data

### Test 10: API Contact List
```bash
curl http://localhost:8080/api/v1/contacts
```
**✅ Expected:** JSON array with both John Doe and Jane Smith

## 🚨 Troubleshooting

### If Database Connection Fails:
```bash
make docker-reset
make docker-up
```

### If Backend Won't Start:
```bash
make api-build
# Check for compilation errors
```

### If Frontend Won't Start:
```bash
cd frontend
npm install
npm run dev -- --port 3000
```

### If Port Already in Use:
```bash
# Stop conflicting processes
pkill -f "crm-api"
pkill -f "next dev"
```

## 🎯 Success Criteria

**✅ All tests pass if:**
- All URLs load without errors
- Contact creation works via both frontend and API
- Data persists between page refreshes
- API returns proper JSON responses
- No console errors in browser developer tools

## 🆘 If Something Fails

Run the log sharing script to help debug:
```bash
./share-logs.sh
```

Then share the output for assistance!
