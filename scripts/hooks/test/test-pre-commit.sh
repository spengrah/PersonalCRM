#!/usr/bin/env bash
# Real Git index/working-tree fixtures; Go formatting is real, Prettier is local and stubbed.
set -euo pipefail
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git init -q "$tmp"
cd "$tmp"
git config user.name Test
git config user.email test@example.invalid
git config commit.gpgsign false
git config core.hooksPath /dev/null
git commit --allow-empty -qm baseline

hook() { bash "$repo/scripts/hooks/pre-commit"; }
fail() { echo "FAIL: $1" >&2; exit 1; }

# Fully staged files are formatted and the intended content remains staged.
printf 'package example\nvar Value=1\n' > 'full file.go'
git add -- 'full file.go'
hook
gofmt < 'full file.go' > "$tmp/expected"
cmp -s 'full file.go' "$tmp/expected" || fail "Go formatting"
git diff --quiet -- 'full file.go' || fail "formatted content not staged"
git commit -qm formatted

# Formatted staged content passes even if the unstaged version is invalid Go.
printf 'package example\n\nvar Value = 2\n' > 'full file.go'
git add -- 'full file.go'
printf 'unstaged and deliberately invalid\n' >> 'full file.go'
cp 'full file.go' "$tmp/working-before"
git show ':full file.go' > "$tmp/index-before"
hook
cmp -s 'full file.go' "$tmp/working-before" || fail "unstaged edits changed"
git show ':full file.go' > "$tmp/index-after"
cmp -s "$tmp/index-before" "$tmp/index-after" || fail "partial index changed"

# Unformatted partial staging fails without pulling in any unstaged contents.
printf 'package example\nvar Value=3\n' > 'full file.go'
git add -- 'full file.go'
printf '// keep unstaged\n' >> 'full file.go'
cp 'full file.go' "$tmp/working-before"
git show ':full file.go' > "$tmp/index-before"
if hook > "$tmp/error" 2>&1; then fail "unformatted staged content accepted"; fi
cmp -s 'full file.go' "$tmp/working-before" || fail "partial worktree changed"
git show ':full file.go' > "$tmp/index-after"
cmp -s "$tmp/index-before" "$tmp/index-after" || fail "unstaged content was staged"
grep -q 'has unstaged changes' "$tmp/error" || fail "missing partial-staging diagnostic"
git rm -fq -- 'full file.go'
git commit -qm remove

# A rename, special filename, and executable mode survive formatting.
printf 'package example\nvar Value=4\n' > source.go
chmod +x source.go
git add source.go
git commit -qm source
name=$'renamed [id] "quote"\nline.go'
git mv source.go "$name"
hook
[[ -x "$name" ]] || fail "executable mode lost"
git diff --quiet -- "$name" || fail "renamed file not staged"
git commit -qm renamed

# Symlink targets are never formatted.
printf 'do not modify\n' > target
ln -s target link.go
git add link.go
hook
[[ "$(cat target)" == "do not modify" ]] || fail "symlink target modified"
git commit -qm symlink

# No package-manager invocation: use only the already installed Prettier binary.
mkdir -p frontend/node_modules/.bin 'frontend/src/[id]'
cat > frontend/node_modules/.bin/prettier <<'SH'
#!/bin/bash
[[ "$1" == --stdin-filepath ]] || exit 8
printf '%s\n' "$2" > "$PRETTIER_ARGS"
[[ "${PRETTIER_FAIL:-0}" == 0 ]] || exit 9
sed 's/BAD/GOOD/g'
SH
chmod +x frontend/node_modules/.bin/prettier
export PRETTIER_ARGS="$tmp/prettier-args"
printf 'BAD\n' > 'frontend/src/[id]/with space.ts'
git add -- 'frontend/src/[id]/with space.ts'
hook
[[ "$(cat "$PRETTIER_ARGS")" == './src/[id]/with space.ts' ]] || fail "literal frontend path lost"
[[ "$(git show ':frontend/src/[id]/with space.ts')" == GOOD ]] || fail "frontend formatting not staged"
printf 'UNSTAGED\n' >> 'frontend/src/[id]/with space.ts'
hook
[[ "$(git show ':frontend/src/[id]/with space.ts')" == GOOD ]] || fail "frontend unstaged edit included"
export PRETTIER_FAIL=1
if hook > "$tmp/error" 2>&1; then fail "formatter failure accepted"; fi
unset PRETTIER_FAIL
mv frontend/node_modules/.bin/prettier frontend/node_modules/.bin/unavailable
if hook > "$tmp/error" 2>&1; then fail "missing formatter accepted"; fi
grep -q 'install frontend dependencies' "$tmp/error" || fail "missing dependency diagnostic"
echo "ALL PASS (pre-commit staging boundaries)"
