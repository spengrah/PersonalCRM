#!/bin/bash
# staging-deployed-sha.sh — print the git SHA currently LIVE on the staging tenant.
#
# Reads the staging backend Quadlet unit's pinned Image= line and prints its tag
# IFF that tag is a 40-hex git sha — the steady-state pin written by
# deploy-artifact.sh's post_migrate_swap (BACKEND_REPO:<40-hex-sha>). Anything
# else prints NOTHING and exits non-zero, so the caller degrades to "base unknown"
# (no auto-reseed):
#   - a @sha256:<digest> rollback anchor (64-hex after the last ':', not 40),
#   - the mutable :latest,
#   - an unreadable / missing unit.
# The tag is split on the LAST ':' (`${image##*:}`) — identical to read_image_tag /
# rollback_ref_for in deploy-artifact.sh — so a registry host:port never confuses it.
#
# Root-owned wrapper invoked by deploy-staging.yml's deploy job via a single
# args-free sudoers entry (`sudo /usr/local/sbin/staging-deployed-sha.sh`), read
# BEFORE the deploy swaps the image (the host-pinned diff base). The tenant
# identity is hardcoded here (not workflow-controllable), mirroring
# staging-reseed.sh / deploy-staging.sh; sudo resets the environment, so the
# sudoers entry must NOT use SETENV/env_keep and the workflow must NEVER use
# sudo -E.
set -uo pipefail

CRM_USER="${CRM_USER:-staging}"
CRM_HOME="${CRM_HOME:-/var/lib/staging}"
BACKEND_UNIT="${STAGING_BACKEND_UNIT:-$CRM_HOME/.config/containers/systemd/personalcrm-backend.container}"

# Read the pinned Image= line as the tenant (mirrors staging-reset.sh read_image_ref).
image="$(sudo -u "$CRM_USER" sed -n 's/^Image=//p' "$BACKEND_UNIT" 2>/dev/null | head -1)"
tag="${image##*:}"

if [[ "$tag" =~ ^[0-9a-f]{40}$ ]]; then
    printf '%s\n' "$tag"
    exit 0
fi
exit 1
