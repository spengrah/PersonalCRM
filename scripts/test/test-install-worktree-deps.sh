#!/usr/bin/env bash
# Tests for scripts/install-worktree-deps.sh + scripts/hooks/check-frontend-deps.sh
# + the post-checkout deps wiring + the pre-push check_frontend_deps gate wiring.
#
# DB-FREE + PORT-FREE + NETWORK-FREE: pure filesystem + local `git` only (temp
# repos under mktemp), and a STUBBED `bun` (a PATH shim that records argv/cwd and
# honors $FAKE_BUN_EXIT) — never a real install. Safe for the pre-push FILTER
# lane; runs on any CI runner.
#
# Invoked from scripts/hooks/test/test-pre-push-filters.sh (like the sibling
# env-link unit), not as a top-level pre-push command.
#
# Layers:
#   1. worktree_deps_should_install   — pure gate predicate
#   2. run_worktree_deps              — entry point (success / no-op / no-frontend
#                                       / bun-missing / install-fail), bun stubbed
#   3. end-to-end `git worktree add`  — hook installs deps + env-link not regressed
#                                       + hook never aborts on install failure
#   4. check-frontend-deps.sh         — the 3-bin preflight (present/missing/partial)
#   5. pre-push gate wiring           — gate is called before run_phases_parallel
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1   # repo root
REPO="$PWD"

# Sanitize hook-inherited git env BEFORE any git command. git exports GIT_DIR to
# hooks (absolute in linked worktrees), which silently redirects every
# `git -C "$tmp" ...` below at the REAL repo with work-tree=$tmp: `add -A` wipes
# the real index, `commit` strands fixture commits on the pushed branch, and
# `worktree add` mutates the real repo. Unsetting restores cwd/-C-based discovery
# so the throwaway repos are isolated. (Same guard as test-link-worktree-env.sh.)
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

source scripts/install-worktree-deps.sh    # source-guard prevents the entry point
source scripts/hooks/check-frontend-deps.sh # source-guard prevents the check body

fail=0
ok()  { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
assert_true()  { local d="$1"; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
assert_false() { local d="$1"; shift; if "$@"; then bad "$d"; else ok "$d"; fi; }

TMPDIRS=()
mk_tmp() { local d; d=$(mktemp -d); TMPDIRS+=("$d"); echo "$d"; }
cleanup() { local d; for d in "${TMPDIRS[@]:-}"; do [ -n "$d" ] && rm -rf "$d"; done; }
trap cleanup EXIT

# --- Stub bun: a PATH shim recording argv + cwd, honoring $FAKE_BUN_EXIT. Never
# does any real I/O, so the suite stays network-free. -------------------------
FAKE_BIN=$(mk_tmp)
cat > "$FAKE_BIN/bun" <<'STUB'
#!/usr/bin/env bash
{ printf 'argv=%s\n' "$*"; printf 'cwd=%s\n' "$PWD"; } >> "${FAKE_BUN_LOG:-/dev/null}"
exit "${FAKE_BUN_EXIT:-0}"
STUB
chmod +x "$FAKE_BIN/bun"
BUN_LOG=$(mk_tmp)/bun.log

# Inject the worktree path resolvers so the run_worktree_deps unit cases need no
# real git worktree — they read INJ_MAIN / INJ_THIS. (The end-to-end cases below
# run the REAL script as a subprocess via `git worktree add`, so they exercise
# the real resolvers; this override only affects this shell's unit calls.)
resolve_main_root() { printf '%s\n' "${INJ_MAIN:-}"; }
resolve_this_root() { printf '%s\n' "${INJ_THIS:-}"; }

# deps_unit <main> <this> <path> <fake_exit>: run run_worktree_deps in a subshell
# with injected resolvers + scoped PATH/env. Sets globals RC (exit code) and OUT
# (combined stdout+stderr). Truncates the bun-stub log first.
deps_unit() {
  local main="$1" this="$2" pth="$3" fexit="$4"
  : > "$BUN_LOG"
  OUT=$(
    export INJ_MAIN="$main" INJ_THIS="$this" PATH="$pth" \
           FAKE_BUN_LOG="$BUN_LOG" FAKE_BUN_EXIT="$fexit"
    run_worktree_deps 2>&1
  )
  RC=$?
}
stub_invoked() { [ -s "$BUN_LOG" ]; }

# ============================================================================
# 1. worktree_deps_should_install — pure gate predicate
# ============================================================================
echo "--- worktree_deps_should_install (gate) ---"
assert_true  "linked worktree (this != main) installs"      worktree_deps_should_install "/a/main" "/a/wt"
assert_false "main checkout (this == main) does not install" worktree_deps_should_install "/a/main" "/a/main"
assert_false "empty this_root does not install"             worktree_deps_should_install "/a/main" ""

# ============================================================================
# 2. run_worktree_deps — success path (stub bun, linked worktree)
# ============================================================================
echo "--- run_worktree_deps: success ---"
d=$(mk_tmp); s_main="$d/main"; s_this="$d/wt"
mkdir -p "$s_main" "$s_this/frontend"
printf '{"name":"fe"}\n' > "$s_this/frontend/package.json"
deps_unit "$s_main" "$s_this" "$FAKE_BIN:/usr/bin:/bin" 0
if [ "$RC" -eq 0 ] && grep -q '^argv=install --frozen-lockfile$' "$BUN_LOG"; then
  ok "success: bun invoked with 'install --frozen-lockfile', returns 0"
else
  bad "success: expected rc 0 + 'install --frozen-lockfile' (rc=$RC, log=$(cat "$BUN_LOG"))"
fi
cwd_val=$(grep '^cwd=' "$BUN_LOG" | head -1 | cut -d= -f2-)
if [ -n "$cwd_val" ] && [ "$cwd_val" -ef "$s_this/frontend" ]; then
  ok "success: bun ran with cwd = <worktree>/frontend"
else
  bad "success: bun cwd '$cwd_val' != '$s_this/frontend'"
fi

# ============================================================================
# 3. run_worktree_deps — no-op in the main checkout (this == main)
# ============================================================================
echo "--- run_worktree_deps: main-checkout no-op ---"
d=$(mk_tmp); mkdir -p "$d/frontend"; printf '{"name":"fe"}\n' > "$d/frontend/package.json"
deps_unit "$d" "$d" "$FAKE_BIN:/usr/bin:/bin" 0
if [ "$RC" -eq 0 ] && ! stub_invoked; then
  ok "main checkout: returns 0, bun NOT invoked"
else
  bad "main checkout: expected rc 0 + no bun (rc=$RC, log=$(cat "$BUN_LOG"))"
fi

# ============================================================================
# 4. run_worktree_deps — bun missing while frontend/package.json exists (FAIL)
# ============================================================================
echo "--- run_worktree_deps: bun missing (returns non-zero) ---"
d=$(mk_tmp); bm_main="$d/main"; bm_this="$d/wt"
mkdir -p "$bm_main" "$bm_this/frontend"
printf '{"name":"fe"}\n' > "$bm_this/frontend/package.json"
# Curated PATH WITHOUT the stub (and without the user's real bun dir): only
# /usr/bin:/bin, which provide git/coreutils but no bun.
deps_unit "$bm_main" "$bm_this" "/usr/bin:/bin" 0
if [ "$RC" -ne 0 ] && ! stub_invoked && printf '%s' "$OUT" | grep -q "bun not found"; then
  ok "bun missing: non-zero exit + 'bun not found' message, no install attempted"
else
  bad "bun missing: expected non-zero + message + empty log (rc=$RC, out=$OUT)"
fi

# ============================================================================
# 5. run_worktree_deps — no frontend/package.json (legitimately nothing to do)
# ============================================================================
echo "--- run_worktree_deps: no frontend/package.json ---"
d=$(mk_tmp); np_main="$d/main"; np_this="$d/wt"
mkdir -p "$np_main" "$np_this/frontend"   # frontend dir but NO package.json
deps_unit "$np_main" "$np_this" "$FAKE_BIN:/usr/bin:/bin" 0
if [ "$RC" -eq 0 ] && ! stub_invoked && printf '%s' "$OUT" | grep -q "no frontend/package.json"; then
  ok "no package.json: returns 0, bun NOT invoked"
else
  bad "no package.json: expected rc 0 + no bun (rc=$RC, out=$OUT)"
fi

# ============================================================================
# 6. run_worktree_deps — bun install fails (FAKE_BUN_EXIT=1) -> non-zero
# ============================================================================
echo "--- run_worktree_deps: install failure (returns non-zero) ---"
d=$(mk_tmp); if_main="$d/main"; if_this="$d/wt"
mkdir -p "$if_main" "$if_this/frontend"
printf '{"name":"fe"}\n' > "$if_this/frontend/package.json"
deps_unit "$if_main" "$if_this" "$FAKE_BIN:/usr/bin:/bin" 1
if [ "$RC" -ne 0 ] && stub_invoked && printf '%s' "$OUT" | grep -q "install FAILED"; then
  ok "install failure: non-zero exit + FAILED message (bun was attempted)"
else
  bad "install failure: expected non-zero + FAILED message (rc=$RC, out=$OUT)"
fi

# ============================================================================
# 7 + 8. End-to-end: a real `git worktree add` fires the hook
# ============================================================================
echo "--- end-to-end: git worktree add (hook installs deps + env-link intact) ---"

# setup_hooked_main <dir> — temp "main" checkout with the REAL scripts, the
# canonical relative core.hooksPath, an env-file .gitignore + a tracked
# .env.example, a tracked frontend/package.json, and one commit. Mirrors the
# env-link test's helper, extended to also copy install-worktree-deps.sh.
setup_hooked_main() {
  local m="$1"
  mkdir -p "$m/scripts/hooks" "$m/frontend"
  cp "$REPO/scripts/link-worktree-env.sh"     "$m/scripts/link-worktree-env.sh"
  cp "$REPO/scripts/install-worktree-deps.sh" "$m/scripts/install-worktree-deps.sh"
  cp "$REPO/scripts/hooks/post-checkout"      "$m/scripts/hooks/post-checkout"
  chmod +x "$m/scripts/link-worktree-env.sh" "$m/scripts/install-worktree-deps.sh" "$m/scripts/hooks/post-checkout"
  printf '.env\nfrontend/.env.local\n' > "$m/.gitignore"
  printf 'EXAMPLE=1\n' > "$m/.env.example"
  printf '{"name":"fe"}\n' > "$m/frontend/package.json"   # tracked -> present in worktrees
  git -C "$m" init -q -b main
  git -C "$m" config user.email test@example.com
  git -C "$m" config user.name "Test"
  git -C "$m" config commit.gpgsign false
  git -C "$m" config core.hooksPath scripts/hooks
  git -C "$m" add -A
  git -C "$m" -c commit.gpgsign=false commit -q -m init
}

# --- 7. Success: stub bun (exit 0) -> deps installed, env files still linked ---
e2e=$(mk_tmp); main="$e2e/main"; setup_hooked_main "$main"
printf 'ROOT=1\n' > "$main/.env"
printf 'FE=1\n'   > "$main/frontend/.env.local"
log="$e2e/bun.log"; : > "$log"
wt="$e2e/wt"
if ( export PATH="$FAKE_BIN:/usr/bin:/bin" FAKE_BUN_LOG="$log" FAKE_BUN_EXIT=0
     git -C "$main" worktree add -q "$wt" -b feature HEAD ) 2>/dev/null; then
  addrc=0
else
  addrc=$?
fi
assert_true "worktree add succeeds (exit 0)" test "$addrc" -eq 0
# (a) deps stub ran in the new worktree's frontend/
e2e_cwd=$(grep '^cwd=' "$log" | head -1 | cut -d= -f2-)
if grep -q '^argv=install --frozen-lockfile$' "$log" \
   && [ -n "$e2e_cwd" ] && [ "$e2e_cwd" -ef "$wt/frontend" ]; then
  ok "hook ran the deps install in the new worktree's frontend/"
else
  bad "hook deps install: argv/cwd wrong (cwd='$e2e_cwd', log=$(cat "$log"))"
fi
# (b) env-link not regressed
if [ -L "$wt/.env" ] && [ "$wt/.env" -ef "$main/.env" ] \
   && [ -L "$wt/frontend/.env.local" ] && [ "$wt/frontend/.env.local" -ef "$main/frontend/.env.local" ]; then
  ok "env files still symlinked into the worktree (env-link not regressed)"
else
  bad "env-link regressed: .env / frontend/.env.local not symlinked"
fi

# --- 8. Hook never aborts the checkout on install failure (stub bun exit 1) ---
mainf="$e2e/mainf"; setup_hooked_main "$mainf"
printf 'ROOT=1\n' > "$mainf/.env"
logf="$e2e/bunf.log"; : > "$logf"
wtf="$e2e/wtf"; errf="$e2e/wtf.err"
if ( export PATH="$FAKE_BIN:/usr/bin:/bin" FAKE_BUN_LOG="$logf" FAKE_BUN_EXIT=1
     git -C "$mainf" worktree add -q "$wtf" -b feature HEAD ) 2>"$errf"; then
  addrcf=0
else
  addrcf=$?
fi
if [ "$addrcf" -eq 0 ] && [ -d "$wtf" ] && [ -f "$wtf/frontend/package.json" ]; then
  ok "install failure: worktree add still exits 0 and the worktree exists"
else
  bad "install failure: worktree add should still succeed (rc=$addrcf)"
fi
if grep -q "install FAILED" "$errf"; then
  ok "install failure: the FAILED message reached stderr"
else
  bad "install failure: expected FAILED message on stderr (got: $(cat "$errf"))"
fi

# ============================================================================
# 9. check-frontend-deps.sh — the 3-bin preflight
# ============================================================================
echo "--- check-frontend-deps.sh (preflight) ---"

# mk_bins <node_modules_dir> <bin...>: create executable .bin/<bin> stubs.
mk_bins() {
  local nm="$1"; shift; mkdir -p "$nm/.bin"
  local b; for b in "$@"; do printf '#!/bin/sh\n' > "$nm/.bin/$b"; chmod +x "$nm/.bin/$b"; done
}

# Pure predicate matrix (no git needed).
r=$(mk_tmp); mkdir -p "$r/frontend"; printf '{"name":"fe"}\n' > "$r/frontend/package.json"
mk_bins "$r/frontend/node_modules" next prettier vitest
assert_true  "frontend_deps_ok: all three bins present" frontend_deps_ok "$r"
r=$(mk_tmp); mkdir -p "$r/frontend"; printf '{"name":"fe"}\n' > "$r/frontend/package.json"
assert_false "frontend_deps_ok: node_modules missing" frontend_deps_ok "$r"
r=$(mk_tmp); mkdir -p "$r/frontend"; printf '{"name":"fe"}\n' > "$r/frontend/package.json"
mk_bins "$r/frontend/node_modules" next   # partial: missing prettier + vitest
assert_false "frontend_deps_ok: partial install (only next)" frontend_deps_ok "$r"
r=$(mk_tmp)   # no frontend/package.json at all
assert_true  "frontend_deps_ok: no frontend/package.json -> nothing to check" frontend_deps_ok "$r"

# Executed script (resolves the repo root via git) — exit code + message.
mk_fe_repo() {  # mk_fe_repo <dir> ; caller populates node_modules afterward
  local g="$1"; mkdir -p "$g/frontend"; printf '{"name":"fe"}\n' > "$g/frontend/package.json"
  git -C "$g" init -q -b main
  git -C "$g" config user.email test@example.com
  git -C "$g" config user.name "Test"
}
g=$(mk_tmp); mk_fe_repo "$g"; mk_bins "$g/frontend/node_modules" next prettier vitest
out=$( cd "$g" && bash "$REPO/scripts/hooks/check-frontend-deps.sh" 2>&1 ); rc=$?
if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
  ok "check script: all bins present -> exit 0, silent"
else
  bad "check script: present -> expected exit 0 + silence (rc=$rc, out=$out)"
fi
g=$(mk_tmp); mk_fe_repo "$g"   # no node_modules
out=$( cd "$g" && bash "$REPO/scripts/hooks/check-frontend-deps.sh" 2>&1 ); rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "frontend dependencies are not installed"; then
  ok "check script: deps missing -> exit 1 + actionable message"
else
  bad "check script: missing -> expected exit 1 + message (rc=$rc, out=$out)"
fi

# ============================================================================
# 10. pre-push GATE wiring (the gate must run BEFORE the parallel block)
# ============================================================================
echo "--- pre-push check_frontend_deps gate wiring ---"
gate_line=$(grep -nE '^check_frontend_deps([[:space:]]|$)' "$REPO/scripts/hooks/pre-push" | head -1 | cut -d: -f1)
rpp_line=$(grep -nE '^run_phases_parallel([[:space:]]|$)' "$REPO/scripts/hooks/pre-push" | head -1 | cut -d: -f1)
if [ -n "$gate_line" ] && [ -n "$rpp_line" ] && [ "$gate_line" -lt "$rpp_line" ]; then
  ok "gate is called before run_phases_parallel (lines $gate_line < $rpp_line)"
else
  bad "gate call ordering wrong (gate=$gate_line, run_phases_parallel=$rpp_line)"
fi
if ( source "$REPO/scripts/hooks/pre-push" >/dev/null 2>&1
     declare -f check_frontend_deps >/dev/null \
       && declare -f check_frontend_deps | grep -q 'check-frontend-deps.sh' ); then
  ok "sourced hook defines check_frontend_deps invoking check-frontend-deps.sh"
else
  bad "hook must define check_frontend_deps that invokes check-frontend-deps.sh"
fi

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
