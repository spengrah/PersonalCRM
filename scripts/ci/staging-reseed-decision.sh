#!/usr/bin/env bash
# staging-reseed-decision.sh <BASE> <HEAD>
#
# Decides whether the develop->staging auto-reseed should fire, by diffing the
# git tree currently LIVE on staging (BASE — the host-pinned 40-hex Image= sha,
# read by staging-deployed-sha.sh BEFORE the deploy swaps it) against the SHA
# being deployed (HEAD). Writes three flags to $GITHUB_ENV (and echoes them for
# local runs / tests):
#   seed_changed        any changed file is in the `seed` path-filters group
#                       (backend/internal/synthetic/**) -> reseed candidate.
#   migrations_changed  any changed file is under backend/migrations/ -> nudge only
#                       (migrations transform the accumulated world; never auto-wipe).
#   base_known          true iff BASE was a resolvable commit and the diff ran.
#
# TWO-DOT `git diff BASE HEAD` (direct tree-vs-tree), NOT three-dot (BASE...HEAD,
# merge-base): the comparison is live-tree vs target-tree, so a rebase/force-push
# or non-linear history must not shift the base to a merge-base.
#
# FAULT-TOLERANT BY CONTRACT: this runs as a post-deploy step in deploy-staging.yml
# AFTER the code+migrate deploy already succeeded, so it must NEVER fail the job.
# Any ambiguity — empty/unresolvable BASE (digest pin / first-ever deploy /
# unreadable), BASE absent from local history (force-push), a missing source file,
# or any git error — degrades to seed_changed=false + base_known=false and exit 0
# (conservative: never auto-wipe on uncertainty; persistence wins). The workflow
# turns base_known=false into a "could not determine base" nudge.
#
# The `seed` group is matched via file_in_group sourced from the pre-push hook —
# the SAME matcher CI/pre-push use, so the seed surface stays single-sourced in
# path-filters.yml with no second hardcoded synthetic prefix. file_in_group reads
# the global FILTERS_FILE, which we re-point at the real (absolute) path-filters.yml
# so the seed definition comes from THIS checkout while `git diff` runs in the CWD
# (the deploy checkout in prod; a throwaway fixture repo in the tests).

set -uo pipefail   # NO -e: a failed diff/source must never exit non-zero.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)"

emit() {
  # emit <key> <value>: stdout for humans/tests + $GITHUB_ENV for the workflow.
  printf '%s=%s\n' "$1" "$2"
  if [ -n "${GITHUB_ENV:-}" ]; then
    printf '%s=%s\n' "$1" "$2" >> "$GITHUB_ENV"
  fi
}

# Conservative degrade: no reseed, base unknown. Every uncertainty path ends here.
emit_unknown() {
  emit seed_changed false
  emit migrations_changed false
  emit base_known false
  exit 0
}

BASE="${1:-}"
HEAD="${2:-}"

# Need both endpoints and a resolvable repo root.
[ -n "$BASE" ] || emit_unknown
[ -n "$HEAD" ] || emit_unknown
[ -n "$REPO_ROOT" ] || emit_unknown

# file_in_group lives in the pre-push hook; the source-guard keeps the hook body
# from running. Source by absolute path so this is CWD-independent, then re-point
# FILTERS_FILE at the real path-filters.yml (the matcher reads that global).
# shellcheck source=/dev/null
source "$REPO_ROOT/scripts/hooks/pre-push" 2>/dev/null || emit_unknown
declare -f file_in_group >/dev/null 2>&1 || emit_unknown
FILTERS_FILE="$REPO_ROOT/path-filters.yml"
[ -f "$FILTERS_FILE" ] || emit_unknown

# Both endpoints must be real commits in the CWD repo (force-push / first-deploy /
# digest-pin base -> not resolvable -> degrade).
git cat-file -e "${BASE}^{commit}" 2>/dev/null || emit_unknown
git cat-file -e "${HEAD}^{commit}" 2>/dev/null || emit_unknown

# TWO-DOT diff: live tree (BASE) vs target tree (HEAD).
changed="$(git diff --name-only "$BASE" "$HEAD" 2>/dev/null)" || emit_unknown

seed_changed=false
migrations_changed=false
while IFS= read -r f; do
  [ -n "$f" ] || continue
  if file_in_group "$f" seed; then
    seed_changed=true
  fi
  case "$f" in
    backend/migrations/*) migrations_changed=true ;;
  esac
done <<< "$changed"

emit seed_changed "$seed_changed"
emit migrations_changed "$migrations_changed"
emit base_known true
exit 0
