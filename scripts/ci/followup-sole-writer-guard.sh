#!/usr/bin/env bash
# followup-sole-writer-guard.sh — belt-and-suspenders grep guard for the
# follow-up manager sole-writer invariant.
#
# Scaffolded inert while FollowUpManager runs in shadow mode — the direct
# path (service/followup.go via FollowUpService.CreateOrRefreshFollowUp /
# CompleteFollowUp) is still the authoritative writer. A later change
# that flips the consumer to sole-writer will remove the early-exit and
# the guard will start enforcing.
#
# Intended final behavior: exit 1 (with a diff against the allowlist) if
# a non-allowlisted file calls CreateOrRefreshFollowUp,
# CompleteFollowUp, RetryPendingCloses, or ListFollowUpsWithPendingClose.
# Mirrors the pattern of scripts/check-cadence-sole-writer.sh.

set -euo pipefail

echo "followup-sole-writer-guard: inert until the follow-up consumer cutover; skipping"
exit 0

# ---------------------------------------------------------------------
# Intended enforcement logic (enabled by the cutover change):
# ---------------------------------------------------------------------
#
# cd "$(dirname "$0")/../.."
#
# ALLOWLIST=(
#   "backend/internal/consumer/followup_manager.go"
#   "backend/internal/consumer/todoist_followup_workers.go"
# )
#
# SYMBOLS_PATTERN='\.(CreateOrRefreshFollowUp|CreateOrRefreshFollowUpObserved|CompleteFollowUp|CompleteFollowUpObserved|RetryPendingCloses|ListFollowUpsWithPendingClose)\('
#
# if command -v rg >/dev/null 2>&1; then
#   HITS=$(rg -l "$SYMBOLS_PATTERN" --glob '*.go' \
#     --glob '!**/*_test.go' --glob '!**/*.sql.go' \
#     --glob '!**/querier.go' --glob '!**/models.go' --glob '!**/db.go' \
#     backend/internal backend/cmd 2>/dev/null | sort -u || true)
# else
#   HITS=$(grep -lE "$SYMBOLS_PATTERN" -r --include='*.go' \
#     --exclude='*_test.go' --exclude='*.sql.go' \
#     --exclude='querier.go' --exclude='models.go' --exclude='db.go' \
#     backend/internal backend/cmd 2>/dev/null | sort -u || true)
# fi
#
# IFS=$'\n' read -r -d '' -a HIT_LINES < <(printf '%s' "$HITS" && printf '\0')
# VIOLATIONS=()
# for f in "${HIT_LINES[@]:-}"; do
#   [[ -z "$f" ]] && continue
#   allowed=false
#   for a in "${ALLOWLIST[@]}"; do
#     [[ "$f" == "$a" ]] && { allowed=true; break; }
#   done
#   $allowed || VIOLATIONS+=("$f")
# done
#
# if [[ ${#VIOLATIONS[@]} -gt 0 ]]; then
#   echo "❌ followup sole-writer grep guard: writer symbols found in non-allowlisted files:" >&2
#   for v in "${VIOLATIONS[@]}"; do echo "   $v" >&2; done
#   exit 1
# fi
#
# echo "✓ followup sole-writer grep guard: ${#HIT_LINES[@]} hit file(s), all allowlisted"
