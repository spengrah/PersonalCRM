#!/usr/bin/env bash
# Regression test for the qa-fn-backfill Makefile target's ROUND+TRACE mutual-
# exclusion guard. The CLI's own strict-arity tests drive main() directly and never
# exercise the Makefile recipe, so without this a dropped guard line would silently
# re-select TRACE (enqueue) while ROUND is also set, with every TS test still green.
#
# Invokes the REAL make target with BOTH set and proves the guard SHORT-CIRCUITS
# before the bun command: a `bun` stub is placed first on PATH that records a sentinel
# and exits non-zero. A guard that kept its message but lost `exit 2` would fall
# through to bun (sentinel present) yet still show a non-zero exit + the message — so
# asserting the sentinel is ABSENT is what actually proves the short-circuit.
#
# Portable: no network, no BSD-only flags — this suite runs on Ubuntu CI.

set -uo pipefail

# Sanitize hook-inherited git env (a no-op outside the pre-push hook); this test runs
# `make`, not git, but keep the suite's convention so nothing leaks into the real repo.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok() { PASS=$((PASS + 1)); }

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR"' EXIT
SENTINEL="$STUB_DIR/bun-was-invoked"
# A bun stub that would fire IF the guard fell through to the recipe's bun line.
cat > "$STUB_DIR/bun" <<STUB
#!/usr/bin/env bash
touch "$SENTINEL"
exit 1
STUB
chmod +x "$STUB_DIR/bun"

# ROUND + TRACE together must error (non-zero) with the mutual-exclusion message,
# BEFORE the bun command runs (bun stub never invoked → sentinel absent).
out="$(PATH="$STUB_DIR:$PATH" make -C "$REPO_ROOT" qa-fn-backfill \
  BEHAVIOR=CON-042 ROUND=abc1234 TRACE=t1 2>&1)"
code=$?

if [ "$code" -eq 0 ]; then
  fail "ROUND+TRACE together should error, but make exited 0"
elif ! printf '%s' "$out" | grep -q "mutually exclusive"; then
  fail "expected a 'mutually exclusive' error message; got: $out"
elif [ -e "$SENTINEL" ]; then
  fail "the guard did NOT short-circuit — bun was reached despite ROUND+TRACE"
else
  ok
fi

echo "qa-fn-backfill-guard: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
