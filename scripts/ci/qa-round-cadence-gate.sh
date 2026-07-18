#!/usr/bin/env bash
# qa-round-cadence-gate.sh <BASE> <HEAD> <DAYS_SINCE>
#
# Decides whether the nightly QA judge round should run. BASE = the LAST round's
# deployed sha (persisted by the wrapper); HEAD = the currently-deployed staging
# sha (read by the wrapper via a staging-deployed-sha.sh-style step); DAYS_SINCE =
# whole days since the last judge round (wrapper-supplied). Truth table:
#   1. HEAD == BASE (staging unchanged)                 -> SKIP (run_round=false).
#   2. else (staging changed):
#      a. judge-relevant path-filter delta over BASE..HEAD -> RUN (fast path).
#      b. else DAYS_SINCE >= MAX_STALENESS_DAYS             -> RUN (staleness floor).
#      c. else                                             -> SKIP.
#   3. any uncertainty resolving BASE/HEAD/DAYS_SINCE (or filters/git/temp/read)
#                                                        -> SKIP (run_round=false).
# Net: zero staging changes -> skip indefinitely; any change -> judged within
# MAX_STALENESS_DAYS at the latest, sooner if judge-relevant. Emits four flags to
# stdout (and $GITHUB_ENV when set) for the wrapper / CI:
#   run_round             true to run (relevant delta OR staleness floor); false to
#                         skip (unchanged, within-floor, OR any uncertainty).
#   judge_relevant_change the honest "did the groups change" signal: true/false on a
#                         clean decision, `unknown` on an uncertain skip. A floor-
#                         forced run reads run_round=true + judge_relevant_change=false.
#   base_known            true on a clean, resolved decision (a run, a within-floor
#                         skip, OR an equal-endpoints skip); false only on an
#                         uncertain skip.
#   changed_groups        sorted CSV of which of backend/frontend/seed matched
#                         (empty when none matched, and empty on any skip).
#
# DEGRADE TO SKIP — this advisory, eventual-convergence gate fails toward NOT
# running (conservative on judge-token spend). The staleness floor bounds how much
# can slip, and a MANUAL trigger is the backstop, so uncertainty need not force a
# run. EVERY uncertainty emits run_round=false (+ base_known=false): an unresolvable
# BASE/HEAD (incl. a force-push that dropped an endpoint), a missing/unreadable
# path-filters.yml, an unsourceable / absent file_in_group, a temp-file/git/read
# failure, or an unusable DAYS_SINCE. There is NO filter-validation apparatus: a
# damaged filter simply fails to match, an unreadable/missing one skips; either way
# the round is skipped, which is correct here. (The SEPARATE deployed-sha-assert
# script is an integrity gate and stays FAIL-CLOSED, not degrade-to-skip.)
#
# TWO-DOT `git diff BASE HEAD` (direct tree-vs-tree), NOT three-dot (BASE...HEAD,
# merge-base): the comparison is last-round-tree vs deployed-tree, so a
# rebase/force-push or non-linear history must NOT shift the base to a merge-base.
# A resolvable non-linear/divergent BASE+HEAD pair is NOT uncertain — it is diffed
# with the required two-dot form. The diff is read NUL-delimited (`-z`) so a path
# with special characters is matched (kept for FAST-path correctness; the floor
# backstops anything the match still misses).
#
# The groups are matched via file_in_group sourced from the pre-push hook — the
# SAME matcher CI/pre-push use, so the judge-input surface stays single-sourced in
# path-filters.yml with no second hardcoded prefix. file_in_group reads the global
# FILTERS_FILE, re-pointed at the real (absolute) path-filters.yml so the group
# definitions come from THIS checkout while `git diff` runs in the CWD (the deploy
# checkout in prod; a throwaway fixture repo in the tests).
#
# NOTE (vs the reseed decision): a test-only synthetic change (`*_test.go` /
# `**/testdata/**` under backend/internal/synthetic/**) is NOT the built seed
# surface, but it DOES match the `backend` group, so it sets run_round=true anyway.
# We deliberately do NOT special-case it: it is a real judge-input change, so
# running is correct (the reseed script's exclusion existed only to avoid a
# destructive wipe, which does not apply to an advisory round).
#
# Wrapper contract: BASE = last-round sha (persisted by the wrapper); HEAD =
# currently-deployed staging sha (read by the wrapper). This script is a pure
# decision — it never checks out, mutates, reads/re-reads deployed state, or
# advances any watermark; the wrapper owns all of that.

set -uo pipefail   # NO -e: a failed diff/source/validation must degrade, not abort.

# Sanitize the repo's standard git-location env isolation set so EVERY git call below
# (the sourced pre-push's rev-parse, plus cat-file/diff) resolves the CWD checkout,
# not some other repo a caller's env points at. GIT_DIR / GIT_COMMON_DIR /
# GIT_OBJECT_DIRECTORY redirect object/ref resolution, and GIT_WORK_TREE with a
# nonexistent-parent path fatals cat-file/diff — all four are load-bearing.
# GIT_INDEX_FILE is inert for a commit-vs-commit diff but is unset as part of the
# standard isolation set. The isolation is contract-tested (a fake git that fails on
# any leaked var).
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)"

emit() {
  # emit <key> <value>: stdout for humans/tests + $GITHUB_ENV for the workflow.
  printf '%s=%s\n' "$1" "$2"
  if [ -n "${GITHUB_ENV:-}" ]; then
    # In CI the wrapper reads the decision from $GITHUB_ENV, NOT stdout. A failed
    # append means the decision did not propagate, so fail VISIBLY (non-zero exit +
    # stderr) rather than exit 0 having emitted only to stdout — a silent success
    # on a lost decision is exactly the failure mode this gate must avoid.
    if ! printf '%s=%s\n' "$1" "$2" >> "$GITHUB_ENV"; then
      printf 'qa-round-cadence-gate: failed to write %s to GITHUB_ENV (%s)\n' "$1" "$GITHUB_ENV" >&2
      exit 3
    fi
  fi
}

# Uncertain skip: we could NOT confidently decide, so DON'T run (this advisory gate
# fails toward not running — conservative on judge-token spend, with the floor + a
# manual trigger as the backstop). Every uncertainty / internal-failure path
# (unresolvable BASE/HEAD, unsourceable or absent file_in_group, unreadable filters,
# git/temp/read failure, unusable DAYS_SINCE) ends here. base_known=false marks
# "decided under uncertainty".
skip_uncertain() {
  emit run_round false
  emit judge_relevant_change unknown
  emit base_known false
  emit changed_groups ""
  exit 0
}

# The QA round is advisory / eventual-convergence: a missed round is fine because
# staleness is BOUNDED by this floor and a manual trigger is the backstop. The
# path-filter match below is the best-effort FAST path (a judge-relevant change runs
# immediately); the floor ensures any staging change is judged within
# MAX_STALENESS_DAYS at the latest even if the match misses it. So the filter match
# does NOT need to be an exhaustively correct negative-prover, and a damaged/unusable
# filter simply skips (uncertainty -> skip_uncertain) rather than forcing a run.
MAX_STALENESS_DAYS=7

BASE="${1:-}"
HEAD="${2:-}"
# DAYS_SINCE = whole days since the last judge round (wrapper-supplied). Only
# consulted when staging changed but no judge-relevant delta was found; validated
# lazily there so a HEAD==BASE skip / judge-relevant run never depends on it.
DAYS_SINCE="${3:-}"

# Need both endpoints and a resolvable repo root.
[ -n "$BASE" ] || skip_uncertain
[ -n "$HEAD" ] || skip_uncertain
[ -n "$REPO_ROOT" ] || skip_uncertain

# No staging change since the last round -> SKIP, and skip indefinitely regardless
# of the staleness floor: nothing changed, so there is nothing to judge (don't burn
# judge tokens). This takes priority over the floor for a VALID equal pair. An
# INVALID equal pair (two identical unresolvable refs) is an uncertainty, not a
# confirmed "unchanged", so it skips_uncertain like any other unresolvable endpoint.
if [ "$BASE" = "$HEAD" ]; then
  git cat-file -e "${BASE}^{commit}" 2>/dev/null || skip_uncertain
  emit run_round false
  emit judge_relevant_change false
  emit base_known true
  emit changed_groups ""
  exit 0
fi

# file_in_group lives in the pre-push hook; the source-guard keeps the hook body
# from running on source. Source by absolute path so this is CWD-independent, then
# re-point FILTERS_FILE at the real path-filters.yml (the matcher reads that global).
# shellcheck source=/dev/null
source "$REPO_ROOT/scripts/hooks/pre-push" 2>/dev/null || skip_uncertain
declare -f file_in_group >/dev/null 2>&1 || skip_uncertain
FILTERS_FILE="$REPO_ROOT/path-filters.yml"
{ [ -f "$FILTERS_FILE" ] && [ -r "$FILTERS_FILE" ]; } || skip_uncertain

# Both endpoints must be real commits in the CWD repo (force-push / dropped
# endpoint -> not resolvable -> skip on uncertainty).
git cat-file -e "${BASE}^{commit}" 2>/dev/null || skip_uncertain
git cat-file -e "${HEAD}^{commit}" 2>/dev/null || skip_uncertain

# TWO-DOT diff: last-round tree (BASE) vs deployed tree (HEAD).
# --no-renames: rename detection collapses a move to only its DESTINATION path, so
# a judge-relevant source (backend/x.go -> docs/x.go) would be hidden behind an
# irrelevant destination and silently skip the round. Disabling it surfaces both
# the deleted source and the added destination.
# -z: without it, git QUOTES paths containing tabs/newlines/quotes/backslashes (a
# tab in `backend/a<TAB>b.go` becomes `"backend/a\tb.go"`), which then fails the
# matcher's `^backend/` anchor -> the change would be missed. -z emits raw
# NUL-delimited paths, keeping the FAST path correct. A shell variable can't hold
# NUL, so the list is buffered through a temp file whose creation AND the git write
# are BOTH status-checked -> any failure (unwritable/full temp, git error) is an
# uncertainty -> skip. The template is TMPDIR-derived so a broken temp dir fails
# mktemp instead of silently falling back.
diff_tmp="$(mktemp "${TMPDIR:-/tmp}/qa-cadence-gate.XXXXXX" 2>/dev/null)" || skip_uncertain
if ! git diff -z --no-renames --name-only "$BASE" "$HEAD" > "$diff_tmp" 2>/dev/null; then
  rm -f "$diff_tmp"
  skip_uncertain
fi

matched_backend=false
matched_frontend=false
matched_seed=false
# Open the buffered list on fd 3 with a status check: a read-side open failure
# (temp removed / unreadable) is an uncertainty, not "no changes" -> skip.
exec 3< "$diff_tmp" || { rm -f "$diff_tmp"; skip_uncertain; }
# read -r -d '' consumes one NUL-delimited path per iteration verbatim (no word
# splitting / globbing), so paths with spaces or other special characters stay intact.
while IFS= read -r -d '' f <&3; do
  [ -n "$f" ] || continue
  file_in_group "$f" backend  && matched_backend=true
  file_in_group "$f" frontend && matched_frontend=true
  file_in_group "$f" seed     && matched_seed=true
done
exec 3<&-
rm -f "$diff_tmp"

# A matcher failure mid-loop (path-filters.yml deleted/unreadable/replaced WHILE
# matching) is indistinguishable from a clean non-match inside file_in_group — all
# groups would stay false and reach a false run_round. Re-check AFTER the loop that
# the filter is still a readable REGULAR file (the same -f && -r as the pre-loop
# check): a bare -r would pass for a readable DIRECTORY swapped in mid-match, which
# file_in_group can't read as a filter. If the check fails, the all-false result is
# not trustworthy -> skip.
{ [ -f "$FILTERS_FILE" ] && [ -r "$FILTERS_FILE" ]; } || skip_uncertain

# Build the sorted CSV of matched groups (deterministic: fixed alphabetical order).
groups=""
[ "$matched_backend" = true ]  && groups="${groups:+$groups,}backend"
[ "$matched_frontend" = true ] && groups="${groups:+$groups,}frontend"
[ "$matched_seed" = true ]     && groups="${groups:+$groups,}seed"

# Decision (staging HAS changed since the last round — HEAD==BASE was handled above):
#   judge-relevant delta      -> RUN (fast path).
#   else DAYS_SINCE >= floor   -> RUN (staleness backstop).
#   else                       -> SKIP.
# judge_relevant_change stays the honest "did the groups change" signal, so a floor
# run reads run_round=true + judge_relevant_change=false + empty changed_groups.
if [ -n "$groups" ]; then
  emit run_round true
  emit judge_relevant_change true
  emit base_known true
  emit changed_groups "$groups"
  exit 0
fi

# Changed but not judge-relevant: the staleness floor decides. An unusable
# DAYS_SINCE is an uncertainty -> skip. The length cap (<=15 digits) keeps the value
# well within int64 so the `-ge` comparison below can never overflow (a 19+-digit
# value errors "number truncated" and would otherwise fall through as a clean skip).
[[ "$DAYS_SINCE" =~ ^[0-9]{1,15}$ ]] || skip_uncertain
if [ "$DAYS_SINCE" -ge "$MAX_STALENESS_DAYS" ]; then
  emit run_round true          # staleness floor forces a run
else
  emit run_round false         # within the floor -> skip
fi
emit judge_relevant_change false
emit base_known true
emit changed_groups ""
exit 0
