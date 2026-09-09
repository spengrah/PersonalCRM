#!/bin/bash
# Post a systemd unit failure to ntfy, for use from an OnFailure= handler.
#
# Usage: notify-unit-failure.sh <failed-unit-name>
#
# Reads NTFY_URL + NTFY_TOPIC from /etc/personalcrm/ntfy.env, the same file
# deploy-artifact.sh uses, so the topic has one writer. Degrades open: an absent
# or incomplete file skips the post and exits 0, because a missing notification
# channel must not turn into a second failed unit.
#
# The body carries the unit name and host only. Unit error text can quote row
# counts and other database facts, so it stays on the host and the reader is
# pointed at the journal instead. The topic is a capability token and is never
# logged.

set -uo pipefail

NTFY_ENV_FILE="${NTFY_ENV_FILE:-/etc/personalcrm/ntfy.env}"

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
    echo "usage: notify-unit-failure.sh <failed-unit-name>" >&2
    exit 2
fi
UNIT="$1"

if [ ! -r "$NTFY_ENV_FILE" ]; then
    echo "notify: $NTFY_ENV_FILE is not readable; skipping ntfy for $UNIT" >&2
    exit 0
fi

# shellcheck source=/dev/null
. "$NTFY_ENV_FILE"

if [ -z "${NTFY_URL:-}" ] || [ -z "${NTFY_TOPIC:-}" ]; then
    echo "notify: NTFY_URL or NTFY_TOPIC unset; skipping ntfy for $UNIT" >&2
    exit 0
fi

BODY="$UNIT failed on $(uname -n). Read the journal on the host: sudo journalctl _SYSTEMD_USER_UNIT=$UNIT -n 50"

# Bounded on purpose: this runs as a Type=oneshot instance keyed on the failing
# unit name, so a request that hangs leaves that instance activating forever and
# every later failure of the same unit is silently dropped.
if ! curl -fsS --connect-timeout 5 --max-time 15 \
    -H "Title: PersonalCRM backup failed" \
    -H "Priority: high" \
    -H "Tags: warning,floppy_disk" \
    -d "$BODY" "$NTFY_URL/$NTFY_TOPIC" >/dev/null 2>&1; then
    echo "notify: ntfy POST failed for $UNIT" >&2
    exit 1
fi

echo "notify: reported $UNIT to ntfy"
