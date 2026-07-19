#!/bin/bash
# Unit tests for scripts/promote-preflight.sh.
#
# Git-fixture hygiene: the pre-push hook exports GIT_DIR/GIT_WORK_TREE/
# GIT_INDEX_FILE, and a fixture's `git init`/`git config` would otherwise hit the
# REAL repo (work-tree fatals AND identity pollution). Unset them up front.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/promote-preflight.sh"
PASS=0; FAIL=0
ok()   { echo "ok: $1"; PASS=$((PASS+1)); }
bad()  { echo "FAIL: $1"; FAIL=$((FAIL+1)); }

# A throwaway repo with a fake `gh` and a fake `origin`, so nothing touches the
# network or the real checkout.
setup_repo() {
  REPO=$(mktemp -d); BIN=$(mktemp -d)
  git init -q "$REPO/origin.git" --bare
  git init -q "$REPO/wt"
  cd "$REPO/wt"
  git config user.email t@example.com; git config user.name t
  git config commit.gpgsign false
  git remote add origin "$REPO/origin.git"
  echo one > f && git add f && git commit -qm one
  git branch -M develop && git push -q origin develop
  export PATH="$BIN:$PATH"
}
teardown() { cd /; rm -rf "$REPO" "$BIN"; }

# $1 = conclusion the fake gh reports; "ERR" makes it exit non-zero.
fake_gh() {
  cat > "$BIN/gh" <<EOF
#!/bin/bash
[ "$1" = "ERR" ] && exit 1
echo "$1"
EOF
  chmod +x "$BIN/gh"
}

run_pf() { bash "$SCRIPT" >"$REPO/out" 2>&1; echo $?; }

# --- both gates green, refs in sync -> pass ---
setup_repo; fake_gh success
[ "$(run_pf)" = "0" ] && ok "in-sync + green -> exit 0" || bad "in-sync + green should pass ($(cat "$REPO/out"))"
teardown

# --- stale local ref -> refuse, and NAME the missing commits ---
setup_repo; fake_gh success
echo two > f2 && git add f2 && git commit -qm "second commit"
git push -q origin develop
git reset -q --hard HEAD~1          # local now BEHIND origin
rc=$(run_pf)
[ "$rc" != "0" ] && ok "stale local ref -> non-zero" || bad "stale local ref should refuse"
grep -q "BEHIND by 1" "$REPO/out" && ok "stale ref names the gap" || bad "stale ref should report how far behind"
grep -q "second commit" "$REPO/out" && ok "stale ref lists the unshipped commit" || bad "should list missing commits"
teardown

# --- CI not finished ("missing") -> refuse ---
setup_repo; fake_gh missing
rc=$(run_pf)
[ "$rc" != "0" ] && ok "missing CI run -> non-zero" || bad "missing CI should refuse"
grep -q "still be running" "$REPO/out" && ok "missing CI explains it may still be running" || bad "missing CI needs a wait hint"
teardown

# --- CI failed -> refuse ---
setup_repo; fake_gh failure
[ "$(run_pf)" != "0" ] && ok "failed CI -> non-zero" || bad "failed CI should refuse"
teardown

# --- gh unavailable/unauthenticated -> FAIL CLOSED (the key property) ---
setup_repo; fake_gh ERR
rc=$(run_pf)
[ "$rc" != "0" ] && ok "gh error -> non-zero (fails closed)" || bad "gh error must BLOCK, not assume green"
grep -q "Blocking rather than assuming green" "$REPO/out" && ok "gh error says it is blocking" || bad "gh error needs a clear message"
teardown

# --- gh absent entirely -> refuse ---
setup_repo
cd "$REPO/wt"
rc=$(PATH="/usr/bin:/bin" bash "$SCRIPT" >"$REPO/out" 2>&1; echo $?)
[ "$rc" != "0" ] && ok "gh not installed -> non-zero" || bad "missing gh should refuse"
teardown

echo "---"; echo "pass=$PASS fail=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
echo "ALL PASS (promote pre-flight)"
