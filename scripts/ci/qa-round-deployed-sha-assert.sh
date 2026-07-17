#!/usr/bin/env bash
# qa-round-deployed-sha-assert.sh <DEPLOYED_SHA> [MANIFEST_PATH]
#
# Pure assertion that the repo checkout — and, after tours, the run manifest —
# exactly match the sha staging is deployed from. A stale checkout runs stale
# selectors against newer markup and manufactures false failures, so the round
# MUST pin to the deployed sha; this helper is how the wrapper proves the pin.
#
#   <DEPLOYED_SHA>   the full lowercase 40-hex git sha staging is built from (the
#                    exact output contract of staging-deployed-sha.sh). Only that
#                    form is accepted; anything else exits non-zero.
#   [MANIFEST_PATH]  optional run manifest (JSON). When supplied, the file's string
#                    `.gitSha` must ALSO be full lowercase 40-hex and EXACTLY equal
#                    to <DEPLOYED_SHA>.
#
# Called TWICE by the wrapper: with NO manifest BEFORE tours (the pre-tour pin
# assertion — checkout HEAD must equal the deployed sha) and WITH the just-written
# manifest AFTER tours (the post-tour provenance assertion — checkout AND manifest
# both equal the deployed sha, so a matching manifest cannot mask a stale checkout).
#
# Fail-closed: any bad arity, malformed sha, unreadable/missing/invalid manifest,
# missing/non-string/non-hex/mismatched `.gitSha`, or git error exits non-zero with
# a targeted message. It NEVER checks out or mutates the repo or the manifest.
# The wrapper owns reading/re-reading the deployed sha, the checkout, choosing the
# manifest, and watermark advancement.

set -uo pipefail

# Sanitize the repo's standard git-location env isolation set so `git rev-parse`
# resolves THIS repo (the one containing the script), not whatever a caller's env
# points at. A poisoned env would otherwise let the script assert against a different
# checkout's HEAD (accepting a stale/wrong sha), which a matching manifest could not
# catch. GIT_DIR / GIT_COMMON_DIR / GIT_OBJECT_DIRECTORY redirect resolution, and
# GIT_WORK_TREE with a nonexistent-parent path fatals rev-parse — all four are
# load-bearing. GIT_INDEX_FILE is inert for rev-parse but is unset as part of the
# standard isolation set. The isolation is contract-tested (a fake git that fails on
# any leaked var).
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY

err() { printf 'qa-round-deployed-sha-assert: %s\n' "$1" >&2; exit 1; }

HEX40='^[0-9a-f]{40}$'

if [ "$#" -ne 1 ] && [ "$#" -ne 2 ]; then
  err "usage: qa-round-deployed-sha-assert.sh <DEPLOYED_SHA> [MANIFEST_PATH]"
fi

DEPLOYED="$1"

[[ "$DEPLOYED" =~ $HEX40 ]] || err "DEPLOYED_SHA must be full lowercase 40-hex, got '$DEPLOYED'"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)"
[ -n "$REPO_ROOT" ] || err "could not resolve repo root from script location"

head="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null)" || err "could not resolve HEAD in $REPO_ROOT"
[ "$head" = "$DEPLOYED" ] || err "checkout HEAD ($head) != deployed sha ($DEPLOYED); pin the checkout before running tours"

# Manifest presence is decided by ARGUMENT COUNT, not by the value: a supplied but
# empty second arg ("") must still trigger (and fail) the manifest assertion rather
# than silently pass as if no manifest were requested.
if [ "$#" -eq 2 ]; then
  MANIFEST="$2"
  { [ -f "$MANIFEST" ] && [ -r "$MANIFEST" ]; } || err "manifest not a readable file: $MANIFEST"
  # Validate ENTIRELY inside jq, with --slurp so a stream of MORE THAN ONE top-level
  # JSON value is rejected (`length != 1`): a two-value manifest like
  # `{"gitSha":"unknown"} {"gitSha":"<expected>"}` must NOT pass just because a later
  # value is valid. The single value's .gitSha must be a string of exactly 40 lowercase
  # hex, anchored \A..\z so a trailing newline or stray character can't slip past shell
  # trailing-newline stripping. Empty/invalid JSON -> length 0 or a parse error -> fail.
  # The manifest is fed via REDIRECTED STDIN, NOT a jq operand: a file literally named
  # `-` (or a dash-leading name) passed as an operand makes jq read stdin / treat it as
  # an option, so an attacker could pipe a valid stream and slip a malformed file
  # named `-` past this fail-closed gate. `< "$MANIFEST"` always reads the file.
  manifest_sha="$(jq -er --slurp 'if length != 1 then error("manifest must contain exactly one JSON value") else .[0].gitSha | select(type == "string") | select(test("\\A[0-9a-f]{40}\\z")) end' < "$MANIFEST" 2>/dev/null)" \
    || err "manifest $MANIFEST must be a single JSON object whose .gitSha is full lowercase 40-hex"
  [ "$manifest_sha" = "$DEPLOYED" ] || err "manifest .gitSha ($manifest_sha) != deployed sha ($DEPLOYED)"
  printf 'qa-round-deployed-sha-assert: OK checkout + manifest pinned to %s\n' "$DEPLOYED"
else
  printf 'qa-round-deployed-sha-assert: OK checkout pinned to %s\n' "$DEPLOYED"
fi
exit 0
