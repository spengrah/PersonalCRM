#!/bin/bash
set -e

# Claude Code PreToolUse hook: Ensure tests pass before git push
#
# This hook intercepts git push commands from Claude agents and runs
# the full test suite before allowing the push to proceed.
#
# Exit codes:
#   0 = Allow command to proceed
#   2 = Block command (tests failed)

# Read JSON input from stdin (Claude passes tool context)
hook_input=$(cat)
command=$(echo "$hook_input" | jq -r '.tool_input.command // ""')

# Only intercept git push commands (not other bash commands)
if [[ ! "$command" =~ ^git[[:space:]]+push ]] && [[ ! "$command" =~ \&\&[[:space:]]*git[[:space:]]+push ]]; then
  exit 0
fi

echo "══════════════════════════════════════════════════════════" >&2
echo "Pre-push hook: Running tests before push..." >&2
echo "══════════════════════════════════════════════════════════" >&2

cd "$CLAUDE_PROJECT_DIR" || {
  echo "Failed to cd to project directory" >&2
  exit 2
}

# Run unit tests
echo "→ Running unit tests (make test-unit)..." >&2
if ! make test-unit 2>&1; then
  echo "" >&2
  echo "❌ Unit tests failed. Fix before pushing." >&2
  exit 2
fi
echo "✓ Unit tests passed" >&2

# Run integration tests
echo "→ Running integration tests (make test-integration)..." >&2
if ! make test-integration 2>&1; then
  echo "" >&2
  echo "❌ Integration tests failed. Fix before pushing." >&2
  exit 2
fi
echo "✓ Integration tests passed" >&2

# Run frontend tests
echo "→ Running frontend tests (make test-frontend)..." >&2
if ! make test-frontend 2>&1; then
  echo "" >&2
  echo "❌ Frontend tests failed. Fix before pushing." >&2
  exit 2
fi
echo "✓ Frontend tests passed" >&2

# Run E2E tests
echo "→ Running E2E tests (make test-e2e)..." >&2
if ! make test-e2e 2>&1; then
  echo "" >&2
  echo "❌ E2E tests failed. Fix before pushing." >&2
  exit 2
fi
echo "✓ E2E tests passed" >&2

echo "══════════════════════════════════════════════════════════" >&2
echo "✓ All tests passed. Proceeding with push." >&2
echo "══════════════════════════════════════════════════════════" >&2
exit 0
