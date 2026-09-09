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

for script in backup-offsite.sh verify-offsite-backup.sh restore-offsite.sh; do
    install -o "$CRM_USER" -g "$CRM_USER" -m 0755 \
        "$REPO_ROOT/scripts/$script" "$INSTALL_BIN_DIR/$script"
done

for unit in personalcrm-backup.service personalcrm-backup.timer \
    personalcrm-backup-verify.service personalcrm-backup-verify.timer; do
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

run_user_systemctl daemon-reload
run_user_systemctl enable --now personalcrm-backup.timer personalcrm-backup-verify.timer
run_user_systemctl list-timers --all personalcrm-backup.timer personalcrm-backup-verify.timer

echo "Installed offsite backup scripts and user timers for $CRM_USER."
