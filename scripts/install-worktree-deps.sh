#!/usr/bin/env bash
# install-worktree-deps.sh — install the per-worktree frontend dependencies
# (frontend/node_modules) into a linked git worktree.
#
# Why deps are INSTALLED, not symlinked (unlike env files): node_modules is
# branch-specific — it is tied to this branch's frontend/bun.lock. Sharing or
# symlinking one node_modules across worktrees breaks the moment two branches'
# lockfiles diverge, and Next.js/watchers misbehave through a symlinked
# node_modules. So env files get symlinked (one source of truth, see
# link-worktree-env.sh) but deps get a real per-worktree install. Go deps need
# nothing here: the global $GOMODCACHE resolves them in any worktree.
#
# Install command: `bun install --frozen-lockfile`. Frozen so the install never
# rewrites the committed frontend/bun.lock (a fresh worktree must not sprout a
# spurious lockfile diff) and so it is reproducible. Genuine package.json /
# bun.lock drift makes a frozen install fail loudly — that is desirable, and
# build-images.yml already installs frozen too. (ci.yml's test jobs use a plain
# `bun install`; the no-lockfile-mutation property matters most at worktree
# birth, so the divergence is deliberate, not an oversight.)
#
# No-op in the main checkout: its deps are owned by `make setup`
# (scripts/setup-dev.sh runs bun install). This script only provisions LINKED
# worktrees.
#
# Sourceable: when sourced (BASH_SOURCE != $0) it only defines functions, so the
# pure logic can be unit-tested with injected input (see
# scripts/test/test-install-worktree-deps.sh). When executed it runs the entry
# point. Deliberately NO `set -e` (matching link-worktree-env.sh): the explicit
# per-branch returns below are the control flow, and `set -e` would abort before
# the loud failure messages print.
set -uo pipefail

# Reuse the canonical worktree path resolvers (resolve_main_root /
# resolve_this_root) from link-worktree-env.sh instead of redefining them — one
# source of truth for "where is the main checkout / this worktree". Sourcing it
# only DEFINES functions (its source-guard suppresses the env-link gate), so
# there are no side effects. The path is computed relative to this script so it
# resolves whether executed by the hook (absolute path) or sourced by the test.
_iwd_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "$_iwd_dir/link-worktree-env.sh"

# worktree_deps_should_install <main_root> <this_root> — pure predicate. Returns
# 0 iff this_root is non-empty AND differs from main_root (i.e. a LINKED
# worktree, not the main checkout). Unit-testable with injected args.
worktree_deps_should_install() {
  local main_root="${1:-}" this_root="${2:-}"
  [ -n "$this_root" ] && [ "$this_root" != "$main_root" ]
}

# run_worktree_deps — side-effecting entry point. Exit codes (Decision 6, the
# distinction is "could it do the job it exists to do?"):
#   0  = succeeded, OR legitimately nothing to do (main checkout, no frontend).
#   !0 = it was supposed to install but could not (bun missing, install failed).
# The hook swallows a non-zero (the worktree is still born, a loud warning is
# shown); `make worktree-deps` surfaces it so recovery never lies about success.
run_worktree_deps() {
  local main_root this_root rc
  main_root="$(resolve_main_root)" || return 0   # not a git repo -> skip quietly
  this_root="$(resolve_this_root)" || return 0
  if ! worktree_deps_should_install "$main_root" "$this_root"; then
    [ -n "${WORKTREE_DEPS_VERBOSE:-}" ] && \
      echo "worktree-deps: this is the main checkout; nothing to install (use 'make setup')" >&2
    return 0   # main-checkout no-op -> success
  fi
  if [ ! -f "$this_root/frontend/package.json" ]; then
    echo "worktree-deps: no frontend/package.json; nothing to install" >&2
    return 0   # legitimately nothing to do -> success
  fi
  if ! command -v bun >/dev/null 2>&1; then
    echo "worktree-deps: bun not found; run 'make setup' to install it, then 'make worktree-deps'" >&2
    return 1   # supposed to install but can't -> failure
  fi
  echo "worktree-deps: installing frontend deps (bun install --frozen-lockfile)..." >&2
  ( cd "$this_root/frontend" && bun install --frozen-lockfile )
  rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "worktree-deps: frontend deps installed" >&2
    return 0
  fi
  echo "worktree-deps: frontend dep install FAILED — run 'cd frontend && bun install' before pushing" >&2
  return "$rc"
}

# Source-guard: run the entry point only when executed directly, not when sourced.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  run_worktree_deps
fi
