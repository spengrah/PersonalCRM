#!/bin/bash
set -euo pipefail

# The stream stays off local storage so a successful run leaves only encrypted
# objects in the remote and a failed run cannot leave plaintext behind.

if [ "$#" -ne 0 ]; then
    echo "usage: backup-offsite.sh" >&2
    exit 2
fi

require_value() {
    local name="$1"
    if [ -z "${!name:-}" ]; then
        echo "backup error: $name is required" >&2
        exit 2
    fi
}

require_command() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "backup error: required command '$1' was not found" >&2
        exit 2
    fi
}

require_value BACKUP_REMOTE
require_value BACKUP_BUCKET
require_value AGE_RECIPIENT

BACKUP_REMOTE_PATH="$BACKUP_REMOTE:$BACKUP_BUCKET"
ZSTD_LEVEL_VALUE="${ZSTD_LEVEL:-3}"
PG_USER="${PG_USER:-crm_user}"
PG_DB="${PG_DB:-personal_crm}"
PG_EXEC="${PG_EXEC:-podman exec -i crm-postgres}"

for command_name in date age rclone zstd; do
    require_command "$command_name"
done

if ! [[ "$ZSTD_LEVEL_VALUE" =~ ^[0-9]+$ ]] || [ "$ZSTD_LEVEL_VALUE" -lt 1 ] || [ "$ZSTD_LEVEL_VALUE" -gt 22 ]; then
    echo "backup error: ZSTD_LEVEL must be an integer from 1 to 22" >&2
    exit 2
fi

TS="$(date -u +%Y%m%dT%H%M%SZ)"
DB_OBJECT="personal_crm-$TS.sql.zst.age"
ENV_OBJECT="personalcrm-env-$TS.age"

delete_partial() {
    local object="$1"
    # B2 hides failed uploads, keeping a truncated name out of the verifier's
    # normal listing while preserving the provider's recovery semantics.
    rclone deletefile "$BACKUP_REMOTE_PATH/$object" >/dev/null 2>&1 || true
}

object_size() {
    local object="$1" line size name
    if ! line="$(rclone lsf "$BACKUP_REMOTE_PATH" --files-only --include "$object" --format sp 2>/dev/null)"; then
        return 1
    fi
    IFS=';' read -r size name <<< "$line"
    if ! [[ "$size" =~ ^[0-9]+$ ]] || [ "$size" -le 0 ] || [ "$name" != "$object" ]; then
        return 1
    fi
    printf '%s\n' "$size"
}

confirm_object() {
    local object="$1" size
    if ! size="$(object_size "$object")"; then
        echo "backup error: uploaded object '$object' is missing or empty" >&2
        return 1
    fi
    printf '%s %s bytes\n' "$object" "$size"
}

upload_database() {
    # PG_EXEC is a configurable word-split command prefix so Podman's exec
    # arguments and the test database stub can share the same pipeline.
    # shellcheck disable=SC2086
    if ! $PG_EXEC pg_dump -U "$PG_USER" -Fp "$PG_DB" \
        | zstd -q -"$ZSTD_LEVEL_VALUE" \
        | age -r "$AGE_RECIPIENT" \
        | rclone rcat "$BACKUP_REMOTE_PATH/$DB_OBJECT"; then
        delete_partial "$DB_OBJECT"
        echo "backup error: database backup pipeline failed" >&2
        return 1
    fi
    if ! confirm_object "$DB_OBJECT"; then
        delete_partial "$DB_OBJECT"
        return 1
    fi
}

upload_environment() {
    local env_file="${CRM_ENV_FILE:-/srv/personalcrm/.env}"
    if [ ! -r "$env_file" ]; then
        echo "backup error: CRM_ENV_FILE is not readable: $env_file" >&2
        return 1
    fi

    if ! age -r "$AGE_RECIPIENT" < "$env_file" \
        | rclone rcat "$BACKUP_REMOTE_PATH/$ENV_OBJECT"; then
        delete_partial "$ENV_OBJECT"
        echo "backup error: environment backup pipeline failed" >&2
        return 1
    fi
    if ! confirm_object "$ENV_OBJECT"; then
        delete_partial "$ENV_OBJECT"
        return 1
    fi
}

upload_database
upload_environment
