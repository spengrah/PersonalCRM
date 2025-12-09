#!/bin/bash
# Start frontend development server detached from terminal

cd frontend
# Use nohup in a subshell that exits immediately
(nohup npm run dev > ../logs/frontend-dev.log 2>&1 &)
sleep 3
# Get the actual PID of next/node process
ACTUAL_PID=$(pgrep -f "next dev" | head -1)
if [ -z "$ACTUAL_PID" ]; then
    ACTUAL_PID=$(pgrep -f "node.*next" | head -1)
fi
if [ -n "$ACTUAL_PID" ]; then
    echo $ACTUAL_PID > ../logs/frontend-dev.pid
    echo "Frontend dev server started with PID: $ACTUAL_PID"
    echo "Frontend should be available at http://localhost:3000"
else
    echo "Warning: Could not determine PID, but process may be running"
    echo "Check logs/frontend-dev.log for details"
fi



