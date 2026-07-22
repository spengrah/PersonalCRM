#!/usr/bin/env bash
# Tests for ghcr-retention.sh — the git-ancestry-aware GHCR tag delete decision.
#
# Builds a throwaway linear git fixture (main a strict prefix of develop), points
# MAIN_REF at a chosen commit, feeds a MOCKED package-version list through the
# GHCR_LIST_VERSIONS_CMD injection hook, and asserts which versions the script
# marks for deletion (DRY_RUN=1, so nothing hits the network). Also covers the
# fail-closed paths (unresolvable main with NO fallback, too-short history, invalid
# BUFFER, real-run-without-token, list-command failure), the buffer boundary, the
# tip/un-promoted safety property across BUFFER values, and multi-package handling.
#
# Portable: no network, no BSD-only flags. Sets a LOCAL git identity (CI may lack a
# global one) and unsets the hook-inherited git env so the fixture is isolated.

set -uo pipefail

# Git pre-push hooks export GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE; without unsetting
# them the fixture's git commands (and the script's rev-parse/rev-list in the
# fixture CWD) would hit the real repo. Unset restores cwd-based discovery.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/ci/ghcr-retention.sh"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---- Build a 12-commit linear fixture; capture each commit SHA into C1..C12. ----
FIXTURE="$(mktemp -d)"
declare -a C
(
  cd "$FIXTURE" || exit 1
  git init -q
  git config user.email "ci@example.com"
  git config user.name "CI"
  git config commit.gpgsign false
  git checkout -q -b develop 2>/dev/null || git branch -q -m develop
) || { echo "fixture init failed" >&2; exit 1; }

for i in $(seq 1 12); do
  ( cd "$FIXTURE" && echo "c$i" > file.txt && git add file.txt && git commit -q -m "c$i" )
  C[i]="$( cd "$FIXTURE" && git rev-parse HEAD )"
done

# A local `main` at C12 (divergent from the C9 we treat as prod). Its presence is
# what makes the no-fallback test meaningful: a stale/divergent local main must NOT
# be used when the configured ref is unresolvable.
( cd "$FIXTURE" && git branch main develop )

# main tip = C9 (so C1..C9 are promoted ancestors; C10..C12 are ahead/un-promoted).
MAIN=${C[9]}

# Mock version list: "<id>\t<tags>". With MAIN=C9, BUFFER=3 the delete floor is
# C9~4 = C5, so deletable = {C1..C5} (strictly MORE than 3 commits behind the tip).
MOCK="$FIXTURE/versions.tsv"
{
  printf '100\t%s\n'         "${C[3]}"          # DELETE: promoted, 6 behind
  printf '101\t%s\n'         "${C[6]}"          # KEEP: exactly BUFFER (3) behind — boundary
  printf '102\t%s\n'         "${C[7]}"          # KEEP: within rollback buffer
  printf '103\t%s\n'         "${C[9]}"          # KEEP: current prod image (main tip)
  printf '104\t%s\n'         "${C[11]}"         # KEEP: un-promoted (ahead of main)
  printf '105\t%s,latest\n'  "${C[2]}"          # KEEP: carries :latest despite old sha
  printf '106\t\n'                               # KEEP: untagged
  printf '107\t%s\n'         "${C[1]}"          # DELETE: oldest promoted
  printf '108\t%s\n'         "${C[5]}"          # DELETE: exactly BUFFER+1 (4) behind — boundary
  printf '109\t%s,%s\n'      "${C[4]}" "${C[11]}" # KEEP: one deletable sha + one un-promoted sha
} > "$MOCK"

run() {  # run <main_ref> <buffer> <dry_run> <token> [list_cmd]  -> stderr in $OUT, rc in $RC
  OUT="$FIXTURE/out.$$"
  # $MOCK expands (outer double quotes); the inner single quotes are literal so the
  # injected command is `cat '<path>'`. shellcheck SC2016 misreads the nesting.
  # shellcheck disable=SC2016
  local list_cmd="${5:-cat '$MOCK'}"
  ( cd "$FIXTURE" \
    && MAIN_REF="$1" BUFFER="$2" DRY_RUN="$3" GH_TOKEN="$4" \
       OWNER="testowner" PACKAGES="pkg" \
       GHCR_LIST_VERSIONS_CMD="$list_cmd" \
       bash "$SCRIPT" ) >/dev/null 2>"$OUT"
  RC=$?
}

# ---- Test 1: the core delete decision (dry run) under the new floor semantics. ----
run "$MAIN" 3 1 ""
[ "$RC" -eq 0 ] || fail "core: expected rc 0, got $RC"
for id in 100 107 108; do
  if grep -q "would delete pkg version ${id} " "$OUT"; then ok; else fail "core: version ${id} should be deleted"; fi
done
for id in 101 102 103 104 105 106 109; do
  if grep -q "version ${id} " "$OUT"; then fail "core: version ${id} must be kept"; else ok; fi
done
if grep -q "3 version(s) would be deleted" "$OUT"; then ok; else fail "core: expected 3 deletions, got: $(grep 'would be deleted' "$OUT")"; fi

# ---- Test 2: unresolvable main + NO fallback -> delete nothing (stale local main
#              at C12 must NOT be used). ----
run "refs/remotes/origin/does-not-exist" 3 1 ""
[ "$RC" -eq 0 ] || fail "no-fallback: expected rc 0, got $RC"
if grep -q "cannot resolve main tip" "$OUT"; then ok; else fail "no-fallback: expected resolve failure message"; fi
if grep -q "would delete" "$OUT"; then fail "no-fallback: must delete nothing (no local-main fallback)"; else ok; fi

# ---- Test 3: history <= BUFFER behind tip -> nothing eligible. ----
run "$MAIN" 50 1 ""
[ "$RC" -eq 0 ] || fail "short-history: expected rc 0, got $RC"
if grep -q "nothing eligible" "$OUT"; then ok; else fail "short-history: expected 'nothing eligible'"; fi
if grep -q "would delete" "$OUT"; then fail "short-history: must delete nothing"; else ok; fi

# ---- Test 4: real run (DRY_RUN=0) with no token -> green no-op skip. ----
run "$MAIN" 3 0 ""
[ "$RC" -eq 0 ] || fail "no-token: expected rc 0 (green skip), got $RC"
if grep -q "not set; skipping" "$OUT"; then ok; else fail "no-token: expected skip message"; fi
if grep -q "would delete\|deleting" "$OUT"; then fail "no-token: must delete nothing"; else ok; fi

# ---- Test 5: invalid BUFFER -> fail-closed (guards the $((10#$BUFFER + 1)) math). ----
# 5a: non-numeric.
run "$MAIN" "abc" 1 ""
[ "$RC" -eq 0 ] || fail "bad-buffer(abc): expected rc 0, got $RC"
if grep -q "not a non-negative integer" "$OUT"; then ok; else fail "bad-buffer(abc): expected validation message"; fi
if grep -q "would delete" "$OUT"; then fail "bad-buffer(abc): must delete nothing"; else ok; fi
# 5b: negative (the '-' is a non-digit -> rejected).
run "$MAIN" "-5" 1 ""
[ "$RC" -eq 0 ] || fail "bad-buffer(-5): expected rc 0, got $RC"
if grep -q "not a non-negative integer" "$OUT"; then ok; else fail "bad-buffer(-5): expected validation message"; fi
if grep -q "would delete" "$OUT"; then fail "bad-buffer(-5): must delete nothing"; else ok; fi
# 5c: overflow-sized value (> 9 digits) -> rejected before it can wrap the arithmetic.
run "$MAIN" "10000000000" 1 ""
[ "$RC" -eq 0 ] || fail "bad-buffer(huge): expected rc 0, got $RC"
if grep -q "too many digits" "$OUT"; then ok; else fail "bad-buffer(huge): expected digit-count message"; fi
if grep -q "would delete" "$OUT"; then fail "bad-buffer(huge): must delete nothing"; else ok; fi

# ---- Test 6: real run deletes exactly the right ids. ----
DELLOG="$FIXTURE/deleted.log"
: > "$DELLOG"
( cd "$FIXTURE" \
  && MAIN_REF="$MAIN" BUFFER=3 DRY_RUN=0 GH_TOKEN="fake" \
     OWNER="testowner" PACKAGES="pkg" \
     GHCR_LIST_VERSIONS_CMD="cat '$MOCK'" \
     GHCR_DELETE_VERSION_CMD="printf '%s\n' \"\$VERSION_ID\" >> '$DELLOG'" \
     bash "$SCRIPT" ) >/dev/null 2>"$FIXTURE/out.real"
rc=$?
[ "$rc" -eq 0 ] || fail "real-run: expected rc 0, got $rc"
deleted="$(sort "$DELLOG" | tr '\n' ' ')"
if [ "$deleted" = "100 107 108 " ]; then ok; else fail "real-run: expected deleted ids '100 107 108', got '${deleted}'"; fi

# ---- Test 7: SAFETY PROPERTY — across BUFFER values, the tip (103/C9) and an
#              un-promoted sha (104/C11) are NEVER deleted. ----
for b in 0 1 3 5; do
  : > "$DELLOG"
  ( cd "$FIXTURE" \
    && MAIN_REF="$MAIN" BUFFER="$b" DRY_RUN=0 GH_TOKEN="fake" \
       OWNER="testowner" PACKAGES="pkg" \
       GHCR_LIST_VERSIONS_CMD="cat '$MOCK'" \
       GHCR_DELETE_VERSION_CMD="printf '%s\n' \"\$VERSION_ID\" >> '$DELLOG'" \
       bash "$SCRIPT" ) >/dev/null 2>"$FIXTURE/out.prop"
  if grep -qx "103" "$DELLOG"; then fail "safety-property (BUFFER=$b): prod tip (103) was deleted"; else ok; fi
  if grep -qx "104" "$DELLOG"; then fail "safety-property (BUFFER=$b): un-promoted (104) was deleted"; else ok; fi
done

# ---- Test 8: multi-package — same mock for two packages -> 6 deletions. ----
: > "$DELLOG"
( cd "$FIXTURE" \
  && MAIN_REF="$MAIN" BUFFER=3 DRY_RUN=0 GH_TOKEN="fake" \
     OWNER="testowner" PACKAGES="pkg1 pkg2" \
     GHCR_LIST_VERSIONS_CMD="cat '$MOCK'" \
     GHCR_DELETE_VERSION_CMD="printf '%s\n' \"\$VERSION_ID\" >> '$DELLOG'" \
     bash "$SCRIPT" ) >/dev/null 2>"$FIXTURE/out.multi"
rc=$?
[ "$rc" -eq 0 ] || fail "multi-package: expected rc 0, got $rc"
n="$(grep -c . "$DELLOG")"
if [ "$n" -eq 6 ]; then ok; else fail "multi-package: expected 6 deletions (3 per package), got $n"; fi

# ---- Test 9: list-command failure on a real run -> surface it (rc 1), delete nothing. ----
: > "$DELLOG"
( cd "$FIXTURE" \
  && MAIN_REF="$MAIN" BUFFER=3 DRY_RUN=0 GH_TOKEN="fake" \
     OWNER="testowner" PACKAGES="pkg" \
     GHCR_LIST_VERSIONS_CMD="exit 3" \
     GHCR_DELETE_VERSION_CMD="printf '%s\n' \"\$VERSION_ID\" >> '$DELLOG'" \
     bash "$SCRIPT" ) >/dev/null 2>"$FIXTURE/out.listfail"
rc=$?
[ "$rc" -eq 1 ] || fail "list-failure: expected rc 1, got $rc"
if grep -q "could not list versions" "$FIXTURE/out.listfail"; then ok; else fail "list-failure: expected error message"; fi
if [ -s "$DELLOG" ]; then fail "list-failure: must delete nothing, deleted: $(tr '\n' ' ' < "$DELLOG")"; else ok; fi

# ---- Test 9b: multi-package where the SECOND package's listing fails -> the first
#      package's deletions still stand, the run exits 1, and the shared $LISTFILE
#      does not bleed the good package's entries into the failed one. ----
: > "$DELLOG"
( cd "$FIXTURE" \
  && MAIN_REF="$MAIN" BUFFER=3 DRY_RUN=0 GH_TOKEN="fake" \
     OWNER="testowner" PACKAGES="pkggood pkgbad" \
     GHCR_LIST_VERSIONS_CMD="[ \"\$PKG\" = pkgbad ] && exit 3; cat '$MOCK'" \
     GHCR_DELETE_VERSION_CMD="printf '%s\n' \"\$VERSION_ID\" >> '$DELLOG'" \
     bash "$SCRIPT" ) >/dev/null 2>"$FIXTURE/out.mixfail"
rc=$?
[ "$rc" -eq 1 ] || fail "mixed-list-failure: expected rc 1, got $rc"
if grep -q "could not list versions for pkgbad" "$FIXTURE/out.mixfail"; then ok; else fail "mixed-list-failure: expected pkgbad error"; fi
mixdeleted="$(sort "$DELLOG" | tr '\n' ' ')"
if [ "$mixdeleted" = "100 107 108 " ]; then ok; else fail "mixed-list-failure: expected only pkggood's '100 107 108', got '${mixdeleted}'"; fi

# ---- Test 10: leading-zero BUFFER is treated as DECIMAL, not octal (10#$BUFFER).
#              With MAIN=C12, "010" as decimal 10 -> floor C12~11 = C1 -> only C1
#              deletable; the octal-8 bug would floor at C12~9 = C3 and also delete
#              C2/C3, narrowing the rollback window (the dangerous direction). ----
MAIN12=${C[12]}
MOCK2="$FIXTURE/versions2.tsv"
{
  printf '200\t%s\n' "${C[1]}"
  printf '201\t%s\n' "${C[2]}"
  printf '202\t%s\n' "${C[3]}"
} > "$MOCK2"
run "$MAIN12" "010" 1 "" "cat '$MOCK2'"
[ "$RC" -eq 0 ] || fail "octal-buffer: expected rc 0, got $RC"
if grep -q "would delete pkg version 200 " "$OUT"; then ok; else fail "octal-buffer: 200 (C1) should be deleted at BUFFER=010 (=decimal 10)"; fi
for id in 201 202; do
  if grep -q "version ${id} " "$OUT"; then fail "octal-buffer: version ${id} must be kept (010 must be decimal 10, not octal 8)"; else ok; fi
done

rm -rf "$FIXTURE"

echo "ghcr-retention.test.sh: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
