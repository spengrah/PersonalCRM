#!/bin/bash
# Asserts that CI's `changes` job reads the shared path-filters.yml (not an
# inline filters block) and that the expected groups exist in the shared file.
# Runs in the always-running `changes` job so a broken filter fails loudly.
set -euo pipefail

# The changes job must read the shared filter file, not an inline block.
grep -q 'filters: ./path-filters.yml' .github/workflows/ci.yml \
  || { echo "FAIL: ci.yml changes job must use filters: ./path-filters.yml"; exit 1; }
grep -qE '^\s*filters:\s*\|' .github/workflows/ci.yml \
  && { echo "FAIL: ci.yml reintroduced an inline filters block"; exit 1; } || true
for g in backend frontend mac_daemon seed migrations spec; do
  grep -qE "^${g}:" path-filters.yml || { echo "FAIL: path-filters.yml missing group $g"; exit 1; }
done
# The spec group is CI-consumed (unlike seed/migrations): the changes job must
# EXPOSE outputs.spec, and some job must still GATE on it — otherwise spec-only
# PRs silently skip the corpus lint. grep -qF (fixed string) dodges regex-escaping
# the ${{ }} braces; a stale comment could in principle satisfy these greps, which
# matches the existing checks' own style and is accepted to keep the guard LCD.
grep -qF 'spec: ${{ steps.filter.outputs.spec }}' .github/workflows/ci.yml \
  || { echo "FAIL: ci.yml changes job must expose outputs.spec (spec-lint job gates on it)"; exit 1; }
grep -qF "needs.changes.outputs.spec == 'true'" .github/workflows/ci.yml \
  || { echo "FAIL: ci.yml must have a job gated on changes.outputs.spec (spec-lint)"; exit 1; }
echo "OK: path-filter parity"
