#!/usr/bin/env bash
# Feed command strings to the policy hook; never execute a push.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
for command in 'git push -n' 'git push --dry-run' 'git push origin feature' 'git status'; do
  jq -n --arg command "$command" '{tool_input: {command: $command}}' |
    bash "$repo/scripts/hooks/block-no-verify.sh"
done
if jq -n '{tool_input: {command: "git push --no-verify"}}' |
  bash "$repo/scripts/hooks/block-no-verify.sh"; then
  echo "FAIL: bypass was allowed" >&2
  exit 1
else
  [[ "$?" == 2 ]]
fi
echo "ALL PASS (dry runs allowed, bypass blocked)"
