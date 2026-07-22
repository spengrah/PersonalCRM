#!/usr/bin/env bash
# ghcr-retention.sh — prune promoted-and-superseded GHCR image tags, safely.
#
# Bounds the growth of the personalcrm-{backend,frontend} GHCR packages (one
# :<sha> tag per develop push, otherwise unbounded) WITHOUT ever deleting an image
# prod is running or could roll back to.
#
# A :<sha> image version is deleted ONLY when ALL hold:
#   1. its SHA is an ANCESTOR of the current `main` tip — i.e. it was promoted AND
#      main has since advanced past it ("superseded by a more recent promotion"),
#   2. it is more than BUFFER commits behind main's tip (prod rollback window), and
#   3. it is neither :latest nor untagged.
#
# NEVER deleted (matches the operator rule "never clear a tag that hasn't been
# promoted to main or superseded by a more recent one"):
#   - the current prod image (main tip), REGARDLESS of age -> a long promote gap
#     (e.g. no promote for 2 weeks) never deletes what prod runs / rolls back to;
#   - the last BUFFER promoted commits (rollback window);
#   - every UN-promoted develop SHA (ahead of main) — un-promoted work is kept;
#   - :latest, and any untagged version (untagged versions may back a manifest, so
#     they are never touched here).
#
# main is a strict linear prefix of develop (fast-forward promote model), so every
# SHA is either an ancestor of main (promoted) or ahead of it (un-promoted); the
# ancestry test cleanly separates the two. The build tags images with the FULL
# 40-hex github.sha, matching `git rev-list` output verbatim.
#
# FAIL-CLOSED: any inability to resolve main / compute the delete set deletes
# NOTHING and exits 0 (a real run also refuses without a token). Deletion is
# irreversible; uncertainty must never widen it.
#
# Env:
#   OWNER      GHCR owner            (default: spengrah)
#   PACKAGES   space-separated pkgs  (default: personalcrm-backend personalcrm-frontend)
#   BUFFER     rollback-window commits kept behind main tip (default: 30)
#   MAIN_REF   ref for the prod tip  (default: origin/main; falls back to main)
#   DRY_RUN    "1" -> log intended deletes, delete nothing (default: 0)
#   GH_TOKEN   PAT with read:packages + delete:packages (required for a real run)
#   GHCR_LIST_VERSIONS_CMD / GHCR_DELETE_VERSION_CMD  test-injection hooks (see below)
#
# Bash 3.2-safe (no associative arrays): the delete set is a temp file matched with
# `grep -Fxq`, so it runs on the macOS system bash the deploy-script tests use.

set -uo pipefail

OWNER="${OWNER:-spengrah}"
PACKAGES="${PACKAGES:-personalcrm-backend personalcrm-frontend}"
BUFFER="${BUFFER:-30}"
MAIN_REF="${MAIN_REF:-origin/main}"
DRY_RUN="${DRY_RUN:-0}"

log() { printf '%s\n' "$*" >&2; }

DELSET="$(mktemp)"
cleanup() { rm -f "$DELSET"; }
trap cleanup EXIT

# Resolve the prod (main) tip; fail-closed if neither ref resolves.
main_tip="$(git rev-parse --verify "${MAIN_REF}^{commit}" 2>/dev/null)" \
  || main_tip="$(git rev-parse --verify "main^{commit}" 2>/dev/null)" \
  || { log "ghcr-retention: cannot resolve main tip (${MAIN_REF} / main); deleting nothing"; exit 0; }

# The delete floor is BUFFER commits behind the tip. If main has <= BUFFER commits
# of history, nothing is old enough to delete -> keep everything.
floor="$(git rev-parse --verify "${main_tip}~${BUFFER}^{commit}" 2>/dev/null)" || {
  log "ghcr-retention: main has <= ${BUFFER} commits behind its tip; nothing eligible, deleting nothing"
  exit 0
}

# Deletable SHAs = the floor commit and all its ancestors (promoted AND strictly
# more than BUFFER commits behind the tip). Full 40-hex, one per line.
git rev-list "$floor" > "$DELSET" 2>/dev/null || {
  log "ghcr-retention: git rev-list failed; deleting nothing"; exit 0
}

# SAFETY: the current prod image must NEVER be in the delete set. If it is, abort
# loudly rather than risk deleting what prod is running.
if grep -Fxq "$main_tip" "$DELSET"; then
  log "ghcr-retention: SAFETY ABORT — main tip ${main_tip} is in the delete set; deleting nothing"
  exit 1
fi

count="$(grep -c . "$DELSET" 2>/dev/null || echo 0)"
if [ "${count:-0}" -eq 0 ]; then
  log "ghcr-retention: empty delete set; nothing to do"
  exit 0
fi

if [ "$DRY_RUN" != "1" ] && [ -z "${GH_TOKEN:-}" ]; then
  # Not-yet-configured secret -> green no-op (deletes nothing), not a weekly red.
  # Set the GHCR_CLEANUP_TOKEN PAT secret (read:packages + delete:packages) to enable.
  log "ghcr-retention: GH_TOKEN/GHCR_CLEANUP_TOKEN not set; skipping (configure the PAT secret to enable). Deleting nothing."
  exit 0
fi

log "ghcr-retention: owner=${OWNER} main_tip=${main_tip} buffer=${BUFFER} deletable-ancestor-SHAs=${count} dry_run=${DRY_RUN}"

is_deletable() { grep -Fxq "$1" "$DELSET"; }

list_versions() {  # <pkg> -> lines: "<id>\t<tag,tag,...>"  (untagged -> empty tag field)
  local pkg="$1"
  if [ -n "${GHCR_LIST_VERSIONS_CMD:-}" ]; then
    PKG="$pkg" bash -c "$GHCR_LIST_VERSIONS_CMD"
    return
  fi
  gh api --paginate "/users/${OWNER}/packages/container/${pkg}/versions" \
    --jq '.[] | [(.id|tostring), (.metadata.container.tags | join(","))] | @tsv'
}

delete_version() {  # <pkg> <id>
  local pkg="$1" id="$2"
  if [ -n "${GHCR_DELETE_VERSION_CMD:-}" ]; then
    PKG="$pkg" VERSION_ID="$id" bash -c "$GHCR_DELETE_VERSION_CMD"
    return
  fi
  gh api -X DELETE "/users/${OWNER}/packages/container/${pkg}/versions/${id}"
}

total=0
for pkg in $PACKAGES; do
  log "== package: ${pkg}"
  while IFS=$'\t' read -r id tags; do
    [ -n "$id" ] || continue
    # Untagged version -> never touch (may back a manifest list / :latest digest).
    [ -n "$tags" ] || continue
    # Delete only if EVERY tag on the version is a deletable SHA. A version that
    # also carries :latest, or any tag not in the delete set, is kept.
    keep=0
    old_ifs="$IFS"; IFS=','
    for t in $tags; do
      if ! is_deletable "$t"; then keep=1; break; fi
    done
    IFS="$old_ifs"
    [ "$keep" -eq 0 ] || continue

    if [ "$DRY_RUN" = "1" ]; then
      log "  DRY-RUN would delete ${pkg} version ${id} (tags: ${tags})"
    else
      log "  deleting ${pkg} version ${id} (tags: ${tags})"
      delete_version "$pkg" "$id" || log "  WARN: delete failed for ${pkg} version ${id}"
    fi
    total=$((total + 1))
  done < <(list_versions "$pkg")
done

if [ "$DRY_RUN" = "1" ]; then
  log "ghcr-retention: ${total} version(s) would be deleted (dry run)"
else
  log "ghcr-retention: ${total} version(s) deleted"
fi
