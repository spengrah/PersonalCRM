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
for g in backend frontend mac_daemon seed; do
  grep -qE "^${g}:" path-filters.yml || { echo "FAIL: path-filters.yml missing group $g"; exit 1; }
done
echo "OK: path-filter parity"
