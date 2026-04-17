#!/usr/bin/env bash
# check-cadence-sole-writer.sh — belt-and-suspenders grep guard for PR 8.
#
# Ships alongside the AST test at backend/tests/sole_writer_static_test.go.
# Enforces plan Step 13 + Design Decision 9: post-cutover, cadence-writing
# queries and repository wrappers are only reachable from
# backend/internal/consumer/cadence_updater.go (the authoritative writer),
# backend/internal/repository/contact.go (where the wrappers are defined),
# and backend/internal/todoist/provider.go (the two explicit carve-outs).
#
# Exits 0 when the hit set matches the allowlist. Exits 1 (with a diff
# against the allowlist) otherwise. Meant to be run from CI *in addition
# to* the AST test — the AST walk is authoritative, but this script
# produces reviewer-visible file/line evidence that a PR expanded or
# contracted the allowlist.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files that are allowed to call cadence-writing symbols. Paths relative
# to the repo root. Keep in sync with allowedFiles in
# backend/tests/sole_writer_static_test.go and Design Decision 9.
ALLOWLIST=(
  "backend/internal/consumer/cadence_updater.go"
  "backend/internal/repository/contact.go"
  "backend/internal/todoist/provider.go"
)

# Cadence-writing symbols. Any call reaching these outside the allowlist
# violates the sole-writer invariant. The leading `\.` (dot) and trailing
# `\(` (open-paren) together match method-call usage like
# `r.queries.UpdateContactBy(` — and skip noise like handler method
# definitions (`func (h *ContactHandler) UpdateContactLastContacted(...)`),
# router registrations (`contactHandler.UpdateContactLastContacted)`), and
# doc comments that merely mention the name. Keep symbol set in sync with
# cadenceWritingSymbols in backend/tests/sole_writer_static_test.go.
SYMBOLS_PATTERN='\.(UpdateContactLastContacted|UpdateContactLastContactedIfLater|UpdateContactOutreachAt|UpdateContactOutreachAtTx|UpdateContactResponseFields|UpdateContactResponseFieldsTx|UpdateContactMutualFields|UpdateContactMutualFieldsTx|UpdateContactCadenceForward|UpdateContactCadenceUnconditional|UpdateContactBy)\('

# Prefer ripgrep when available (faster + respects .gitignore). Fall back
# to grep -R so the script works on plain CI containers.
if command -v rg >/dev/null 2>&1; then
  HITS=$(rg -l "$SYMBOLS_PATTERN" \
    --glob '*.go' \
    --glob '!**/*_test.go' \
    --glob '!**/contact.sql.go' \
    --glob '!**/querier.go' \
    --glob '!**/models.go' \
    --glob '!**/db.go' \
    backend/internal backend/cmd 2>/dev/null | sort -u || true)
else
  HITS=$(grep -lE "$SYMBOLS_PATTERN" \
    -r --include='*.go' \
    --exclude='*_test.go' \
    --exclude='contact.sql.go' \
    --exclude='querier.go' \
    --exclude='models.go' \
    --exclude='db.go' \
    backend/internal backend/cmd 2>/dev/null | sort -u || true)
fi

# Compare hit set to allowlist. A file that hits the pattern but isn't
# allowlisted is a violation; an allowlisted file that doesn't hit the
# pattern anymore is a cleanup signal (not a failure).
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
  echo "allowedFiles map." >&2
  exit 1
fi

echo "✓ cadence sole-writer grep guard: ${#HIT_LINES[@]} hit file(s), all allowlisted"
