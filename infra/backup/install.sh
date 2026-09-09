#!/bin/bash
set -euo pipefail

# Installation is root-only because the destination paths and file ownership
# belong to the service tenant, not to the checkout owner.

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this installer as root: sudo ./infra/backup/install.sh" >&2
    exit 1
fi

CRM_USER="${CRM_USER:-crm}"
CRM_HOME="${CRM_HOME:-/var/lib/personalcrm}"
BACKUP_ENV_FILE=/srv/personalcrm/backup.env
# Deliberately not overridable: the notifier resolves this path itself at run
# time, in a different process, so an install-time override would grant access
# to one file while the notifier read another. Keep the two in lockstep.
NTFY_ENV_FILE=/etc/personalcrm/ntfy.env
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_BIN_DIR=/srv/personalcrm/bin
INSTALL_UNIT_DIR="$CRM_HOME/.config/systemd/user"

if [ ! -f "$BACKUP_ENV_FILE" ]; then
    echo "Missing $BACKUP_ENV_FILE. Copy infra/backup/backup.env.example there, fill in its values, and run this installer again." >&2
    exit 1
fi

CRM_UID="$(id -u "$CRM_USER")"
install -d -o "$CRM_USER" -g "$CRM_USER" -m 0755 "$INSTALL_BIN_DIR"
# install -d only chowns the leaf and rewrites owner and mode on directories
# that already exist, so create each missing level under the tenant home and
# leave existing ones alone (a 0700 .config stays 0700).
for dir in "$CRM_HOME/.config" "$CRM_HOME/.config/systemd" "$INSTALL_UNIT_DIR"; do
    [ -d "$dir" ] || install -d -o "$CRM_USER" -g "$CRM_USER" -m 0755 "$dir"
done

for script in backup-offsite.sh verify-offsite-backup.sh restore-offsite.sh notify-unit-failure.sh; do
    install -o "$CRM_USER" -g "$CRM_USER" -m 0755 \
        "$REPO_ROOT/scripts/$script" "$INSTALL_BIN_DIR/$script"
done

for unit in personalcrm-backup.service personalcrm-backup-predeploy.service \
    personalcrm-backup.timer \
    personalcrm-backup-verify.service personalcrm-backup-verify.timer \
    'personalcrm-ntfy-failure@.service'; do
    install -o "$CRM_USER" -g "$CRM_USER" -m 0644 \
        "$REPO_ROOT/infra/backup/$unit" "$INSTALL_UNIT_DIR/$unit"
done

run_user_systemctl() {
    # A root shell may start in a directory the tenant cannot access, while
    # rootless Podman and user systemd expect the tenant's runtime context.
    cd /tmp
    sudo -u "$CRM_USER" HOME="$CRM_HOME" \
        XDG_RUNTIME_DIR="/run/user/$CRM_UID" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$CRM_UID/bus" \
        systemctl --user "$@"
}

# The failure notifier runs as the tenant but the ntfy topic lives in a
# root-owned file shared with the deploy script. Grant the tenant read access
# rather than copying the topic into a second file, and say so out loud when the
# file is missing: silent notifications are indistinguishable from none.
if [ -f "$NTFY_ENV_FILE" ]; then
    # The file's mode alone is not enough: the tenant also has to traverse the
    # directory, which is root-only on this host.
    ntfy_env_dir="$(dirname "$NTFY_ENV_FILE")"
    chgrp "$CRM_USER" "$ntfy_env_dir" "$NTFY_ENV_FILE"
    chmod g+rx "$ntfy_env_dir"
    chmod 0640 "$NTFY_ENV_FILE"
    # Prove it as the tenant instead of trusting the modes above: the notifier
    # degrades open, so an unreadable file would fail silently at 03:30.
    if sudo -u "$CRM_USER" test -r "$NTFY_ENV_FILE"; then
        echo "Granted $CRM_USER read access to $NTFY_ENV_FILE for failure notifications."
    else
        echo "ERROR: $CRM_USER cannot read $NTFY_ENV_FILE, so backup failures would NOT notify. Fix the path's permissions and run this installer again." >&2
        exit 1
    fi
else
    echo "WARNING: $NTFY_ENV_FILE is missing, so backup failures will NOT notify." >&2
    echo "         Create it with NTFY_URL and NTFY_TOPIC, then re-run this installer." >&2
fi

run_user_systemctl daemon-reload
run_user_systemctl enable --now personalcrm-backup.timer personalcrm-backup-verify.timer
run_user_systemctl list-timers --all personalcrm-backup.timer personalcrm-backup-verify.timer

echo "Installed offsite backup scripts and user timers for $CRM_USER."
