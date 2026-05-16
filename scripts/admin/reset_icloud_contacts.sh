#!/bin/bash
# scripts/admin/reset_icloud_contacts.sh
#
# Destructive operator script for the v1 Mac-daemon re-pair limitation.
# Hard-deletes every iCloud Contacts external_contact row and every
# iCloud Contacts event-log row so a newly-paired Mac can resync
# everything from scratch.
#
# Background:
# external_contact.host_id is set on first INSERT only (preserves the
# original paired host's ownership across content updates). When a new
# Mac pairs onto the same Pi, the previous host's rows survive with
# their original host_id and the new host's GET /known-ids returns
# empty. The new host's full CNContactStore scan emits upsert events
# that dedup-absorb at the event-log layer ((source, source_id)
# uniqueness), so the new host appears not to see its own contacts.
#
# This script is the operator workaround. It is intentionally NOT
# wired into RevokeHost — the append-only event-log invariant
# (event.go) takes precedence in production code, and the upsert state
# on external_contact rows preserves user decisions
# (crm_contact_id, match_status). Running this script destroys both,
# so the operator must explicitly confirm.
#
# What gets deleted:
#   - Every external_contact row with source = 'icloud_contacts'
#     (live AND tombstoned). User-curated match state (crm_contact_id,
#     match_status='imported'/'ignored') is destroyed.
#   - Every event row with source = 'icloud_contacts'. Cursor history
#     for the iCloud Contacts source is destroyed; the daemon's next
#     sync will start from scratch.
#
# What is NOT touched:
#   - The mac_host table. Re-pair / revoke flows are unchanged.
#   - Other sources (gcontacts, gcal_attendee, telegram, messages).
#     The DELETE statements are tightly scoped to source = 'icloud_contacts'.
#   - identity rows. The daemon's full resync will refresh identities
#     via the normal external_contact.upserted handler.
#
# Usage:
#   PI_HOST=raspberry-pi ./scripts/admin/reset_icloud_contacts.sh
#
# Or, if invoked from the Pi itself (no SSH hop):
#   LOCAL=1 ./scripts/admin/reset_icloud_contacts.sh

set -e

PI_HOST="${PI_HOST:-raspberry-pi}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-crm-postgres}"
POSTGRES_USER="${POSTGRES_USER:-crm_user}"
POSTGRES_DB="${POSTGRES_DB:-personal_crm}"
LOCAL="${LOCAL:-0}"

print_banner() {
    echo "================================================================"
    echo "  Mac-daemon iCloud Contacts state reset"
    echo "================================================================"
    echo ""
    echo "This will HARD-DELETE all iCloud Contacts state on the Pi:"
    echo "  - external_contact rows where source = 'icloud_contacts'"
    echo "  - event rows where source = 'icloud_contacts'"
    echo ""
    echo "Target:"
    if [ "$LOCAL" = "1" ]; then
        echo "  postgres = ${POSTGRES_USER}@${POSTGRES_CONTAINER}:${POSTGRES_DB} (local)"
    else
        echo "  ssh host = ${PI_HOST}"
        echo "  postgres = ${POSTGRES_USER}@${POSTGRES_CONTAINER}:${POSTGRES_DB}"
    fi
    echo ""
    echo "User-curated state on icloud_contacts rows (crm_contact_id,"
    echo "match_status='imported'/'ignored', etc.) will be DESTROYED."
    echo "The daemon's full resync will recreate rows with host_id set"
    echo "to the currently-paired host."
    echo ""
}

run_psql() {
    local sql="$1"
    if [ "$LOCAL" = "1" ]; then
        docker exec "$POSTGRES_CONTAINER" psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$sql"
    else
        ssh "$PI_HOST" "docker exec ${POSTGRES_CONTAINER} psql -U ${POSTGRES_USER} -d ${POSTGRES_DB} -c \"${sql}\""
    fi
}

print_banner

# Show current state so the operator sees what they're about to delete.
echo "Current iCloud Contacts row counts:"
run_psql "SELECT 'external_contact' AS table, COUNT(*) AS rows FROM external_contact WHERE source = 'icloud_contacts'
UNION ALL
SELECT 'event'            AS table, COUNT(*) AS rows FROM event            WHERE source = 'icloud_contacts';"

echo ""
read -r -p "Type 'yes' to proceed with deletion: " confirm
if [ "$confirm" != "yes" ]; then
    echo "Aborted."
    exit 1
fi

echo ""
echo "Deleting external_contact rows ..."
run_psql "DELETE FROM external_contact WHERE source = 'icloud_contacts';"

echo "Deleting event rows ..."
run_psql "DELETE FROM event WHERE source = 'icloud_contacts';"

echo ""
echo "Done. The paired Mac daemon's next full resync will repopulate"
echo "the rows with host_id set to the currently-paired host."
