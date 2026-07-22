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
#   - the tip PLUS the BUFFER most recent promoted ancestors (the rollback window);
#   - every UN-promoted develop SHA (ahead of main) — un-promoted work is kept;
#   - :latest, and any untagged version (untagged versions may back a manifest, so
#     they are never touched here).
#
# SCOPE: this bounds TAG count, not total storage. Deleting a tagged buildx
# manifest-index can leave its child/attestation manifests as UNTAGGED orphans,
# which this script intentionally never touches (safety). An untagged-orphan sweep
# is a separate follow-up.
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
#   BUFFER     rollback-window commits kept behind main tip (default: 30; must be a
#              non-negative integer — anything else fails closed and deletes nothing)
#   MAIN_REF   ref for the prod tip  (default: origin/main; NO fallback — a ref that
#              does not resolve deletes nothing, so a stale local `main` can't drive it)
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

# BUFFER feeds `$((BUFFER + 1))` arithmetic below; a non-numeric value there would
# resolve to 1 (bash treats an unset name as 0) and collapse the rollback window to
# the tip alone. Reject anything that is not a non-negative integer -> delete nothing.
case "$BUFFER" in
  ''|*[!0-9]*)
    log "ghcr-retention: BUFFER='${BUFFER}' is not a non-negative integer; deleting nothing"
    exit 0 ;;
esac

DELSET="$(mktemp)"
LISTFILE="$(mktemp)"
cleanup() { rm -f "$DELSET" "$LISTFILE"; }
trap cleanup EXIT

# Resolve the prod (main) tip; fail-closed if the ref does not resolve. There is NO
# fallback to a bare local `main`: a stale or divergent local main must never drive
# deletions. The caller passes a fetched remote ref (origin/main) and the workflow
# fails the job if that fetch fails, so an unresolved ref here means "delete nothing".
main_tip="$(git rev-parse --verify "${MAIN_REF}^{commit}" 2>/dev/null)" \
  || { log "ghcr-retention: cannot resolve main tip (${MAIN_REF}); deleting nothing"; exit 0; }

# Keep the tip plus the BUFFER most recent promoted ancestors (the rollback window);
# delete only ancestors STRICTLY MORE THAN BUFFER commits behind the tip. The floor
# is the first deletable commit (BUFFER+1 behind); rev-list from it covers it and
# every older ancestor. If main has <= BUFFER commits of history behind its tip,
# nothing is old enough -> keep everything.
floor="$(git rev-parse --verify "${main_tip}~$((BUFFER + 1))^{commit}" 2>/dev/null)" || {
  log "ghcr-retention: main has <= ${BUFFER} commits behind its tip; nothing eligible, deleting nothing"
  exit 0
}

# Deletable SHAs = the floor commit and all its ancestors (promoted AND strictly
# more than BUFFER commits behind the tip). Full 40-hex, one per line.
git rev-list "$floor" > "$DELSET" 2>/dev/null || {
  log "ghcr-retention: git rev-list failed; deleting nothing"; exit 0
}

# SAFETY (defense-in-depth): the current prod image must NEVER be in the delete set.
# The floor math (tip~(BUFFER+1)) makes this unreachable — the floor is always a
# strict ancestor of the tip — but assert it anyway so any future refactor of the
# floor/rev-list logic that reintroduces the tip fails LOUD instead of deleting prod.
if grep -Fxq "$main_tip" "$DELSET"; then
  log "ghcr-retention: SAFETY ABORT — main tip ${main_tip} is in the delete set; deleting nothing"
  exit 1
fi

count="$(grep -c . "$DELSET" 2>/dev/null)"
count="${count:-0}"
if [ "$count" -eq 0 ]; then
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
list_failed=0
for pkg in $PACKAGES; do
  log "== package: ${pkg}"
  # Capture the listing to a file and CHECK its exit status. A process-substitution
  # `while ... < <(list_versions)` would swallow a list/API/auth/pagination failure
  # and silently report "0 deleted"; capturing surfaces it (and we exit non-zero at
  # the end so a scheduled run goes red instead of looking like a clean no-op).
  if ! list_versions "$pkg" > "$LISTFILE" 2>/dev/null; then
    log "  ERROR: could not list versions for ${pkg} (API/auth/pagination failure); skipping this package"
    list_failed=1
    continue
  fi
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
  done < "$LISTFILE"
done

if [ "$DRY_RUN" = "1" ]; then
  log "ghcr-retention: ${total} version(s) would be deleted (dry run)"
else
  log "ghcr-retention: ${total} version(s) deleted"
fi

# Surface any listing failure as a non-zero exit so a scheduled run goes red rather
# than masquerading as a clean no-op (deletions that DID run above still stand).
if [ "$list_failed" -eq 1 ]; then
  log "ghcr-retention: one or more package listings failed; exiting non-zero to surface it"
  exit 1
fi
