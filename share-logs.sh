#!/bin/bash

# Share Logs Script - Collects all relevant logs for debugging

echo "📋 Collecting logs for debugging..."
echo "================================="

LOG_BUNDLE="debug-logs-$(date +%Y%m%d-%H%M%S).txt"

{
    echo "=== PERSONAL CRM DEBUG LOGS ==="
    echo "Generated: $(date)"
    echo "================================="
    echo ""
    
    echo "=== SYSTEM INFO ==="
    echo "OS: $(uname -a)"
    echo "Docker version:"
    docker --version 2>/dev/null || echo "Docker not available"
    echo "Node version:"
    node --version 2>/dev/null || echo "Node not available"
    echo "Go version:"
    go version 2>/dev/null || echo "Go not available"
    echo ""
    
    echo "=== RUNNING PROCESSES ==="
    ps aux | grep -E "(crm-api|next dev|postgres)" | grep -v grep
    echo ""
    
    echo "=== DOCKER CONTAINERS ==="
    docker ps -a 2>/dev/null || echo "Docker not running"
    echo ""
    
    echo "=== PORT STATUS ==="
    echo "Port 3000 (Frontend):"
    nc -z localhost 3000 && echo "✅ Open" || echo "❌ Closed"
    echo "Port 8080 (Backend):"
    nc -z localhost 8080 && echo "✅ Open" || echo "❌ Closed"
    echo "Port 5432 (Database):"
    nc -z localhost 5432 && echo "✅ Open" || echo "❌ Closed"
    echo ""
    
    echo "=== API HEALTH CHECK ==="
    curl -s http://localhost:8080/health 2>/dev/null || echo "❌ API not responding"
    echo ""
    
    echo "=== CONTACTS API TEST ==="
    curl -s http://localhost:8080/api/v1/contacts 2>/dev/null || echo "❌ Contacts API not responding"
    echo ""
    
    echo "=== REMINDER STATS TEST ==="
    curl -s http://localhost:8080/api/v1/reminders/stats 2>/dev/null || echo "❌ Reminders API not responding"
    echo ""
    
    echo "=== ENVIRONMENT VARIABLES ==="
    echo "DATABASE_URL: ${DATABASE_URL:-'Not set'}"
    echo "NODE_ENV: ${NODE_ENV:-'Not set'}"
    echo "PORT: ${PORT:-'Not set'}"
    echo ""
    
    echo "=== SMOKE TEST LOG (if exists) ==="
    if [ -f "smoke-test.log" ]; then
        echo "Last 50 lines of smoke-test.log:"
        tail -50 smoke-test.log
    else
        echo "No smoke-test.log found"
    fi
    echo ""
    
    echo "=== DOCKER LOGS ==="
    if docker ps | grep -q crm-postgres; then
        echo "PostgreSQL container logs (last 20 lines):"
        docker logs --tail 20 crm-postgres 2>/dev/null
    else
        echo "PostgreSQL container not running"
    fi
    echo ""
    
    echo "=== FILE STRUCTURE ==="
    echo "Key files present:"
    ls -la backend/bin/crm-api 2>/dev/null && echo "✅ Backend binary exists" || echo "❌ Backend binary missing"
    ls -la backend/migrations/*.sql 2>/dev/null && echo "✅ Migration files exist" || echo "❌ Migration files missing"
    ls -la frontend/package.json 2>/dev/null && echo "✅ Frontend package.json exists" || echo "❌ Frontend package.json missing"
    ls -la .env 2>/dev/null && echo "✅ .env file exists" || echo "❌ .env file missing"
    echo ""
    
} > "$LOG_BUNDLE"

echo "✅ Debug logs collected in: $LOG_BUNDLE"
echo ""
echo "📤 To share these logs:"
echo "1. Copy the contents of $LOG_BUNDLE"
echo "2. Paste into your message to the assistant"
echo "3. Or run: cat $LOG_BUNDLE"
echo ""
echo "💡 Quick copy command:"
echo "cat $LOG_BUNDLE | pbcopy"

