#!/bin/bash
# Configure git to use project hooks from scripts/hooks/

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$PROJECT_ROOT"

git config core.hooksPath scripts/hooks

echo "Git hooks configured."
echo "  hooks path: scripts/hooks/"
echo "  pre-commit: auto-formats Go and frontend files"
echo "  pre-push: lint, swift (mac-daemon), tests"
