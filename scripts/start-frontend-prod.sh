#!/bin/bash
# Start production frontend server detached from terminal

cd frontend
# Use nohup in a subshell that exits immediately
(nohup sh -c "PORT=3001 npm run start" > ../logs/frontend.log 2>&1 &)
sleep 2
# Get the actual PID of npm/node process
ACTUAL_PID=$(pgrep -f "PORT=3001 npm run start" | head -1)
if [ -z "$ACTUAL_PID" ]; then
    ACTUAL_PID=$(pgrep -f "next start" | head -1)
fi
if [ -n "$ACTUAL_PID" ]; then
    echo $ACTUAL_PID > ../logs/frontend.pid
    echo "Frontend started with PID: $ACTUAL_PID"
else
    echo "Warning: Could not determine PID"
fi

