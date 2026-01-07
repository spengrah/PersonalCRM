#!/bin/bash
set -e
# Save project dir before changing to hooks dir
export CLAUDE_PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$(pwd)}"
cd "$(dirname "$0")"
cat | node dist/session-end-cleanup.mjs
