#!/usr/bin/env bash
# Tests for ghcr-retention.sh — the git-ancestry-aware GHCR tag delete decision.
#
# Builds a throwaway linear git fixture (main a strict prefix of develop), points
# MAIN_REF at a chosen commit, feeds a MOCKED package-version list through the
# GHCR_LIST_VERSIONS_CMD injection hook, and asserts which versions the script
# marks for deletion (DRY_RUN=1, so nothing hits the network). Also covers the
# fail-closed paths (unresolvable main, too-short history, real-run-without-token).
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
  # Name the branch `develop` so neither `main` nor `master` exists — the
  # unresolvable-main test relies on the script's `main` fallback also failing.
  git checkout -q -b develop 2>/dev/null || git branch -q -m develop
) || { echo "fixture init failed" >&2; exit 1; }

for i in $(seq 1 12); do
  ( cd "$FIXTURE" && echo "c$i" > file.txt && git add file.txt && git commit -q -m "c$i" )
  C[i]="$( cd "$FIXTURE" && git rev-parse HEAD )"
done

# main tip = C9 (so C1..C9 are promoted ancestors; C10..C12 are ahead/un-promoted).
MAIN=${C[9]}

# Mock version list: "<id>\t<tags>". Tab-separated, one per line.
MOCK="$FIXTURE/versions.tsv"
{
  printf '100\t%s\n'    "${C[3]}"            # deletable: promoted, >3 behind tip
  printf '101\t%s\n'    "${C[6]}"            # deletable: exactly the floor (C9~3)
  printf '102\t%s\n'    "${C[7]}"            # KEEP: within rollback buffer
  printf '103\t%s\n'    "${C[9]}"            # KEEP: current prod image (main tip)
  printf '104\t%s\n'    "${C[11]}"           # KEEP: un-promoted (ahead of main)
  printf '105\t%s,latest\n' "${C[2]}"        # KEEP: carries :latest despite old sha
  printf '106\t\n'                            # KEEP: untagged
  printf '107\t%s\n'    "${C[1]}"            # deletable: oldest promoted
} > "$MOCK"

run() {  # run <main_ref> <buffer> <dry_run> <token>  -> stderr captured to $OUT, rc in $RC
  OUT="$FIXTURE/out.$$"
  ( cd "$FIXTURE" \
    && MAIN_REF="$1" BUFFER="$2" DRY_RUN="$3" GH_TOKEN="$4" \
       OWNER="testowner" PACKAGES="pkg" \
       GHCR_LIST_VERSIONS_CMD="cat '$MOCK'" \
       bash "$SCRIPT" ) >/dev/null 2>"$OUT"
  RC=$?
}

# ---- Test 1: the core delete decision (dry run). ----
run "$MAIN" 3 1 ""
[ "$RC" -eq 0 ] || fail "core: expected rc 0, got $RC"
for id in 100 101 107; do
  if grep -q "would delete pkg version ${id} " "$OUT"; then ok; else fail "core: version ${id} should be deleted"; fi
done
for id in 102 103 104 105 106; do
  if grep -q "version ${id} " "$OUT"; then fail "core: version ${id} must be kept"; else ok; fi
done
# exactly 3 deletions reported
if grep -q "3 version(s) would be deleted" "$OUT"; then ok; else fail "core: expected 3 deletions, got: $(grep 'would be deleted' "$OUT")"; fi

# ---- Test 2: unresolvable main -> fail-closed, delete nothing. ----
run "refs/heads/nope" 3 1 ""
[ "$RC" -eq 0 ] || fail "unresolvable-main: expected rc 0, got $RC"
if grep -q "cannot resolve main tip" "$OUT"; then ok; else fail "unresolvable-main: expected resolve failure message"; fi
if grep -q "would delete" "$OUT"; then fail "unresolvable-main: must delete nothing"; else ok; fi

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

# ---- Test 5: real run actually invokes delete for the right ids only. ----
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
if [ "$deleted" = "100 101 107 " ]; then ok; else fail "real-run: expected deleted ids '100 101 107', got '${deleted}'"; fi

rm -rf "$FIXTURE"

echo "ghcr-retention.test.sh: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
