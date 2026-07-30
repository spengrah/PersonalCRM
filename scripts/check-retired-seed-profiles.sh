#!/usr/bin/env bash
# check-retired-seed-profiles.sh — grep guard for the RETIRED seed profile names.
#
# The `dev` and `prod-shaped` catalog profiles are deleted and `ProfileParams`
# refuses both by name. A surviving reference is not cosmetic: an operator who
# follows a documented `--profile dev` command gets a hard refusal, and a script
# that still defaults to a retired world fails at reseed time — after it has
# already stopped the backend.
#
# A bare "zero hits in the tree" scan CANNOT gate this, because some occurrences
# must survive: the refusal test names both retired literals, and the retirement
# rationale plus the dated design docs describe what was removed. A scan that
# cannot tell those from a stale operator instruction is not a gate. So every
# permitted occurrence is enumerated below WITH ITS REASON, anything outside the
# allowlist fails, and an allowlisted path that no longer matches ALSO fails —
# the allowlist cannot outlive its subject.
#
# Scope is the tracked tree (`git ls-files`): that is what ships, and it excludes
# build output, node_modules, linked worktrees and the gitignored plan/progress
# evidence without a hand-maintained exclude list.
#
# Wired in two places: a `make lint` prerequisite, so the pre-push LINT lane runs
# it on EVERY push regardless of the changed paths, and a named CI step in the
# Backend Quality job, which the `backend` path group gates (scripts/** and
# Makefile are in it).
#
# Exits 0 when every match is allowlisted and every allowlist entry still
# matches, 1 otherwise.

set -euo pipefail

# Optional scan root (default: the repo this script lives in). The self-test
# passes a temp git repo so it can prove the guard actually fails.
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$ROOT"

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "❌ retired-seed-profile guard: $ROOT is not a git work tree" >&2
  exit 1
fi

# Retired names, in the shapes they actually appear in. Deliberately NOT the bare
# word `dev`, which is a legitimate everyday word in this tree.
PATTERNS=(
  # Deleted Go identifiers.
  'ProfileDev\b'
  'ProfileProdShaped\b'
  'runCatalogProfile\b'
  # The retired world name in any spelling.
  'prod-?shaped'
  # Retired operator command, script default, and summary/provenance forms.
  '--profile[ =]+(dev|prod-shaped)\b'
  '(DEV_SEED|STAGING_RESET|TOURS_SEED)_PROFILE(:-|=)"?(dev|prod-shaped)\b'
  'profile=(dev|prod-shaped)\b'
  # The retired world's prose name, bare or quoted (`dev`, "dev", 'dev').
  '[[:punct:]]?dev[[:punct:]]? synthetic world'
)

COMBINED=""
for p in "${PATTERNS[@]}"; do
  COMBINED="${COMBINED:+$COMBINED|}$p"
done

# Allowlist: `path|reason`, one per line. Keep it NARROW — an entry here is a
# statement that this exact file is supposed to name a retired profile.
ALLOWLIST='scripts/check-retired-seed-profiles.sh|this guard names the retired profiles in its own pattern list
scripts/check-retired-seed-profiles.test.sh|the guard self-test feeds retired literals to the guard as fixtures
backend/internal/synthetic/profiles_test.go|TestProfileParams asserts both retired names are REFUSED with an unknown-profile error
.ai/patterns/synthetic-seed-toolkit.md|records what the retired catalog profiles were and why they were deleted
.ai/spec/2026-06-07-synthetic-seed-generator-design.md|dated design doc, superseded in place
.ai/spec/2026-06-07-staging-environment-design.md|dated design doc, superseded in place
.ai/spec/2026-07-08-piece4-track-b-agentic-qa-harness-design.md|dated design doc, superseded in place
.ai/spec/2026-07-12-langfuse-as-qa-ssot-plan.md|dated design doc, superseded in place
.ai/spec/2026-07-13-synthetic-seed-fidelity-audit.md|dated design doc, superseded in place'

# History is permitted only when it SAYS it is history: an allowlisted
# `.ai/spec/` doc must carry the supersession note, so a dated doc cannot quietly
# read as a current instruction.
SUPERSEDED_MARKER='Superseded on the seed profile'

is_allowed() {
  printf '%s\n' "$ALLOWLIST" | cut -d'|' -f1 | grep -qxF "$1"
}

if [ -z "$(git ls-files | head -n 1)" ]; then
  echo "❌ retired-seed-profile guard: no tracked files under $ROOT" >&2
  exit 1
fi

HITS="$(git ls-files -z | xargs -0 grep -InHE "$COMBINED" -- 2>/dev/null || true)"

STALE=""
ALLOWED_COUNT=0
if [ -n "$HITS" ]; then
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    file="${hit%%:*}"
    if is_allowed "$file"; then
      ALLOWED_COUNT=$((ALLOWED_COUNT + 1))
    else
      STALE="${STALE}${hit}
"
    fi
  done <<EOF
$HITS
EOF
fi

FAILED=0

if [ -n "$STALE" ]; then
  echo "❌ retired-seed-profile guard: reference(s) to a RETIRED seed profile outside the allowlist:" >&2
  printf '%s' "$STALE" | sed 's/^/   /' >&2
  echo >&2
  echo "The surviving worlds are 'standard' (the default everywhere) and 'minimal-scoped'" >&2
  echo "(an explicit operator override). Update the reference, or — if the occurrence is" >&2
  echo "deliberate (a refusal test, or history that says it is history) — add the file to" >&2
  echo "the ALLOWLIST in this script WITH A REASON." >&2
  FAILED=1
fi

# An allowlist entry whose subject is gone is a hole in the gate, not a comment.
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  path="${entry%%|*}"
  reason="${entry#*|}"
  if [ ! -f "$path" ]; then
    echo "❌ retired-seed-profile guard: allowlisted path no longer exists: $path ($reason)" >&2
    echo "   Remove the entry." >&2
    FAILED=1
    continue
  fi
  if ! grep -qIE "$COMBINED" -- "$path"; then
    echo "❌ retired-seed-profile guard: allowlisted path no longer names a retired profile: $path" >&2
    echo "   Remove the entry — a stale allowlist silently widens the gate." >&2
    FAILED=1
    continue
  fi
  case "$path" in
    .ai/spec/*)
      if ! grep -qF "$SUPERSEDED_MARKER" -- "$path"; then
        echo "❌ retired-seed-profile guard: $path names a retired profile without the" >&2
        echo "   supersession note (\"$SUPERSEDED_MARKER\"). A dated doc may keep its history," >&2
        echo "   but it must say so." >&2
        FAILED=1
      fi
      ;;
  esac
done <<EOF
$ALLOWLIST
EOF

if [ "$FAILED" -ne 0 ]; then
  exit 1
fi

echo "✓ retired-seed-profile guard: no stale 'dev'/'prod-shaped' references ($ALLOWED_COUNT allowlisted occurrence(s))"
