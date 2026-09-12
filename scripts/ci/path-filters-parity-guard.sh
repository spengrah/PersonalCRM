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
for g in backend frontend mac_daemon scripts seed migrations spec; do
  grep -qE "^${g}:" path-filters.yml || { echo "FAIL: path-filters.yml missing group $g"; exit 1; }
done
# The spec and scripts groups each gate their own CI job (unlike seed/migrations):
# the changes job must EXPOSE the output, and some job must still GATE on it —
# otherwise a spec-only or scripts-only PR silently skips its suite. grep -qF
# (fixed string) dodges regex-escaping the ${{ }} braces; a stale comment could
# in principle satisfy these greps, which matches the existing checks' own style
# and is accepted to keep the guard LCD.
for g in spec scripts; do
  grep -qF "${g}: \${{ steps.filter.outputs.${g} }}" .github/workflows/ci.yml \
    || { echo "FAIL: ci.yml changes job must expose outputs.${g} (a job gates on it)"; exit 1; }
  grep -qF "needs.changes.outputs.${g} == 'true'" .github/workflows/ci.yml \
    || { echo "FAIL: ci.yml must have a job gated on changes.outputs.${g}"; exit 1; }
done
echo "OK: path-filter parity"
