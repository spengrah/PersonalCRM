#!/usr/bin/env bash
# crm-marker-construction-guard.sh — grep guard for the CRM-marker
# construction invariant.
#
# The Todoist CRM-marker wire format must be built in exactly one place:
# contacttask.EncodeMarker (backend/internal/contacttask/marker.go). Any
# other file that constructs the marker risks drifting the wire format
# away from the single sanctioned encoder.
#
# Fingerprint (three patterns, OR'd together):
#   (a) map-literal / JSON key  "crm" : true        — today's form
#   (b) struct field tag        json:"crm"          — a struct encoder
#   (c) escaped-string JSON     \"crm\":true        — hand-built literals
#
# Allowlist = exactly one file. After the refactor, no other file builds
# the marker, so the only legitimate hit is the primitive. The companion
# AST test (backend/tests/crm_marker_construction_static_test.go) enforces
# function-level precision within that file.
#
# Mirrors scripts/check-cadence-sole-writer.sh and
# scripts/ci/followup-sole-writer-guard.sh.

set -euo pipefail

cd "$(dirname "$0")/../.."

ALLOWLIST=(
  "backend/internal/contacttask/marker.go"
)

# (a) "crm" : true   (b) json:"crm"   (c) \"crm\":true
PATTERN='"crm"[[:space:]]*:[[:space:]]*true|json:"crm"|\\"crm\\"[[:space:]]*:[[:space:]]*true'

if command -v rg >/dev/null 2>&1; then
  HITS=$(rg -l "$PATTERN" \
    --glob '*.go' \
    --glob '!**/*_test.go' \
    --glob '!**/*.sql.go' \
    --glob '!**/querier.go' \
    --glob '!**/models.go' \
    --glob '!**/db.go' \
    backend/internal backend/cmd 2>/dev/null | sort -u || true)
else
  HITS=$(grep -lE "$PATTERN" \
    -r --include='*.go' \
    --exclude='*_test.go' \
    --exclude='*.sql.go' \
    --exclude='querier.go' \
    --exclude='models.go' \
    --exclude='db.go' \
    backend/internal backend/cmd 2>/dev/null | sort -u || true)
fi

violation_count=0
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
      echo "❌ CRM-marker construction guard: marker fingerprint found in non-allowlisted file: $f" >&2
      if command -v rg >/dev/null 2>&1; then
        rg -n "$PATTERN" "$f" >&2 | sed 's/^/      /'
      else
        grep -nE "$PATTERN" "$f" >&2 | sed 's/^/      /'
      fi
      violation_count=$((violation_count + 1))
    fi
  done <<< "$HITS"
fi

if (( violation_count > 0 )); then
  echo >&2
  echo "Fix: build the CRM marker via contacttask.EncodeMarker — it is the" >&2
  echo "single sanctioned encoder. Do not inline the marker map/struct elsewhere." >&2
  exit 1
fi

echo "✓ CRM-marker construction guard: marker fingerprint confined to contacttask/marker.go"
