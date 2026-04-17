#!/usr/bin/env bash
# check-cadence-sole-writer.sh — belt-and-suspenders grep guard.
#
# Ships alongside the AST test at backend/tests/sole_writer_static_test.go.
# Enforces that cadence-writing queries are only reachable from
# backend/internal/consumer/cadence_updater.go (the authoritative
# writer), the repository wrapper methods themselves, and the two
# explicit Todoist carve-outs.
#
# Enforcement detail:
#   - Inventory includes CreateContact + UpdateContact, scoped to sqlc
#     Querier receivers (`queries.X`, `db.New(tx).X`, `txQueries.X`) so
#     wrapper-to-wrapper calls from the service layer are NOT flagged.
#   - Allowlist is file-level here (the belt); function-level precision
#     lives in the AST test (the suspenders). Any file that matches the
#     grep pattern MUST also be covered by the AST allowlist.
#
# Exits 0 when the hit set matches the allowlist. Exits 1 (with a diff
# against the allowlist) otherwise.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files that are allowed to contain cadence-writing call sites. Paths
# relative to the repo root. The AST test enforces function-level
# precision within each file; this script is the coarse first check.
ALLOWLIST=(
  "backend/internal/consumer/cadence_updater.go"
  "backend/internal/repository/contact.go"
  "backend/internal/todoist/provider.go"
)

# Cadence-writing symbols with their receiver-scope restrictions. Most
# names are distinctive and can match on `\.SYMBOL\(` alone; the two
# collision-prone names (CreateContact, UpdateContact) require an sqlc
# Querier receiver to avoid flagging every service/handler caller.
#
# Keep in sync with cadenceWritingSymbols + querierScopedSymbols in
# backend/tests/sole_writer_static_test.go.
DISTINCTIVE_PATTERN='\.(UpdateContactLastContacted|UpdateContactLastContactedIfLater|UpdateContactOutreachAt|UpdateContactOutreachAtTx|UpdateContactResponseFields|UpdateContactResponseFieldsTx|UpdateContactMutualFields|UpdateContactMutualFieldsTx|UpdateContactCadenceForward|UpdateContactCadenceUnconditional|UpdateContactBy)\('

# Querier-scoped CreateContact/UpdateContact. Match only when the
# receiver chain looks like an sqlc Querier: `queries.X(`, `q.X(`,
# `txQueries.X(`, or `db.New(tx).X(`.
QUERIER_SCOPED_PATTERN='((\.queries|\.q|\.txQueries|queries|txQueries|db\.New\([^)]*\))\.(CreateContact|UpdateContact))\('

SYMBOLS_PATTERN="${DISTINCTIVE_PATTERN}|${QUERIER_SCOPED_PATTERN}"

# Prefer ripgrep when available (faster + respects .gitignore). Fall back
# to grep -R so the script works on plain CI containers.
if command -v rg >/dev/null 2>&1; then
  HITS=$(rg -l "$SYMBOLS_PATTERN" \
    --glob '*.go' \
    --glob '!**/*_test.go' \
    --glob '!**/*.sql.go' \
    --glob '!**/querier.go' \
    --glob '!**/models.go' \
    --glob '!**/db.go' \
    backend/internal backend/cmd 2>/dev/null | sort -u || true)
else
  HITS=$(grep -lE "$SYMBOLS_PATTERN" \
    -r --include='*.go' \
    --exclude='*_test.go' \
    --exclude='*.sql.go' \
    --exclude='querier.go' \
    --exclude='models.go' \
    --exclude='db.go' \
    backend/internal backend/cmd 2>/dev/null | sort -u || true)
fi

# Compare hit set to allowlist.
IFS=$'\n' read -r -d '' -a HIT_LINES < <(printf '%s' "$HITS" && printf '\0')
VIOLATIONS=()
for f in "${HIT_LINES[@]:-}"; do
  [[ -z "$f" ]] && continue
  allowed=false
  for a in "${ALLOWLIST[@]}"; do
    if [[ "$f" == "$a" ]]; then
      allowed=true
      break
    fi
  done
  if ! $allowed; then
    VIOLATIONS+=("$f")
  fi
done

if [[ ${#VIOLATIONS[@]} -gt 0 ]]; then
  echo "❌ cadence sole-writer grep guard: cadence-writing symbols found in non-allowlisted files:" >&2
  for v in "${VIOLATIONS[@]}"; do
    echo "   $v" >&2
    if command -v rg >/dev/null 2>&1; then
      rg -n "$SYMBOLS_PATTERN" "$v" >&2 | sed 's/^/      /'
    else
      grep -nE "$SYMBOLS_PATTERN" "$v" >&2 | sed 's/^/      /'
    fi
  done
  echo >&2
  echo "Fix: route the write through CadenceUpdater, or justify + allowlist the file" >&2
  echo "in both this script's ALLOWLIST and backend/tests/sole_writer_static_test.go's" >&2
  echo "allowedCallSites map." >&2
  exit 1
fi

echo "✓ cadence sole-writer grep guard: ${#HIT_LINES[@]} hit file(s), all allowlisted"
