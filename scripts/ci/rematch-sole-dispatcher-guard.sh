#!/usr/bin/env bash
# rematch-sole-dispatcher-guard.sh — grep guard for the event-bus
# rematch dispatcher invariant.
#
# Production rematch dispatch flows through events.Bus.PublishTx →
# RematchDispatcher consumer → RematchService.Run. The legacy
# StartRematchForContact method is kept only for unit tests (see
# service/rematch.go godoc Deprecated: note). Any non-test caller
# indicates a regression in the sole-dispatcher invariant.
#
# Legal call sites for StartRematchForContact:
#   - backend/internal/service/rematch.go        (definition)
#   - backend/internal/service/rematch_test.go   (test coverage)
#   - backend/tests/*_test.go                    (integration tests)
#
# Mirrors scripts/ci/followup-sole-writer-guard.sh.

set -euo pipefail

cd "$(dirname "$0")/../.."

# Match only the call form — `.StartRematchForContact(` — so prose
# comments that reference the name descriptively don't flag.
SYMBOL_PATTERN='\.StartRematchForContact\('

# Only inspect Go source, exclude generated files.
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

# Allow the definition file to reference the symbol.
ALLOWLIST=(
  "backend/internal/service/rematch.go"
)

violation_count=0

HITS=$(find_hits "$SYMBOL_PATTERN")
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
      echo "ERROR: rematch sole-dispatcher guard: StartRematchForContact found in non-test, non-allowlisted file: $f" >&2
      violation_count=$((violation_count + 1))
    fi
  done <<< "$HITS"
fi

if (( violation_count > 0 )); then
  echo "ERROR: production rematch dispatch must flow through events.Bus + RematchDispatcher only." >&2
  exit 1
fi

echo "OK: rematch sole-dispatcher guard: StartRematchForContact has no production callers."
