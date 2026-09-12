#!/usr/bin/env bash
# Run the real Makefile -> selector -> Makefile chain with stubbed DB/test actions.
set -euo pipefail
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE E2E_PRINT_ONLY
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/scripts/hooks/lib" "$tmp/frontend/tests/e2e"
cp "$repo/Makefile" "$tmp/Makefile"
cp "$repo/scripts/run-e2e-local.mjs" "$tmp/scripts/"
cp "$repo/scripts/hooks/test-map-coverage-check.sh" "$tmp/scripts/hooks/"
cp "$repo/scripts/hooks/lib/test-map-coverage.mjs" "$tmp/scripts/hooks/lib/"
cat >> "$tmp/Makefile" <<'MAKE'

e2e-ports-free:
	@:
e2e-db:
	@echo reset >> "$(E2E_TEST_LOG)"
	@exit "$${E2E_SETUP_EXIT:-0}"
test-e2e-local:
	@printf 'run %s\n' "$$PLAYWRIGHT_GREP" >> "$(E2E_TEST_LOG)"
MAKE
cat > "$tmp/frontend/tests/e2e/test-map.json" <<'JSON'
[{"pattern":"^frontend/tests/e2e/","tags":["@area:contacts"]}]
JSON
printf '// baseline\n' > "$tmp/frontend/tests/e2e/sample.spec.ts"
git -C "$tmp" init -q
git -C "$tmp" config user.name Test
git -C "$tmp" config user.email test@example.invalid
git -C "$tmp" config commit.gpgsign false
git -C "$tmp" config core.hooksPath /dev/null
git -C "$tmp" add .
git -C "$tmp" commit -qm baseline
export E2E_BASE_REF
E2E_BASE_REF=$(git -C "$tmp" rev-parse HEAD)
export E2E_TEST_LOG="$tmp/actions.log"
printf '// changed\n' >> "$tmp/frontend/tests/e2e/sample.spec.ts"

run_target() { (cd "$tmp" && make --no-print-directory test-e2e-diff) > "$tmp/output" 2>&1; }
fail() { cat "$tmp/output" >&2; echo "FAIL: $1" >&2; exit 1; }
run_target || fail "diff-selected command failed"
[[ "$(grep -c '^reset$' "$E2E_TEST_LOG")" == 1 ]] || fail "database setup did not run exactly once"
grep -q '^run .*@area:contacts' "$E2E_TEST_LOG" || fail "selected tag not passed to test runner"
[[ "$(wc -l < "$E2E_TEST_LOG" | tr -d ' ')" == 2 ]] || fail "unexpected extra setup or test execution"

: > "$E2E_TEST_LOG"
export E2E_SETUP_EXIT=1
if run_target; then fail "setup failure accepted"; fi
[[ "$(cat "$E2E_TEST_LOG")" == reset ]] || fail "tests ran after setup failure"
unset E2E_SETUP_EXIT

: > "$E2E_TEST_LOG"
printf 'invalid json\n' > "$tmp/frontend/tests/e2e/test-map.json"
if run_target; then fail "invalid mapping accepted"; fi
[[ ! -s "$E2E_TEST_LOG" ]] || fail "database reset before mapping validation"
echo "ALL PASS (single E2E setup, validation before setup)"
