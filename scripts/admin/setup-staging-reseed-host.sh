#!/bin/bash
# setup-staging-reseed-host.sh — provision the staging host for auto-reseed.
#
# Installs the three staging reseed scripts to /usr/local/sbin (root:root, 0755)
# and grants the GitHub Actions staging runner the TWO NOPASSWD sudoers lines it
# needs to drive the reseed from deploy-staging.yml:
#   <RUNNER_USER> ALL=(root) NOPASSWD: /usr/local/sbin/staging-reseed.sh
#   <RUNNER_USER> ALL=(root) NOPASSWD: /usr/local/sbin/staging-deployed-sha.sh
# staging-reset.sh is installed too (it is exec'd by staging-reseed.sh, already
# root) but gets NO sudoers line — the runner never sudo-invokes it directly.
#
# Safe, idempotent, fail-loud: install(1) overwrites in place (owner/mode set every
# run, no appends), the sudoers drop-in is a fixed-name file overwritten atomically
# (no duplicate lines), and every precondition failure exits non-zero with an
# actionable message. The sudoers lines carry NO SETENV/env_keep, preserving the
# env-trust seam that pins the tenant identity inside the wrappers (sudo resets env).
#
# CONTEXT: this is STAGING, not the Pi/prod runbook. The runner is a
# `[self-hosted, staging]` agent whose user runs deploy-staging.sh. This script does
# NOT create the runner user or install deploy-staging.sh (part of the earlier
# staging code-deploy standup) — it ASSERTS both and refuses loudly if either is
# missing, so a partial standup is caught before the first seed-touching deploy.
#
# Usage (on the staging host, from a repo checkout):
#   sudo scripts/admin/setup-staging-reseed-host.sh
#   sudo RUNNER_USER=my-runner scripts/admin/setup-staging-reseed-host.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Runner account the sudoers capability is granted to. Overridable in case the
# staging runner account name differs from the prod one.
RUNNER_USER="${RUNNER_USER:-gha-runner}"
# Install destination + sudoers dir. Real defaults; overridable so the test can
# redirect without root. The sudoers LINES always reference the canonical
# /usr/local/sbin path (that is what the runner sudo-invokes at runtime).
USRLOCALSBIN="${USRLOCALSBIN:-/usr/local/sbin}"
SUDOERSD="${SUDOERSD:-/etc/sudoers.d}"

SUDOERS_DROPIN="$SUDOERSD/gha-runner-staging-reseed"
# The three scripts installed to /usr/local/sbin. Only the two wrappers the runner
# sudo-invokes get a sudoers line (the two NOPASSWD lines written below);
# staging-reset.sh does not.
INSTALL_SCRIPTS=(staging-reset.sh staging-reseed.sh staging-deployed-sha.sh)

die() { echo "setup-staging-reseed-host: $*" >&2; exit 1; }

# --- Preconditions (fail-loud) -------------------------------------------------

[ "$(id -u)" -eq 0 ] || die "must run as root — re-run: sudo RUNNER_USER=$RUNNER_USER scripts/admin/setup-staging-reseed-host.sh"

command -v install >/dev/null 2>&1 || die "'install' not found on PATH"
command -v visudo  >/dev/null 2>&1 || die "'visudo' not found on PATH"

if ! id "$RUNNER_USER" >/dev/null 2>&1; then
    die "runner user '$RUNNER_USER' does not exist. Create/confirm the STAGING runner user (default 'gha-runner'; override with RUNNER_USER=...) — the account the [self-hosted, staging] GitHub Actions agent runs as. This is the staging standup, NOT the Pi/prod runbook."
fi

# deploy-staging.sh belongs to the earlier staging code-deploy standup; this script
# does NOT install it. A host missing it is not fully stood up — refuse rather than
# leave a partial setup.
[ -f "$USRLOCALSBIN/deploy-staging.sh" ] || die "$USRLOCALSBIN/deploy-staging.sh is not installed. Complete the staging code-deploy provisioning first (this script only adds the reseed scripts + sudoers)."

for name in "${INSTALL_SCRIPTS[@]}"; do
    src="$REPO_ROOT/scripts/$name"
    [ -r "$src" ] || die "source script not found or unreadable: $src"
done

# --- Install the scripts (idempotent: install overwrites + sets owner/mode) -----

echo "Installing reseed scripts to $USRLOCALSBIN (root:root, 0755)..."
for name in "${INSTALL_SCRIPTS[@]}"; do
    install -o root -g root -m 0755 "$REPO_ROOT/scripts/$name" "$USRLOCALSBIN/$name"
    echo "  installed $name"
done

# --- Sudoers drop-in (idempotent: fixed name, atomic overwrite, validated) ------

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# Two args-free NOPASSWD lines, NO SETENV/env_keep. The paths are the canonical
# runtime locations the runner sudo-invokes from deploy-staging.yml.
{
    printf '%s ALL=(root) NOPASSWD: /usr/local/sbin/staging-reseed.sh\n' "$RUNNER_USER"
    printf '%s ALL=(root) NOPASSWD: /usr/local/sbin/staging-deployed-sha.sh\n' "$RUNNER_USER"
} > "$tmp"
chmod 0440 "$tmp"

echo "Validating sudoers drop-in..."
visudo -cf "$tmp" || die "generated sudoers file failed validation; refusing to install it"

install -o root -g root -m 0440 "$tmp" "$SUDOERS_DROPIN"
echo "  installed $SUDOERS_DROPIN"

echo "Re-validating the installed sudoers drop-in..."
visudo -cf "$SUDOERS_DROPIN" || die "installed sudoers file failed re-validation"

# --- Post-checks (fail-loud, non-destructive) ----------------------------------

echo "Verifying installed scripts..."
for name in "${INSTALL_SCRIPTS[@]}"; do
    dest="$USRLOCALSBIN/$name"
    meta="$(stat -c '%U %G %a' "$dest")" || die "post-check: cannot stat $dest"
    [ "$meta" = "root root 755" ] || die "post-check: $dest has wrong owner/mode '$meta' (want 'root root 755')"
    echo "  ok: $dest ($meta)"
done

echo "Installed sudoers drop-in ($SUDOERS_DROPIN):"
cat "$SUDOERS_DROPIN"

echo "Done. The staging runner ($RUNNER_USER) can now drive the auto-reseed."
