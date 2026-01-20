#!/bin/bash
# Block git push --no-verify and git push -n
# Used by Claude Code PreToolUse hook

# Read JSON input from stdin
input=$(cat)
command=$(echo "$input" | jq -r '.tool_input.command // ""')

# Check if this is a git push with --no-verify or -n flag
if [[ "$command" =~ git[[:space:]]+push.*--no-verify ]] || [[ "$command" =~ git[[:space:]]+push.*[[:space:]]-n([[:space:]]|$) ]]; then
  echo "Blocked: Cannot skip pre-push hooks (--no-verify or -n flag)" >&2
  exit 2
fi

exit 0
