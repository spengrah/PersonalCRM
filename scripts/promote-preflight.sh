#!/bin/bash
# Pre-flight for `make promote`. Refuses to advance main unless the SHA about to
# be promoted is the fetched develop tip AND both prod gates already record
# success for it.
#
# Why this exists locally at all, when deploy-prod.yml re-checks the same things:
# by the time deploy-prod aborts, main has ALREADY moved (the push succeeded; the
# workflow refuses afterward), leaving main ahead of what is actually deployed.
# Refusing here keeps main from moving in the first place.
#
# MIRROR, not shared code: deploy-prod.yml runs on the Pi with no checkout, so it
# cannot call this script — it reimplements these queries with curl+jq. The two
# copies are kept honest by scripts/ci/promote-gate-parity-guard.sh, which fails
# if either side changes its notion of "green" without the other.
#
# Fails CLOSED: any gh/network/auth failure blocks the promote rather than
# assuming green. Matches deploy-prod.yml's posture (it aborts on "missing").
set -euo pipefail

BRANCH="${PROMOTE_SOURCE_BRANCH:-develop}"
TARGET="${PROMOTE_TARGET_BRANCH:-main}"

command -v gh >/dev/null 2>&1 \
  || { echo "❌ promote pre-flight: gh is not installed (required to read CI status)"; exit 1; }

echo "→ Fetching origin/$BRANCH..."
git fetch --quiet origin "$BRANCH" \
  || { echo "❌ promote pre-flight: git fetch failed"; exit 1; }

local_sha=$(git rev-parse "$BRANCH")
remote_sha=$(git rev-parse "origin/$BRANCH")

# `make promote` pushes the LOCAL ref, so a stale checkout silently promotes an
# older SHA than intended. Refuse rather than retarget: promoting origin/$BRANCH
# automatically would ship commits the operator has never looked at, which is a
# worse failure than stopping.
if [ "$local_sha" != "$remote_sha" ]; then
  echo "❌ promote pre-flight: local $BRANCH does not match origin/$BRANCH"
  echo "   local:  $local_sha"
  echo "   remote: $remote_sha"
  if git merge-base --is-ancestor "$local_sha" "$remote_sha"; then
    echo "   Local is BEHIND by $(git rev-list --count "$local_sha..$remote_sha") commit(s):"
    git log --oneline "$local_sha..$remote_sha" | sed 's/^/     /'
    echo "   Fast-forward first, then re-run: git branch -f $BRANCH origin/$BRANCH"
  else
    echo "   Branches have DIVERGED — reconcile before promoting."
  fi
  exit 1
fi

sha="$local_sha"
echo "→ Promoting $sha ($(git log -1 --format=%s "$sha"))"

# Gate on a workflow RUN's conclusion, not on individual check-runs: a run whose
# jobs are all path-filtered to `skipped` still concludes `success`, which is the
# correct answer for a docs-only commit. Reading check-runs would reject it.
#
# status=completed is load-bearing (same as deploy-prod.yml): it excludes any
# still-running run so in-progress can never read as success. If CI has not
# finished for this SHA the query returns nothing and we block.
gate_workflow() {
  local wf="$1" label="$2" conclusion
  if ! conclusion=$(gh api \
      "repos/{owner}/{repo}/actions/workflows/${wf}/runs?head_sha=${sha}&status=completed&per_page=1" \
      --jq '.workflow_runs[0].conclusion // "missing"' 2>/dev/null); then
    echo "❌ promote pre-flight: could not read ${label} status for $sha (gh auth/network?)"
    echo "   Blocking rather than assuming green."
    exit 1
  fi
  if [ "$conclusion" != "success" ]; then
    echo "❌ promote pre-flight: ${label} for $sha is '${conclusion}', not success."
    if [ "$conclusion" = "missing" ]; then
      echo "   No COMPLETED ${label} run for this SHA — it may still be running. Wait and retry."
    fi
    exit 1
  fi
  echo "✅ ${label}: success"
}

# Both gates deploy-prod.yml enforces. build-images is the one most likely to
# lag: it runs async on the develop push and takes longer than CI, so a promote
# fired right after a merge can pass CI and still have no :<sha> image.
gate_workflow "ci.yml" "CI"
gate_workflow "build-images.yml" "image build"

echo "✅ promote pre-flight passed — advancing $TARGET to $sha"
