#!/bin/bash
# Install git hooks from templates

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HOOKS_DIR="$PROJECT_ROOT/.git/hooks"
TEMPLATES_DIR="$PROJECT_ROOT/.git-hooks"

echo "Installing git hooks from templates..."

# Check if .git directory exists
if [ ! -d "$PROJECT_ROOT/.git" ]; then
  echo "Error: Not a git repository"
  exit 1
fi

# Install pre-commit hook
if [ -f "$TEMPLATES_DIR/pre-commit.template" ]; then
  cp "$TEMPLATES_DIR/pre-commit.template" "$HOOKS_DIR/pre-commit"
  chmod +x "$HOOKS_DIR/pre-commit"
  echo "✓ Installed pre-commit hook (gofmt + prettier)"
else
  echo "Warning: pre-commit.template not found"
fi

# Install pre-push hook
if [ -f "$TEMPLATES_DIR/pre-push.template" ]; then
  cp "$TEMPLATES_DIR/pre-push.template" "$HOOKS_DIR/pre-push"
  chmod +x "$HOOKS_DIR/pre-push"
  echo "✓ Installed pre-push hook (make lint)"
else
  echo "Warning: pre-push.template not found"
fi

echo ""
echo "Git hooks installed successfully!"
echo ""
echo "Hooks installed:"
echo "  - pre-commit: Auto-formats Go and frontend files (gofmt + prettier)"
echo "  - pre-push: Runs golangci-lint and ESLint before push"
echo ""
echo "To bypass hooks (not recommended): git commit --no-verify / git push --no-verify"
