#!/usr/bin/env bash
# followup-sole-writer-guard.sh — grep guard for the follow-up manager
# sole-writer invariant.
#
# After the FollowUpManager cutover, only consumer/followup_manager.go is
# allowed to reference the former direct-path writer symbols. Also
# checks that the retired todoist_close_pending metadata key is no
# longer referenced anywhere in the Go source — the key was written by
# the deleted FollowUpService.completeFollowUpInner, and its persistence
# would indicate a partial revert.
#
# Mirrors scripts/check-cadence-sole-writer.sh.

set -euo pipefail

cd "$(dirname "$0")/../.."

ALLOWLIST=(
  "backend/internal/consumer/followup_manager.go"
)

SYMBOLS_PATTERN='\.(CreateOrRefreshFollowUp|CreateOrRefreshFollowUpObserved|CompleteFollowUp|CompleteFollowUpObserved|RetryPendingCloses|ListFollowUpsWithPendingClose)\('

LITERAL_PATTERN='todoist_close_pending'

find_hits() {
  local pattern="$1"
  if command -v rg >/dev/null 2>&1; then
    rg -l "$pattern" --glob '*.go' \
      --glob '!**/*_test.go' --glob '!**/*.sql.go' \
      --glob '!**/querier.go' --glob '!**/models.go' --glob '!**/db.go' \
      backend/internal backend/cmd 2>/dev/null | sort -u || true
  else
    grep -lE "$pattern" -r --include='*.go' \
      --exclude='*_test.go' --exclude='*.sql.go' \
      --exclude='querier.go' --exclude='models.go' --exclude='db.go' \
      backend/internal backend/cmd 2>/dev/null | sort -u || true
  fi
}

violation_count=0

# Symbol check.
HITS=$(find_hits "$SYMBOLS_PATTERN")
if [[ -n "$HITS" ]]; then
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    allowed=false
    for a in "${ALLOWLIST[@]}"; do
      if [[ "$f" == "$a" ]]; then
        allowed=true
        break
      fi
    done
    if ! $allowed; then
      echo "❌ followup sole-writer guard: writer symbol found in non-allowlisted file: $f" >&2
      violation_count=$((violation_count + 1))
    fi
  done <<< "$HITS"
fi

# Literal check — todoist_close_pending must be dead code.
LITERAL_HITS=$(find_hits "$LITERAL_PATTERN")
if [[ -n "$LITERAL_HITS" ]]; then
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    echo "❌ followup sole-writer guard: retired metadata key 'todoist_close_pending' referenced in: $f" >&2
    violation_count=$((violation_count + 1))
  done <<< "$LITERAL_HITS"
fi

if (( violation_count > 0 )); then
  exit 1
fi

echo "✓ followup sole-writer guard: all writer symbols confined to consumer/followup_manager.go; todoist_close_pending literal absent"
