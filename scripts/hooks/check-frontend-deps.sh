#!/usr/bin/env bash
# check-frontend-deps.sh — stateless preflight that fails with a CLEAR message
# when frontend/node_modules is missing or incomplete, instead of letting the
# pre-push frontend commands fail later with a cryptic `next: command not found`
# (exit 127).
#
# Why this exists: a fresh linked worktree's deps are installed at birth by
# scripts/install-worktree-deps.sh (via the post-checkout hook), but that
# birth-time signal is ephemeral — a programmatic worktree creator (Orca) may
# discard its stderr, and the hook deliberately swallows the installer's exit
# code so the checkout always succeeds. So a worktree whose install failed,
# never ran, or was manually removed would still hit the original exit-127 at
# pre-push. This stateless check is the durable backstop: no marker file, so it
# also catches those cases and self-clears the moment deps are installed.
#
# It runs as a fast-fail GATE from scripts/hooks/pre-push (check_frontend_deps),
# BEFORE the parallel LINT/FRONTEND phases, so its message REPLACES the cryptic
# errors rather than printing alongside them (the LINT phase does not
# short-circuit and runs concurrently with FRONTEND).
#
# Sourceable: when sourced (BASH_SOURCE != $0) it only defines functions, so the
# pure logic can be unit-tested with an injected root (see
# scripts/test/test-install-worktree-deps.sh). When executed it runs the check
# and exits with its status.
set -uo pipefail

# The exact binaries the pre-push frontend commands invoke:
#   "Frontend lint"  -> next lint && prettier --check .
#   "Frontend tests" -> vitest run
# Keep this list in sync with those commands (.ai/pre-push.json +
# frontend/package.json): if the frontend ever drops or renames one of these
# tools, the matching command changes in the same edit, so update this list
# then too. A test asserts the 3-bin check via a partial-install fixture.
FRONTEND_REQUIRED_BINS=(next prettier vitest)

# frontend_deps_ok <repo_root> — pure predicate. Returns 0 when there is nothing
# to check (no frontend/package.json) OR every required .bin binary is present
# and executable; returns 1 when frontend/package.json exists but any required
# binary is missing (catches a missing OR partial/empty install). Prints
# nothing — the caller owns the user-facing message. Unit-testable with an
# injected root (no git repo needed).
frontend_deps_ok() {
  local root="$1" bin
  [ -f "$root/frontend/package.json" ] || return 0
  for bin in "${FRONTEND_REQUIRED_BINS[@]}"; do
    [ -x "$root/frontend/node_modules/.bin/$bin" ] || return 1
  done
  return 0
}

# check_frontend_deps_main — entry point. Resolve the repo root, run the pure
# check, and on failure print the actionable, checkout-agnostic message + return
# non-zero. The message leads with `cd frontend && bun install` because that
# works in EVERY checkout (the gate also runs when pushing from the main
# checkout, where `make worktree-deps` deliberately no-ops).
check_frontend_deps_main() {
  local root
  root="$(git rev-parse --show-toplevel 2>/dev/null)" || return 0  # not a repo -> skip
  if frontend_deps_ok "$root"; then
    return 0
  fi
  echo "frontend dependencies are not installed (frontend/node_modules is missing or incomplete). Install them: 'cd frontend && bun install' (or 'make worktree-deps' in a linked worktree, 'make setup' in the main checkout). This usually means a worktree was created without provisioning." >&2
  return 1
}

# Source-guard: run the check only when executed directly, not when sourced.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  check_frontend_deps_main
fi
