#!/usr/bin/env bash
# link-worktree-env.sh — symlink the main checkout's gitignored env files
# (.env, .env.local, frontend/.env.local, ...) into a linked git worktree.
#
# Why: env files are gitignored, so a fresh worktree (Orca workspace, VPS dev
# sandbox, or a plain `git worktree add`) starts WITHOUT them and the dev stack
# can't run. This links them in from the main checkout. It is environment-
# agnostic: the post-checkout git hook calls it on worktree creation wherever a
# worktree is born, and `make worktree-env` calls it on demand.
#
# Symlink (not copy) on purpose: one source of truth in the main checkout, so
# rotating a secret there propagates and no stale copies scatter across
# worktrees. The core script refuses to touch the main checkout's real files.
#
# Sourceable: when sourced (BASH_SOURCE != $0) it only defines functions, so the
# pure logic can be unit-tested with injected input (see
# scripts/test/test-link-worktree-env.sh). When executed it runs the gate.
set -uo pipefail

# resolve_main_root prints the absolute path of the MAIN checkout (the worktree
# whose .git is a real directory). The main root is the parent of the common git
# dir. Prints nothing and returns non-zero outside a git repository.
resolve_main_root() {
  local common
  common="$(git rev-parse --git-common-dir 2>/dev/null)" || return 1
  [ -n "$common" ] || return 1
  # --git-common-dir may be relative to CWD; absolutize it, then take its parent.
  common="$(cd "$common" 2>/dev/null && pwd)" || return 1
  (cd "$common/.." 2>/dev/null && pwd) || return 1
}

# resolve_this_root prints the absolute path of the CURRENT worktree's root.
resolve_this_root() {
  git rev-parse --show-toplevel 2>/dev/null
}

# enumerate_env_files <root> prints the repo-relative paths of gitignored env
# files in <root>. Scoped to the repo root and frontend/ (the only places env
# files live) via glob pathspecs — this keeps the ignored-tree walk fast and,
# crucially, never reaches into node_modules (which is ignored and may itself
# contain stray .env files). Add a pathspec here if env files gain a new home.
enumerate_env_files() {
  local root="$1"
  git -C "$root" ls-files --others --ignored --exclude-standard \
    -- ':(glob).env*' ':(glob)frontend/.env*' 2>/dev/null
}

# link_env_file <src_root> <dest_root> <rel> links <dest_root>/<rel> ->
# <src_root>/<rel>. Returns 0 when a link was created or refreshed, non-zero
# when it skipped (missing source, or a real file already occupies the
# destination — never clobber a real file). Idempotent.
link_env_file() {
  local src_root="$1" dest_root="$2" rel="$3"
  local src="$src_root/$rel" dest="$dest_root/$rel"
  [ -e "$src" ] || return 1
  if [ -L "$dest" ]; then
    ln -sfn "$src" "$dest"   # refresh existing symlink (idempotent)
    return 0
  fi
  if [ -e "$dest" ]; then
    echo "worktree-env: $rel exists and is not a symlink; leaving as-is" >&2
    return 1
  fi
  mkdir -p "$(dirname "$dest")"
  ln -s "$src" "$dest"
}

# run_worktree_env is the side-effecting entry point: resolve the roots, refuse
# to run in the main checkout, then link every gitignored env file from the main
# checkout into this worktree. Set WORKTREE_ENV_VERBOSE=1 for the no-op message.
run_worktree_env() {
  local main_root this_root rel linked=0
  main_root="$(resolve_main_root)" || return 0       # not a git repo -> skip quietly
  this_root="$(resolve_this_root)" || return 0
  if [ "$this_root" = "$main_root" ]; then
    [ -n "${WORKTREE_ENV_VERBOSE:-}" ] && \
      echo "worktree-env: this is the main checkout; nothing to link" >&2
    return 0
  fi
  while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    if link_env_file "$main_root" "$this_root" "$rel"; then
      echo "worktree-env: linked $rel" >&2
      linked=$((linked + 1))
    fi
  done < <(enumerate_env_files "$main_root")
  if [ "$linked" -eq 0 ]; then
    echo "worktree-env: no env files to link from $main_root" >&2
  fi
}

# Source-guard: run the gate only when executed directly, not when sourced.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  run_worktree_env
fi
