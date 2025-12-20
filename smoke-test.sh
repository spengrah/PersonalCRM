#!/bin/bash

# Personal CRM Smoke Test Script
# This script will turn everything off, start it back up, and test basic functionality

set -e  # Exit on any error

echo "🚀 Starting Personal CRM Smoke Test..."
echo "====================================="

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Log file
LOG_FILE="smoke-test.log"
echo "📝 Logs will be saved to: $LOG_FILE"
echo "$(date): Starting smoke test" > "$LOG_FILE"

# Function to log and print
log_and_print() {
    echo -e "$1"
    echo "$(date): $1" >> "$LOG_FILE"
}

# Function to check if a process is running
check_process() {
    local process_name=$1
    if pgrep -f "$process_name" > /dev/null; then
        return 0
    else
        return 1
    fi
}

# Function to wait for a port to be available
wait_for_port() {
    local port=$1
    local timeout=${2:-30}
    local count=0
    
    log_and_print "${YELLOW}⏳ Waiting for port $port to be available...${NC}"
    
    while ! nc -z localhost "$port" 2>/dev/null; do
        if [ $count -ge $timeout ]; then
            log_and_print "${RED}❌ Timeout waiting for port $port${NC}"
            return 1
        fi
        sleep 1
        count=$((count + 1))
    done
    
    log_and_print "${GREEN}✅ Port $port is available${NC}"
    return 0
}

# Step 1: Turn everything off
log_and_print "${BLUE}🛑 Step 1: Stopping all services...${NC}"

# Kill any existing processes
if check_process "crm-api"; then
    log_and_print "🔸 Stopping backend API..."
    pkill -f "crm-api" || true
    sleep 2
fi

if check_process "next dev"; then
    log_and_print "🔸 Stopping frontend..."
    pkill -f "next dev" || true
    sleep 2
fi

# Stop Docker containers
log_and_print "🔸 Stopping Docker containers..."
make docker-down >> "$LOG_FILE" 2>&1 || true
sleep 3

log_and_print "${GREEN}✅ All services stopped${NC}"

# Step 2: Start everything back up
log_and_print "${BLUE}🚀 Step 2: Starting services...${NC}"

# Start Docker
log_and_print "🔸 Starting Docker containers..."
make docker-up >> "$LOG_FILE" 2>&1
if [ $? -ne 0 ]; then
    log_and_print "${RED}❌ Failed to start Docker containers${NC}"
    exit 1
fi

# Wait for PostgreSQL to be ready
wait_for_port 5432 30
if [ $? -ne 0 ]; then
    log_and_print "${RED}❌ PostgreSQL failed to start${NC}"
    exit 1
fi

# Give PostgreSQL a moment to fully initialize
sleep 5

# Note: Database migrations are now run automatically on backend startup

# Build and start backend API
log_and_print "🔸 Building backend API..."
make api-build >> "$LOG_FILE" 2>&1
if [ $? -ne 0 ]; then
    log_and_print "${RED}❌ Failed to build backend API${NC}"
    exit 1
fi

log_and_print "🔸 Starting backend API..."
# Source environment and start API in background
(
    set -a
    source ./.env
    set +a
    export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"
    ./backend/bin/crm-api >> "$LOG_FILE" 2>&1
) &

# Wait for backend to be ready
wait_for_port 8080 30
if [ $? -ne 0 ]; then
    log_and_print "${RED}❌ Backend API failed to start${NC}"
    exit 1
fi

# Start frontend
log_and_print "🔸 Starting frontend..."
(
    cd frontend
    npm run dev -- --port 3000 >> "../$LOG_FILE" 2>&1
) &

# Wait for frontend to be ready
wait_for_port 3000 30
if [ $? -ne 0 ]; then
    log_and_print "${RED}❌ Frontend failed to start${NC}"
    exit 1
fi

log_and_print "${GREEN}✅ All services started successfully${NC}"

# Step 3: Test basic functionality
log_and_print "${BLUE}🧪 Step 3: Testing basic functionality...${NC}"

# Test 1: Health check
log_and_print "🔸 Testing health endpoint..."
HEALTH_RESPONSE=$(curl -s -w "%{http_code}" http://localhost:8080/health)
HTTP_CODE="${HEALTH_RESPONSE: -3}"
RESPONSE_BODY="${HEALTH_RESPONSE%???}"

if [ "$HTTP_CODE" = "200" ]; then
    log_and_print "${GREEN}✅ Health check passed${NC}"
    echo "Health response: $RESPONSE_BODY" >> "$LOG_FILE"
else
    log_and_print "${RED}❌ Health check failed (HTTP $HTTP_CODE)${NC}"
    echo "Health response: $RESPONSE_BODY" >> "$LOG_FILE"
    exit 1
fi

# Test 2: Contacts endpoint
log_and_print "🔸 Testing contacts endpoint..."
CONTACTS_RESPONSE=$(curl -s -w "%{http_code}" http://localhost:8080/api/v1/contacts)
HTTP_CODE="${CONTACTS_RESPONSE: -3}"
RESPONSE_BODY="${CONTACTS_RESPONSE%???}"

if [ "$HTTP_CODE" = "200" ]; then
    log_and_print "${GREEN}✅ Contacts endpoint working${NC}"
    echo "Contacts response: $RESPONSE_BODY" >> "$LOG_FILE"
else
    log_and_print "${RED}❌ Contacts endpoint failed (HTTP $HTTP_CODE)${NC}"
    echo "Contacts response: $RESPONSE_BODY" >> "$LOG_FILE"
    exit 1
fi

# Test 3: Reminder stats endpoint
log_and_print "🔸 Testing reminder stats endpoint..."
STATS_RESPONSE=$(curl -s -w "%{http_code}" http://localhost:8080/api/v1/reminders/stats)
HTTP_CODE="${STATS_RESPONSE: -3}"
RESPONSE_BODY="${STATS_RESPONSE%???}"

if [ "$HTTP_CODE" = "200" ]; then
    log_and_print "${GREEN}✅ Reminder stats endpoint working${NC}"
    echo "Stats response: $RESPONSE_BODY" >> "$LOG_FILE"
else
    log_and_print "${RED}❌ Reminder stats endpoint failed (HTTP $HTTP_CODE)${NC}"
    echo "Stats response: $RESPONSE_BODY" >> "$LOG_FILE"
    exit 1
fi

# Test 4: Create a test contact
log_and_print "🔸 Testing contact creation..."
TIMESTAMP=$(date +%s)
TEST_CONTACT=$(cat << EOF
{
    "full_name": "Test User",
    "email": "test-${TIMESTAMP}@example.com",
    "phone": "+1234567890",
    "cadence": "weekly",
    "notes": "Smoke test contact"
}
EOF
)

CREATE_RESPONSE=$(curl -s -w "%{http_code}" -X POST http://localhost:8080/api/v1/contacts \
    -H "Content-Type: application/json" \
    -d "$TEST_CONTACT")

HTTP_CODE="${CREATE_RESPONSE: -3}"
RESPONSE_BODY="${CREATE_RESPONSE%???}"

if [ "$HTTP_CODE" = "201" ]; then
    log_and_print "${GREEN}✅ Contact creation working${NC}"
    echo "Create contact response: $RESPONSE_BODY" >> "$LOG_FILE"
    
    # Extract contact ID for cleanup
    CONTACT_ID=$(echo "$RESPONSE_BODY" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
    echo "Created contact ID: $CONTACT_ID" >> "$LOG_FILE"
else
    log_and_print "${RED}❌ Contact creation failed (HTTP $HTTP_CODE)${NC}"
    echo "Create contact response: $RESPONSE_BODY" >> "$LOG_FILE"
    exit 1
fi

# Test 5: Frontend accessibility
log_and_print "🔸 Testing frontend accessibility..."
FRONTEND_RESPONSE=$(curl -s -w "%{http_code}" http://localhost:3000)
HTTP_CODE="${FRONTEND_RESPONSE: -3}"

if [ "$HTTP_CODE" = "200" ]; then
    log_and_print "${GREEN}✅ Frontend accessible${NC}"
else
    log_and_print "${RED}❌ Frontend not accessible (HTTP $HTTP_CODE)${NC}"
    exit 1
fi

# Cleanup: Delete test contact if created
if [ -n "$CONTACT_ID" ]; then
    log_and_print "🔸 Cleaning up test contact..."
    curl -s -X DELETE "http://localhost:8080/api/v1/contacts/$CONTACT_ID" >> "$LOG_FILE" 2>&1
fi

# Step 4: Summary and next steps
log_and_print "${GREEN}🎉 SMOKE TEST PASSED! 🎉${NC}"
log_and_print "=========================="
log_and_print ""
log_and_print "${BLUE}📊 Service Status:${NC}"
log_and_print "• PostgreSQL: Running on port 5432"
log_and_print "• Backend API: Running on port 8080"
log_and_print "• Frontend: Running on port 3000"
log_and_print ""
log_and_print "${BLUE}🌐 Access URLs:${NC}"
log_and_print "• Dashboard: ${YELLOW}http://localhost:3000${NC}"
log_and_print "• API Docs: ${YELLOW}http://localhost:8080/swagger/index.html${NC}"
log_and_print "• Health Check: ${YELLOW}http://localhost:8080/health${NC}"
log_and_print ""
log_and_print "${BLUE}📝 Logs saved to: ${YELLOW}$LOG_FILE${NC}"
log_and_print ""
log_and_print "${GREEN}✨ Ready for feature development! ✨${NC}"

# Function to show running processes
show_processes() {
    log_and_print "${BLUE}📋 Running Processes:${NC}"
    ps aux | grep -E "(crm-api|next dev)" | grep -v grep | while read line; do
        log_and_print "• $line"
    done
}

show_processes

echo ""
echo "🎯 Next steps:"
echo "1. Visit http://localhost:3000 to use the CRM"
echo "2. Add some contacts and test the reminder system"
echo "3. Check the logs if anything seems off"
echo "4. Run this script again anytime you need to restart everything"
echo ""
echo "💡 To stop everything: make docker-down && pkill -f 'crm-api|next dev'"
