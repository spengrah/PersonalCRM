#!/bin/bash
# Deploy CRM to the Pi and/or the Mac daemon, but only when their
# respective source paths have changed since the last successful
# deploy.
#
# Usage:
#   ./scripts/deploy-all.sh              # deploy whatever changed
#   ./scripts/deploy-all.sh --force-pi   # always deploy to Pi
#   ./scripts/deploy-all.sh --force-mac  # always deploy Mac daemon
#   ./scripts/deploy-all.sh --force      # both
#
# Change detection:
#   Per-target last-deployed HEAD SHA stored in:
#     .deploy-state/pi.sha
#     .deploy-state/mac.sha
#   For each target, `git diff --quiet <last-sha> -- <paths>` is the
#   "has anything changed?" probe — includes uncommitted working-tree
#   changes so a deploy-from-WIP works as expected.
#
# Paths watched per target:
#   Pi:  backend/  frontend/  infra/  Makefile
#        scripts/deploy.sh  scripts/setup-pi.sh
#   Mac: mac-daemon/
#
# State files are gitignored (.deploy-state/) — wiping the dir
# safely re-deploys everything on the next run.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

STATE_DIR=".deploy-state"
PI_STATE="$STATE_DIR/pi.sha"
MAC_STATE="$STATE_DIR/mac.sha"
PI_PATHS=(backend frontend infra Makefile scripts/deploy.sh scripts/setup-pi.sh)
MAC_PATHS=(mac-daemon)

FORCE_PI=false
FORCE_MAC=false
for arg in "$@"; do
    case $arg in
        --force-pi)  FORCE_PI=true ;;
        --force-mac) FORCE_MAC=true ;;
        --force)     FORCE_PI=true; FORCE_MAC=true ;;
        *) echo "unknown arg: $arg" >&2; exit 1 ;;
    esac
done

mkdir -p "$STATE_DIR"
HEAD_SHA="$(git rev-parse HEAD)"

# Returns 0 (deploy) if state file is missing OR if `git diff` against
# the recorded SHA reports changes on the given paths. Returns 1 (skip)
# only when the diff is clean.
needs_deploy() {
    local state_file="$1"
    shift
    local paths=("$@")
    if [ ! -f "$state_file" ]; then
        return 0
    fi
    local last
    last="$(cat "$state_file")"
    if ! git rev-parse --verify --quiet "$last" >/dev/null; then
        echo "  warning: last-deployed SHA $last is not in this repo (rebased away?). Treating as changed." >&2
        return 0
    fi
    if git diff --quiet "$last" -- "${paths[@]}"; then
        return 1
    else
        return 0
    fi
}

echo "=== Deploy-all (HEAD=$HEAD_SHA) ==="
echo ""

# Pi
if $FORCE_PI || needs_deploy "$PI_STATE" "${PI_PATHS[@]}"; then
    if $FORCE_PI; then
        echo ">>> Deploying to Pi (forced)"
    else
        echo ">>> Deploying to Pi"
    fi
    ./scripts/deploy.sh
    echo "$HEAD_SHA" > "$PI_STATE"
    echo "Pi: deployed at $HEAD_SHA"
    echo ""
else
    echo "Pi:  no changes since $(cat "$PI_STATE") — skipping"
    echo ""
fi

# Mac
if $FORCE_MAC || needs_deploy "$MAC_STATE" "${MAC_PATHS[@]}"; then
    if $FORCE_MAC; then
        echo ">>> Deploying Mac daemon (forced)"
    else
        echo ">>> Deploying Mac daemon"
    fi
    ./scripts/deploy-mac-daemon.sh
    echo "$HEAD_SHA" > "$MAC_STATE"
    echo "Mac: deployed at $HEAD_SHA"
    echo ""
else
    echo "Mac: no changes since $(cat "$MAC_STATE") — skipping"
    echo ""
fi

echo "=== Done ==="
