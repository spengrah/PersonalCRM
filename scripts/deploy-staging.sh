#!/bin/bash
# Root-owned staging deploy wrapper — the env-trust seam for develop->staging.
#
# The self-hosted staging runner invokes this through a single sudoers entry
# (`sudo /usr/local/sbin/deploy-staging.sh "$SHA"`). sudo resets the environment
# by default, so the ONLY tenant identity that reaches the root-run deploy is what
# this runner-immutable wrapper hardcodes below — NOT anything the workflow could
# set. Everything else (QUADLET_DIR, BACKEND_REPO, ENV_FILE, BACKUP_SCRIPT,
# RESTORE_SCRIPT, NTFY_ENV_FILE, ...) takes deploy-artifact.sh's prod-correct
# default, which is right because staging reuses prod's paths/repos/network
# (DECISION A). For this seam to hold: the sudoers entry must NOT use
# SETENV/env_keep, and the workflow must NEVER use sudo -E / --preserve-env.
set -euo pipefail

export CRM_USER=staging
export CRM_HOME=/var/lib/staging

# "${1:-}" (not "$1"): with set -u a bare "$1" would abort on a no-arg invocation
# BEFORE exec; the default-empty expansion forwards an empty arg to
# deploy-artifact.sh, which owns SHA validation (exit 2 on a bad/empty arg). The
# trust boundary is "this exact root-owned script," so the wrapper does not
# re-validate the SHA itself.
exec /usr/local/sbin/deploy-artifact.sh "${1:-}"
