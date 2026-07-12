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
# TWO MODES (mirrors staging-reset.sh):
#   default (ssh)  Run from a dev Mac against STAGING_HOST (required — env or gitignored root .env).
#                  There is no repo checkout on the host, so this ships the
#                  installer + the three source scripts to a temp dir on the host
#                  and re-invokes itself there with --local. Uses `ssh -t` so sudo
#                  can prompt for a password interactively; the temp dir is removed
#                  afterward. The source of truth is THIS local checkout.
#   --local        Run on the staging host itself, as root, from a repo checkout
#                  (the ssh mode calls this on the far side). Does the real install
#                  + sudoers work described above.
#
# Usage:
#   # from a dev Mac (default): provisions STAGING_HOST over ssh
#   ./scripts/admin/setup-staging-reseed-host.sh
#   STAGING_HOST=my-staging RUNNER_USER=my-runner ./scripts/admin/setup-staging-reseed-host.sh
#   # on the staging host, from a checkout, as root:
#   sudo ./scripts/admin/setup-staging-reseed-host.sh --local
#   sudo RUNNER_USER=my-runner ./scripts/admin/setup-staging-reseed-host.sh --local

set -euo pipefail

# --- Args / mode ---------------------------------------------------------------
LOCAL=false
RUNNER_USER_FLAG=""
while [ $# -gt 0 ]; do
    case "$1" in
        --local) LOCAL=true; shift ;;
        --runner-user)
            [ $# -ge 2 ] || { echo "setup-staging-reseed-host: --runner-user needs a value" >&2; exit 2; }
            RUNNER_USER_FLAG="$2"; shift 2 ;;
        *) echo "setup-staging-reseed-host: unknown argument: $1" >&2; exit 2 ;;
    esac
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Runner account the sudoers capability is granted to. Precedence: --runner-user
# flag (how ssh mode threads it through sudo, which resets env) > RUNNER_USER env >
# default. Overridable in case the staging runner account name differs from prod.
RUNNER_USER="${RUNNER_USER_FLAG:-${RUNNER_USER:-gha-runner}}"
# Staging host targeted in ssh mode.
# The host alias is deliberately NOT committed (privacy rule: no hostnames in
# tracked artifacts) — resolve from the env, else the gitignored root .env.
STAGING_HOST="${STAGING_HOST:-}"
if [ -z "$STAGING_HOST" ] && [ -r "$REPO_ROOT/.env" ]; then
    STAGING_HOST="$(sed -n 's/^STAGING_HOST=//p' "$REPO_ROOT/.env" | head -1)"
fi
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

# --- SSH mode (default): bootstrap onto the host, then run --local there --------
# There is no repo checkout on the staging host, so ship the installer + the three
# source scripts over ssh and re-invoke this script with --local on the far side.
# All the real install/sudoers logic lives in the --local path below (one source of
# truth); this branch is purely the transport.
if [ "$LOCAL" != true ]; then
    command -v ssh >/dev/null 2>&1 || die "'ssh' not found on PATH"
    command -v tar >/dev/null 2>&1 || die "'tar' not found on PATH"

    # Validate the local sources BEFORE touching the host (friendlier than a remote
    # failure). The installer itself + the three scripts it installs.
    [ -r "$REPO_ROOT/scripts/admin/setup-staging-reseed-host.sh" ] \
        || die "installer not found in this checkout: $REPO_ROOT/scripts/admin/setup-staging-reseed-host.sh"
    for name in "${INSTALL_SCRIPTS[@]}"; do
        [ -r "$REPO_ROOT/scripts/$name" ] || die "source script not found or unreadable: $REPO_ROOT/scripts/$name"
    done

    [ -n "$STAGING_HOST" ] \
        || die "STAGING_HOST is not set — export it, or add STAGING_HOST=<host> to the gitignored root .env (the alias is deliberately not committed)"
    if ! ssh -q -o ConnectTimeout=5 "$STAGING_HOST" exit; then
        die "cannot reach STAGING_HOST '$STAGING_HOST' over ssh (set STAGING_HOST=... to override)"
    fi

    echo "Provisioning '$STAGING_HOST' over ssh (installer runs there as root; sudo may prompt)..."

    # 1. Ship installer + sources to a fresh 0700 temp dir on the host; capture it.
    #    Non-TTY ssh here so the tarball can stream on stdin.
    remote_dir="$(tar czf - -C "$REPO_ROOT" \
        scripts/admin/setup-staging-reseed-host.sh \
        scripts/staging-reset.sh scripts/staging-reseed.sh scripts/staging-deployed-sha.sh \
        | ssh "$STAGING_HOST" 'd="$(mktemp -d /tmp/crm-reseed-provision.XXXXXX)" && tar xzf - -C "$d" && printf %s "$d"')" \
        || die "failed to ship the provisioning bundle to '$STAGING_HOST'"
    [ -n "$remote_dir" ] || die "could not create a staging temp dir on '$STAGING_HOST'"

    # 2. Run the installer on the host as root, then remove the temp dir. `-t`
    #    allocates a TTY so sudo can prompt. RUNNER_USER is passed as a --local
    #    FLAG (args survive sudo; env does not) — no SETENV needed. printf %q keeps
    #    the interpolated values shell-safe in the remote command string.
    remote_installer="$remote_dir/scripts/admin/setup-staging-reseed-host.sh"
    remote_cmd="sudo bash $(printf %q "$remote_installer") --local --runner-user $(printf %q "$RUNNER_USER"); rc=\$?; rm -rf $(printf %q "$remote_dir"); exit \$rc"
    ssh -t "$STAGING_HOST" "$remote_cmd"
    exit $?
fi

# --- LOCAL mode: install on THIS host (must be root) ----------------------------

[ "$(id -u)" -eq 0 ] || die "must run as root — re-run: sudo RUNNER_USER=$RUNNER_USER scripts/admin/setup-staging-reseed-host.sh --local"

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
