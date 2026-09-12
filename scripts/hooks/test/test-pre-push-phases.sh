#!/bin/bash
# Exercise the real hook in an isolated repo with lightweight check commands.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."
repo="$PWD"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
git init -q "$tmp"
cd "$tmp"
git config user.name "Test"
git config user.email "test@example.invalid"
git config commit.gpgsign false
git config core.hooksPath /dev/null
mkdir -p .ai scripts/hooks
cp "$repo/scripts/hooks/pre-push" scripts/hooks/pre-push
cp "$repo/path-filters.yml" .
# Preserve the real selection config while replacing heavyweight commands.
jq '.checks |= map(.command = ("echo executed:" + .name + "; test \"$FAIL_CHECK\" != " + (.name | @sh)))' \
  "$repo/.ai/pre-push.json" > .ai/pre-push.json
git add .
git commit -qm base
base=$(git rev-parse HEAD)
git update-ref refs/remotes/origin/develop "$base"
export FAIL_CHECK=""
source scripts/hooks/pre-push

assert_contains() {
  if ! grep -qF "$2" <<< "$1"; then
    echo "FAIL: expected $2"; exit 1
  fi
}
assert_absent() {
  if grep -qF "$2" <<< "$1"; then
    echo "FAIL: unexpected $2"; exit 1
  fi
}
select_for() { run_checks "$1"; }
out=$(select_for README.md)
assert_contains "$out" "executed:Repo hygiene"
assert_absent "$out" "executed:Frontend lint"
assert_absent "$out" "executed:Backend lint"
assert_absent "$out" "executed:Spec drift"
out=$(select_for backend/internal/example.go)
assert_contains "$out" "executed:Backend lint"
assert_contains "$out" "executed:Spec drift"
assert_contains "$out" "executed:API docs drift"
assert_absent "$out" "executed:Frontend lint"
out=$(select_for frontend/src/example.ts)
assert_contains "$out" "executed:Frontend lint"
assert_contains "$out" "executed:Spec coverage"
assert_contains "$out" "executed:API types drift"
assert_absent "$out" "executed:Backend lint"
out=$(select_for spec/example.yaml)
assert_contains "$out" "executed:Spec lint"
assert_contains "$out" "executed:Spec drift"
assert_absent "$out" "executed:Frontend lint"
assert_absent "$out" "executed:Backend lint"
out=$(select_for mac-daemon/Sources/example.swift)
assert_absent "$out" "executed:Frontend lint"
assert_absent "$out" "executed:Backend lint"
out=$(select_for .ai/pre-push.json)
assert_contains "$out" "executed:Frontend lint"
assert_contains "$out" "executed:Backend lint"
assert_contains "$out" "executed:Spec drift"
out=$(select_for Makefile)
assert_contains "$out" "executed:Backend lint"
assert_absent "$out" "executed:Frontend lint"

# Formatting passes literal existing paths and never treats an empty set as ".".
mkdir -p frontend/node_modules/.bin 'frontend/src/[id]'
cat > frontend/node_modules/.bin/prettier <<'SH'
#!/bin/bash
printf '%s\n' "$@" > "$PRETTIER_ARGS"
exit "${PRETTIER_EXIT:-0}"
SH
chmod +x frontend/node_modules/.bin/prettier
export PRETTIER_ARGS="$tmp/prettier-args"
touch 'frontend/src/[id]/with space.ts' frontend/src/other.ts
check_frontend_format $'frontend/src/[id]/with space.ts\nfrontend/deleted.ts\nbackend/example.go'
assert_contains "$(cat "$PRETTIER_ARGS")" "./src/[id]/with space.ts"
assert_absent "$(cat "$PRETTIER_ARGS")" "deleted.ts"
assert_absent "$(cat "$PRETTIER_ARGS")" "other.ts"
rm "$PRETTIER_ARGS"
check_frontend_format frontend/deleted.ts
[[ ! -e "$PRETTIER_ARGS" ]]
check_frontend_format frontend/.prettierignore
[[ "$(cat "$PRETTIER_ARGS")" == $'--check\n.' ]]
export PRETTIER_EXIT=1
if check_frontend_format frontend/src/other.ts; then
  echo "FAIL: formatting failure accepted"; exit 1
fi
unset PRETTIER_EXIT
rm -rf "$tmp/frontend"
rm -f "$PRETTIER_ARGS"

# Real git stdin: new branch, existing branch, reversal, deletion, multiple refs.
mkdir -p backend
touch backend/example.go
git add .
git commit -qm backend
head=$(git rev-parse HEAD)
zero=0000000000000000000000000000000000000000
out=$(printf 'refs/heads/feature %s refs/heads/feature %s\n' "$head" "$zero" | bash scripts/hooks/pre-push origin)
assert_contains "$out" "executed:Backend lint"
assert_absent "$out" "executed:Frontend lint"
export FAIL_CHECK="Spec drift"
if printf 'refs/heads/feature %s refs/heads/feature %s\n' "$head" "$base" | bash scripts/hooks/pre-push origin > "$tmp/failure.log" 2>&1; then
  echo "FAIL: required check failure allowed push"; exit 1
fi
assert_contains "$(cat "$tmp/failure.log")" "FAIL Spec drift"
export FAIL_CHECK=""
[[ "$(pushed_files "refs/heads/feature $base refs/heads/feature $head")" == "backend/example.go" ]]
[[ -z "$(pushed_files "delete $zero refs/heads/feature $head")" ]]
[[ "$(pushed_files "refs/heads/feature $head refs/heads/feature $base")" == "backend/example.go" ]]
out=$(pushed_files "delete $zero refs/heads/old $head
refs/heads/feature $head refs/heads/feature $base")
[[ "$out" == "backend/example.go" ]]
mkdir -p docs
git mv backend/example.go docs/example.go
git commit -qm rename
renamed=$(git rev-parse HEAD)
out=$(pushed_files "refs/heads/feature $renamed refs/heads/feature $head")
assert_contains "$out" "backend/example.go"
assert_contains "$out" "docs/example.go"
if pushed_files "refs/heads/feature $head refs/heads/feature missing" >/dev/null 2>&1; then
  echo "FAIL: invalid remote SHA accepted"; exit 1
fi
git update-ref -d refs/remotes/origin/develop
if pushed_files "refs/heads/feature $head refs/heads/feature $zero" >/dev/null 2>&1; then
  echo "FAIL: missing development base accepted"; exit 1
fi

# A shared hooksPath still checks the invoking worktree.
out=$(printf 'refs/heads/feature %s refs/heads/feature %s\n' "$head" "$base" | bash "$repo/scripts/hooks/pre-push" origin)
assert_contains "$out" "executed:Backend lint"
out=$(printf 'refs/heads/develop %s refs/heads/main %s\n' "$head" "$base" | bash scripts/hooks/pre-push origin)
assert_contains "$out" "promotion to main"
assert_absent "$out" "executed:"
if refs_all_target_main "refs/heads/main $head refs/heads/main $base
refs/heads/feature $head refs/heads/feature $base"; then
  echo "FAIL: mixed push skipped"; exit 1
fi
if refs_all_target_main ""; then echo "FAIL: empty push skipped"; exit 1; fi
rm .ai/pre-push.json
if printf 'refs/heads/develop %s refs/heads/main %s\n' "$head" "$base" | bash "$repo/scripts/hooks/pre-push" origin >/dev/null 2>&1; then
  echo "FAIL: missing configuration accepted"; exit 1
fi
echo "ALL PASS (pre-push selection and execution)"
