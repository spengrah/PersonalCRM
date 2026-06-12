#!/usr/bin/env bash
# test-map-coverage-check.sh — guards E2E test-map spec self-coverage in the
# pre-push LINT phase (which runs unconditionally, NOT gated by should_skip_tests
# — so the gate fires even on a frontend-test-only push, which is exactly when a
# spec self-entry can drift). Converts the hand-patched core.md gotcha ("Adding
# settings hooks without test-map entry") into a mechanical gate.
#
# Invariant: every tracked frontend/tests/e2e/*.spec.ts (top-level, excluding
# helpers/) is matched by at least one `pattern` in test-map.json. A spec with no
# matching pattern means editing that test never selects it in test-e2e-diff — a
# uniquely bad, silent failure mode this guard makes loud.
#
# The actual regex matching lives in scripts/hooks/lib/test-map-coverage.mjs and
# uses JS `new RegExp(pattern).test(file)` — the same construction run-e2e-local.mjs
# uses — so the guard's answer is byte-identical to the diff-selector's. The module
# distinguishes exit 0 (all matched), 1 (offenders on stdout), 2 (internal error).
#
# Fail-closed: this wrapper captures the module's exit code explicitly and treats
# ANY non-zero rc as push-blocking. It never infers pass/fail from whether stdout
# was empty (an empty stdout under rc=2 is an error, not a pass) — so a malformed
# test-map.json, an invalid regex, a missing spec list, or a missing `node` all
# block the push instead of silently passing.
#
# Sourceable: when sourced (BASH_SOURCE != $0) it only defines functions, so the
# logic can be unit-tested with injected input (see test/test-test-map-coverage-check.sh).
set -uo pipefail

run_test_map_coverage() {
  # Optional $1 overrides the map path. Production callers pass nothing (the guard
  # body and the pre-push command both invoke it with no args); only the self-test
  # passes an injected map so it can drive this real wrapper (incl. its
  # command-substitution rc-capture below) against a malformed map. Using a function
  # parameter rather than an env var avoids an exported-in-a-dev-shell footgun.
  local repo_root map specs offenders rc
  repo_root="$(git rev-parse --show-toplevel)"
  map="${1:-$repo_root/frontend/tests/e2e/test-map.json}"

  # Tracked top-level specs only. An untracked new spec is in-progress work and
  # shouldn't block; once committed it's caught. The glob is non-recursive so
  # helpers/** is excluded.
  specs="$(git -C "$repo_root" ls-files 'frontend/tests/e2e/*.spec.ts')"
  if [ -z "$specs" ]; then
    # The repo always has specs — an empty list means a broken invocation. Fail closed.
    echo "test-map coverage: no e2e spec files found via git ls-files — refusing to pass" >&2
    return 1
  fi

  # Capture stdout (offenders) and the explicit exit code. Word-splitting $specs
  # into argv is intentional (paths have no spaces).
  # shellcheck disable=SC2086
  offenders="$(node "$repo_root/scripts/hooks/lib/test-map-coverage.mjs" "$map" $specs)"
  rc=$?

  case "$rc" in
    0)
      return 0
      ;;
    1)
      echo "e2e test-map: spec file(s) not matched by any test-map.json pattern — editing them won't select them in test-e2e-diff. Add a self-entry:" >&2
      echo "$offenders" | sed 's/^/  - /' >&2
      return 1
      ;;
    *)
      # rc=2 (internal error) or any other non-{0,1} value — the module already
      # printed the cause to stderr. Fail closed; never treat an error as a pass.
      echo "e2e test-map: coverage check failed to run (see error above)" >&2
      return 1
      ;;
  esac
}

# Source-guard: run the gate only when executed directly, not when sourced by tests.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  run_test_map_coverage
fi
