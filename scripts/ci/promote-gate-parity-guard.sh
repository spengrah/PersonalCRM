#!/bin/bash
# Asserts that the LOCAL promote pre-flight and the REMOTE prod deploy gate agree
# on what "green" means.
#
# deploy-prod.yml runs on the Pi with no checkout and no `gh`, so it cannot call
# scripts/promote-preflight.sh — the two necessarily reimplement the same queries.
# Two divergent definitions of green is the worst outcome (promote says go, deploy
# says no, and main has already moved), so this guard fails if one side changes
# without the other.
set -euo pipefail

WF=".github/workflows/deploy-prod.yml"
PF="scripts/promote-preflight.sh"

for f in "$WF" "$PF"; do
  [ -f "$f" ] || { echo "FAIL: missing $f"; exit 1; }
done

# Both gated workflows must appear on both sides. If deploy-prod grows a third
# gate, the pre-flight must grow it too or it will let a doomed promote through.
for wf in ci.yml build-images.yml; do
  grep -qF "workflows/${wf}/runs" "$WF" \
    || { echo "FAIL: $WF no longer queries ${wf} — update $PF and this guard"; exit 1; }
  grep -qF "${wf}" "$PF" \
    || { echo "FAIL: $PF does not gate on ${wf}, but $WF does"; exit 1; }
done

# status=completed is the filter that stops an in-progress run reading as success.
# Losing it on either side turns the gate into a coin flip.
for f in "$WF" "$PF"; do
  grep -qF 'status=completed' "$f" \
    || { echo "FAIL: $f dropped the status=completed filter (in-progress runs could read as success)"; exit 1; }
done

# Both sides must compare against a run CONCLUSION and require exactly "success",
# with "missing" as the empty-result fallback.
for f in "$WF" "$PF"; do
  grep -qF '.workflow_runs[0].conclusion // "missing"' "$f" \
    || { echo "FAIL: $f no longer reads .workflow_runs[0].conclusion with a \"missing\" fallback"; exit 1; }
  grep -qF '!= "success"' "$f" \
    || { echo "FAIL: $f no longer requires the conclusion to be exactly \"success\""; exit 1; }
done

echo "OK: promote gate parity"
