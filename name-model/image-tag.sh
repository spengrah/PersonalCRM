#!/usr/bin/env bash
# image-tag.sh [<revision>]: the name model image's tag for a revision
# (default HEAD): "src-" plus the git tree hash of name-model/.
#
# name-model/ holds every source of the image (its Dockerfile, code, pinned
# requirements and units), so the tag changes exactly when one of them does.
# build-images.yml builds and publishes the image under this tag only when the
# registry lacks it, and stamps it on the backend image of every revision as the
# label org.personalcrm.name-model.tag, which scripts/deploy-artifact.sh reads.
# The "src-" prefix keeps the tag from ever reading as a commit SHA, so
# scripts/ci/ghcr-retention.sh, which deletes only tags equal to old commit
# SHAs, never deletes a model tag.
set -euo pipefail
tree="$(git rev-parse --verify "${1:-HEAD}:name-model")"
echo "src-$tree"
