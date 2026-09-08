#!/bin/bash
set -euo pipefail

# Freshness is checked before PostgreSQL work so an expired remote cannot
# trigger a restore into the live server.

if [ "$#" -ne 0 ]; then
    echo "usage: verify-offsite-backup.sh" >&2
    exit 2
fi

require_value() {
    local name="$1"
    if [ -z "${!name:-}" ]; then
        echo "verify error: $name is required" >&2
        exit 2
    fi
}

require_command() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "verify error: required command '$1' was not found" >&2
        exit 2
    fi
}

require_value BACKUP_REMOTE
require_value BACKUP_BUCKET
AGE_IDENTITY_FILE="${AGE_IDENTITY_FILE:-/var/lib/personalcrm/.config/personalcrm-backup/age.key}"
require_value AGE_IDENTITY_FILE

BACKUP_REMOTE_PATH="$BACKUP_REMOTE:$BACKUP_BUCKET"
PG_USER="${PG_USER:-crm_user}"
PG_DB="${PG_DB:-personal_crm}"
PG_EXEC="${PG_EXEC:-podman exec -i crm-postgres}"
VERIFY_DB="${VERIFY_DB:-personal_crm_verify}"
MAX_AGE_HOURS="${BACKUP_MAX_AGE_HOURS:-36}"
TOLERANCE_PCT="${VERIFY_CONTACT_TOLERANCE_PCT:-5}"

for command_name in age date rclone zstd; do
    require_command "$command_name"
done

if ! [[ "$MAX_AGE_HOURS" =~ ^[0-9]+$ ]]; then
    echo "verify error: BACKUP_MAX_AGE_HOURS must be a non-negative integer" >&2
    exit 2
fi
if ! [[ "$TOLERANCE_PCT" =~ ^[0-9]+$ ]]; then
    echo "verify error: VERIFY_CONTACT_TOLERANCE_PCT must be a non-negative integer" >&2
    exit 2
fi

if ! [[ "$PG_DB" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || \
    ! [[ "$VERIFY_DB" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
    echo "verify error: database names must be simple PostgreSQL identifiers" >&2
    exit 2
fi

parse_utc_timestamp() {
    local compact="$1" iso parsed

    # GNU date rejects the compact YYYYMMDDTHHMMSSZ object-name form, so expand
    # it to ISO 8601 first; both GNU (-d) and BSD (-j -f) accept that. A result
    # counts only when it is a plain epoch integer.
    iso="${compact:0:4}-${compact:4:2}-${compact:6:2}T${compact:9:2}:${compact:11:2}:${compact:13:2}Z"

    parsed="$(date -u -d "$iso" +%s 2>/dev/null || true)"
    if [[ "$parsed" =~ ^[0-9]+$ ]]; then
        printf '%s\n' "$parsed"
        return 0
    fi

    parsed="$(date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$iso" +%s 2>/dev/null || true)"
    if [[ "$parsed" =~ ^[0-9]+$ ]]; then
        printf '%s\n' "$parsed"
        return 0
    fi
    return 1
}

if ! latest_object="$(rclone lsf "$BACKUP_REMOTE_PATH" --files-only --include 'personal_crm-*.sql.zst.age' \
    | LC_ALL=C sort | tail -n 1)"; then
    echo "verify error: could not list backup objects" >&2
    exit 1
fi
if [ -z "$latest_object" ]; then
    echo "verify error: no database backup objects found" >&2
    exit 1
fi
if ! [[ "$latest_object" =~ ^personal_crm-([0-9]{8}T[0-9]{6}Z)\.sql\.zst\.age$ ]]; then
    echo "verify error: newest backup has an invalid name: $latest_object" >&2
    exit 1
fi

if ! backup_epoch="$(parse_utc_timestamp "${BASH_REMATCH[1]}")"; then
    echo "verify error: cannot parse backup timestamp in '$latest_object'" >&2
    exit 1
fi
if ! now_epoch="$(date -u +%s)" || ! [[ "$now_epoch" =~ ^[0-9]+$ ]]; then
    echo "verify error: cannot read the current UTC time" >&2
    exit 1
fi

age_seconds=$((now_epoch - backup_epoch))
if [ "$age_seconds" -lt 0 ]; then
    age_seconds=0
fi
max_age_seconds=$((MAX_AGE_HOURS * 3600))
age_hours=$((age_seconds / 3600))
if [ "$age_seconds" -gt "$max_age_seconds" ]; then
    echo "verify error: newest backup '$latest_object' is stale (${age_hours} hours old)" >&2
    exit 1
fi

cleanup_verify_db() {
    local rc=$?
    # The scratch database must not survive a failed restore or assertion.
    # shellcheck disable=SC2086
    $PG_EXEC psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -qAtc \
        "DROP DATABASE IF EXISTS $VERIFY_DB" >/dev/null 2>&1 || true
    exit "$rc"
}
trap cleanup_verify_db EXIT

# shellcheck disable=SC2086
$PG_EXEC psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -qAtc \
    "DROP DATABASE IF EXISTS $VERIFY_DB"
# shellcheck disable=SC2086
$PG_EXEC psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -qAtc \
    "CREATE DATABASE $VERIFY_DB"

# shellcheck disable=SC2086
if ! rclone cat "$BACKUP_REMOTE_PATH/$latest_object" \
    | age -d -i "$AGE_IDENTITY_FILE" \
    | zstd -d -q \
    | $PG_EXEC psql -U "$PG_USER" -d "$VERIFY_DB" -v ON_ERROR_STOP=1 -q; then
    echo "verify error: restore pipeline failed for '$latest_object'" >&2
    exit 1
fi

# shellcheck disable=SC2086
if ! live_version="$($PG_EXEC psql -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 -qAtc \
    'SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1')"; then
    echo "verify error: could not read the live migration version" >&2
    exit 1
fi
# shellcheck disable=SC2086
if ! restored_version="$($PG_EXEC psql -U "$PG_USER" -d "$VERIFY_DB" -v ON_ERROR_STOP=1 -qAtc \
    'SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1')"; then
    echo "verify error: could not read the restored migration version" >&2
    exit 1
fi
if [ -z "$live_version" ] || [ -z "$restored_version" ]; then
    echo "verify error: migration version query returned an empty value" >&2
    exit 1
fi
if [ "$live_version" != "$restored_version" ]; then
    echo "verify error: migration version mismatch: live=$live_version restored=$restored_version" >&2
    exit 1
fi

# shellcheck disable=SC2086
if ! live_contacts="$($PG_EXEC psql -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 -qAtc \
    'SELECT count(*) FROM contact WHERE deleted_at IS NULL')"; then
    echo "verify error: could not read the live contact count" >&2
    exit 1
fi
# shellcheck disable=SC2086
if ! restored_contacts="$($PG_EXEC psql -U "$PG_USER" -d "$VERIFY_DB" -v ON_ERROR_STOP=1 -qAtc \
    'SELECT count(*) FROM contact WHERE deleted_at IS NULL')"; then
    echo "verify error: could not read the restored contact count" >&2
    exit 1
fi
if ! [[ "$live_contacts" =~ ^[0-9]+$ ]] || ! [[ "$restored_contacts" =~ ^[0-9]+$ ]]; then
    echo "verify error: contact count query returned a non-integer value" >&2
    exit 1
fi
if [ "$restored_contacts" -le 0 ]; then
    echo "verify error: restored contact count must be greater than zero (got $restored_contacts)" >&2
    exit 1
fi
if [ "$live_contacts" -le 0 ]; then
    echo "verify error: live contact count must be greater than zero (got $live_contacts)" >&2
    exit 1
fi

contact_delta=$((live_contacts - restored_contacts))
if [ "$contact_delta" -lt 0 ]; then
    contact_delta=$((-contact_delta))
fi
if [ $((contact_delta * 100)) -gt $((live_contacts * TOLERANCE_PCT)) ]; then
    echo "verify error: contact count out of tolerance: live=$live_contacts restored=$restored_contacts tolerance=${TOLERANCE_PCT}%" >&2
    exit 1
fi

printf 'verified %s age_hours=%s migration_version=%s live_contacts=%s restored_contacts=%s\n' \
    "$latest_object" "$age_hours" "$live_version" "$live_contacts" "$restored_contacts"
