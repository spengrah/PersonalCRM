#!/bin/bash
set -euo pipefail

# The default target is deliberately separate from the live database so an
# operator must name a production target explicitly before overwriting it.

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    echo "usage: restore-offsite.sh <object-name> [target-db]" >&2
    exit 2
fi

require_value() {
    local name="$1"
    if [ -z "${!name:-}" ]; then
        echo "restore error: $name is required" >&2
        exit 2
    fi
}

require_command() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "restore error: required command '$1' was not found" >&2
        exit 2
    fi
}

require_value BACKUP_REMOTE
require_value BACKUP_BUCKET
AGE_IDENTITY_FILE="${AGE_IDENTITY_FILE:-/var/lib/personalcrm/.config/personalcrm-backup/age.key}"
require_value AGE_IDENTITY_FILE

OBJECT_NAME="$1"
TARGET_DB="${2:-personal_crm_restore}"
BACKUP_REMOTE_PATH="$BACKUP_REMOTE:$BACKUP_BUCKET"
PG_USER="${PG_USER:-crm_user}"
PG_EXEC="${PG_EXEC:-podman exec -i crm-postgres}"

for command_name in age rclone zstd; do
    require_command "$command_name"
done

if ! [[ "$OBJECT_NAME" =~ ^personal_crm-[0-9]{8}T[0-9]{6}Z\.sql\.zst\.age$ ]]; then
    echo "restore error: object name must be a timestamped database backup" >&2
    exit 2
fi
if ! [[ "$TARGET_DB" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
    echo "restore error: target database must be a simple PostgreSQL identifier" >&2
    exit 2
fi

# shellcheck disable=SC2086
if ! database_exists="$($PG_EXEC psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -qAtc \
    "SELECT 1 FROM pg_database WHERE datname = '$TARGET_DB'" )"; then
    echo "restore error: could not inspect target database '$TARGET_DB'" >&2
    exit 1
fi
if [ "$database_exists" != "1" ]; then
    # PostgreSQL identifiers are validated above before interpolation into DDL.
    # shellcheck disable=SC2086
    $PG_EXEC psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -qAtc \
        "CREATE DATABASE $TARGET_DB"
fi

# shellcheck disable=SC2086
if ! rclone cat "$BACKUP_REMOTE_PATH/$OBJECT_NAME" \
    | age -d -i "$AGE_IDENTITY_FILE" \
    | zstd -d -q \
    | $PG_EXEC psql -U "$PG_USER" -d "$TARGET_DB" -v ON_ERROR_STOP=1 -q; then
    echo "restore error: restore pipeline failed for '$OBJECT_NAME'" >&2
    exit 1
fi

printf 'restored %s to database %s\n' "$OBJECT_NAME" "$TARGET_DB"
