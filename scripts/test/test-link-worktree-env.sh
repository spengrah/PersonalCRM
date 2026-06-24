#!/usr/bin/env bash
# Tests for scripts/link-worktree-env.sh + scripts/hooks/post-checkout.
#
# DB-FREE + PORT-FREE + NETWORK-FREE: pure filesystem + local `git` only (temp
# repos under mktemp). Safe for the pre-push FILTER lane; runs on any CI runner.
#
# Invoked from scripts/hooks/test/test-pre-push-filters.sh (like the per-worktree
# test-pg resolver unit), not as a top-level pre-push command.
#
# Three layers:
#   1. link_env_file               — pure fs link/skip/idempotency safety
#   2. enumerate_env_files         — gitignored-env discovery in a temp repo
#   3. worktree_env_should_provision + a real `git worktree add` end-to-end
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1   # repo root
REPO="$PWD"

source scripts/link-worktree-env.sh   # source-guard prevents the gate body
source scripts/hooks/post-checkout     # source-guard prevents the hook body

fail=0
ok()  { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
assert_true()  { local d="$1"; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
assert_false() { local d="$1"; shift; if "$@"; then bad "$d"; else ok "$d"; fi; }

# --- portable symlink-target read (no GNU-only `readlink -f`) ----------------
link_target() { readlink "$1"; }

TMPDIRS=()
mk_tmp() { local d; d=$(mktemp -d); TMPDIRS+=("$d"); echo "$d"; }
cleanup() { local d; for d in "${TMPDIRS[@]:-}"; do [ -n "$d" ] && rm -rf "$d"; done; }
trap cleanup EXIT

# setup_hooked_main <dir> — build a temp "main" checkout with the REAL scripts,
# the canonical relative core.hooksPath, an env-file .gitignore, a tracked
# .env.example, and one commit. Caller creates any actual env files afterward.
setup_hooked_main() {
  local m="$1"
  mkdir -p "$m/scripts/hooks" "$m/frontend"
  cp "$REPO/scripts/link-worktree-env.sh" "$m/scripts/link-worktree-env.sh"
  cp "$REPO/scripts/hooks/post-checkout"  "$m/scripts/hooks/post-checkout"
  chmod +x "$m/scripts/link-worktree-env.sh" "$m/scripts/hooks/post-checkout"
  printf '.env\nfrontend/.env.local\n' > "$m/.gitignore"
  printf 'EXAMPLE=1\n' > "$m/.env.example"          # tracked, must NOT be linked
  git -C "$m" init -q -b main
  git -C "$m" config user.email test@example.com
  git -C "$m" config user.name "Test"
  git -C "$m" config commit.gpgsign false
  git -C "$m" config core.hooksPath scripts/hooks   # the canonical relative method
  git -C "$m" add -A
  git -C "$m" -c commit.gpgsign=false commit -q -m init
}

# ============================================================================
# 1. link_env_file — pure filesystem behavior
# ============================================================================
echo "--- link_env_file ---"
d=$(mk_tmp); src="$d/main"; dst="$d/wt"
mkdir -p "$src" "$dst"
printf 'ROOT=1\n' > "$src/.env"

# Happy path: creates a symlink pointing at the source.
if link_env_file "$src" "$dst" ".env" && [ -L "$dst/.env" ] \
   && [ "$(link_target "$dst/.env")" = "$src/.env" ]; then
  ok "links a flat env file to the source"
else
  bad "links a flat env file to the source"
fi

# Idempotent: re-running refreshes the symlink, still a symlink, exit 0.
if link_env_file "$src" "$dst" ".env" && [ -L "$dst/.env" ]; then
  ok "re-link is idempotent (stays a symlink)"
else
  bad "re-link is idempotent (stays a symlink)"
fi

# Nested path: creates the parent dir and links.
mkdir -p "$src/frontend"; printf 'FE=1\n' > "$src/frontend/.env.local"
if link_env_file "$src" "$dst" "frontend/.env.local" \
   && [ -L "$dst/frontend/.env.local" ]; then
  ok "creates parent dir and links a nested env file"
else
  bad "creates parent dir and links a nested env file"
fi

# Missing source: no link, non-zero return.
if link_env_file "$src" "$dst" ".env.nope"; then
  bad "missing source should skip (non-zero)"
elif [ -e "$dst/.env.nope" ]; then
  bad "missing source must not create a destination"
else
  ok "missing source skips without creating anything"
fi

# Real file at destination: never clobbered; non-zero return. The source must
# EXIST so we get past the missing-source check and actually reach the
# clobber-protection guard (the case under test).
printf 'PRESERVE=1\n' > "$dst/.env.real"
printf 'SOURCE=1\n'   > "$src/.env.real"
if link_env_file "$src" "$dst" ".env.real" 2>/dev/null; then
  bad "real dest file should be skipped (non-zero)"
else
  if [ -f "$dst/.env.real" ] && [ ! -L "$dst/.env.real" ] \
     && [ "$(cat "$dst/.env.real")" = "PRESERVE=1" ]; then
    ok "never clobbers a real file at the destination"
  else
    bad "never clobbers a real file at the destination"
  fi
fi

# ============================================================================
# 2. enumerate_env_files — gitignored-env discovery
# ============================================================================
echo "--- enumerate_env_files ---"
repo=$(mk_tmp)
git -C "$repo" init -q -b main
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name "Test"
printf '.env\n.env.local\nfrontend/.env.local\n' > "$repo/.gitignore"
mkdir -p "$repo/frontend"
printf 'EXAMPLE=1\n' > "$repo/.env.example"          # tracked template
printf 'FE_EXAMPLE=1\n' > "$repo/frontend/.env.example"
git -C "$repo" add -A
git -C "$repo" -c commit.gpgsign=false commit -q -m init
# gitignored env files, present on disk:
printf 'ROOT=1\n' > "$repo/.env"
printf 'LOCAL=1\n' > "$repo/.env.local"
printf 'FE=1\n' > "$repo/frontend/.env.local"

got="$(enumerate_env_files "$repo" | sort | tr '\n' ',')"
want=".env,.env.local,frontend/.env.local,"
if [ "$got" = "$want" ]; then
  ok "lists exactly the gitignored env files (excludes tracked .env.example)"
else
  bad "enumerate_env_files: got '$got' want '$want'"
fi

# run_worktree_env must be a no-op in the MAIN checkout — never symlink over /
# clobber the main checkout's own real env files. ($repo is a main checkout.)
( cd "$repo" && run_worktree_env ) >/dev/null 2>&1
if [ -f "$repo/.env" ] && [ ! -L "$repo/.env" ] \
   && [ ! -L "$repo/.env.local" ] && [ ! -L "$repo/frontend/.env.local" ]; then
  ok "run_worktree_env is a no-op in the main checkout (real files untouched)"
else
  bad "run_worktree_env must not touch files in the main checkout"
fi

# ============================================================================
# 3. worktree_env_should_provision — gate predicate
# ============================================================================
echo "--- worktree_env_should_provision (gate) ---"
zero40="0000000000000000000000000000000000000000"
zero64="0000000000000000000000000000000000000000000000000000000000000000"
assert_true  "fresh worktree (SHA-1 zero, branch) provisions"   worktree_env_should_provision "$zero40" "1"
assert_true  "fresh worktree (SHA-256 zero, branch) provisions" worktree_env_should_provision "$zero64" "1"
assert_false "ordinary branch switch does not provision"        worktree_env_should_provision "abc1234" "1"
assert_false "file checkout (flag 0) does not provision"        worktree_env_should_provision "$zero40" "0"
assert_false "empty args do not provision"                      worktree_env_should_provision "" ""

# ============================================================================
# 4. End-to-end: a real `git worktree add` fires the hook and links env files
# ============================================================================
echo "--- end-to-end: git worktree add ---"
e2e=$(mk_tmp); main="$e2e/main"
setup_hooked_main "$main"
# gitignored env files in the main checkout, present on disk:
printf 'ROOT=1\n' > "$main/.env"
printf 'FE=1\n' > "$main/frontend/.env.local"

wt="$e2e/wt"
git -C "$main" worktree add -q "$wt" -b feature HEAD 2>/dev/null

# -ef compares by inode (following symlinks), robust to macOS /private path
# normalization in the hook's pwd-resolved main_root.
if [ -L "$wt/.env" ] && [ "$wt/.env" -ef "$main/.env" ]; then
  ok "worktree add linked .env to the main checkout"
else
  bad "worktree add linked .env to the main checkout"
fi
if [ -L "$wt/frontend/.env.local" ] && [ "$wt/frontend/.env.local" -ef "$main/frontend/.env.local" ]; then
  ok "worktree add linked frontend/.env.local"
else
  bad "worktree add linked frontend/.env.local"
fi
# Tracked template is checked out normally — present, but NOT a symlink.
if [ -e "$wt/.env.example" ] && [ ! -L "$wt/.env.example" ]; then
  ok "tracked .env.example is a normal file, not linked"
else
  bad "tracked .env.example is a normal file, not linked"
fi
# The main checkout's real env file is untouched.
if [ -f "$main/.env" ] && [ ! -L "$main/.env" ]; then
  ok "main checkout's real .env is left untouched"
else
  bad "main checkout's real .env is left untouched"
fi

# Zero env files in main: the hook still runs cleanly and links nothing.
empty_main="$e2e/empty-main"
setup_hooked_main "$empty_main"        # no .env / frontend/.env.local on disk
empty_wt="$e2e/empty-wt"
if git -C "$empty_main" worktree add -q "$empty_wt" -b feature HEAD 2>/dev/null \
   && [ ! -e "$empty_wt/.env" ] && [ ! -e "$empty_wt/frontend/.env.local" ]; then
  ok "worktree add with zero env files in main: succeeds, links nothing"
else
  bad "worktree add with zero env files in main: should succeed and link nothing"
fi

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
