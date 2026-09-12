#!/bin/bash
# Block git push --no-verify. The -n/--dry-run options are allowed.
# Used by Claude Code PreToolUse hook

# Read JSON input from stdin
input=$(cat)
command=$(echo "$input" | jq -r '.tool_input.command // ""')

# Check if this is a git push with --no-verify.
if [[ "$command" =~ git[[:space:]]+push.*--no-verify ]]; then
  echo "Blocked: Cannot skip pre-push hooks (--no-verify)" >&2
  exit 2
fi

exit 0
