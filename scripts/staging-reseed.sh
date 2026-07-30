#!/bin/bash
# staging-reseed.sh — root-owned auto-reseed wrapper (the env-trust seam).
#
# deploy-staging.yml's deploy job invokes this through a single args-free sudoers
# entry (`sudo /usr/local/sbin/staging-reseed.sh`). sudo resets the environment, so
# the ONLY tenant identity + reseed flags that reach the root-run reset are what
# this runner-immutable wrapper hardcodes below — NOT anything the workflow could
# set. Mirrors deploy-staging.sh exactly. For this seam to hold: the sudoers entry
# must NOT use SETENV/env_keep, and the workflow must NEVER use sudo -E.
#
# --require-oauth-empty is pinned HERE (not workflow-controllable): the auto path
# always carries the oauth guard, so a connected sync account (non-empty
# oauth_credential) disables the destructive wipe and staging keeps its data.
# Manual `make staging-reset` runs staging-reset.sh WITHOUT the flag (operator
# force = full wipe regardless of oauth).
#
# STAGING_RESET_PROFILE is pinned here too, for the same reason the tenant
# identity is: which WORLD the automated reseed builds belongs inside the
# runner-immutable trust seam, not to an ambient default that a caller could
# shift or a future default flip could change under us. staging-reset.sh's own
# default is the same value; stating it here makes the automated path's world
# selection an explicit property of the wrapper.
set -euo pipefail

export CRM_USER=staging
export CRM_HOME=/var/lib/staging
export STAGING_RESET_PROFILE=standard

exec /usr/local/sbin/staging-reset.sh --local --require-oauth-empty
