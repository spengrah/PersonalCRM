#!/usr/bin/env bash
# Assert that every pinned tour-fixture marker resolves to EXACTLY ONE contact in
# the world currently served by TOURS_API_URL.
#
# Why this exists as a gate step rather than a printout: a world missing a marker
# fails inside Playwright, twenty minutes in, as an opaque resolveFixture throw.
# Running the identical predicate first turns that into a specific failure in two
# seconds, naming every offending marker and its count.
#
# The marker tokens are read from the Go SSOT (backend/internal/synthetic/
# fixtures.go), not restated here, so this script cannot drift from the seed. The
# match predicate is the identical one resolveFixture uses: search the contacts
# list for the marker, then keep only rows whose full_name CONTAINS it (full-text
# search ranks and returns neighbours; only the marker decides).
#
# Deliberately NOT wired into run-tours.sh: the contacts tour CONSUMES
# fxdeletevictim, so a second TOURS_SKIP_RESET=1 iteration against an already
# swept world legitimately has zero matches for it. This is a gate step to run
# against a FRESH world, which is why it is invoked explicitly.
#
# Env:
#   TOURS_API_URL   backend base URL (default http://localhost:8080)
#   TOURS_API_KEY   API key (omitted when empty, for an unauthenticated backend)
#   FIXTURES_GO     override the SSOT path (tests only)
#
# Exit: 0 only when every marker resolves to exactly one contact. 1 on any
# missing marker, any duplicate, any HTTP failure, or an unreadable SSOT.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

API_URL="${TOURS_API_URL:-http://localhost:8080}"
API_KEY="${TOURS_API_KEY:-}"
FIXTURES_GO="${FIXTURES_GO:-$REPO_ROOT/backend/internal/synthetic/fixtures.go}"

if [ ! -r "$FIXTURES_GO" ]; then
    echo "check-tour-markers: cannot read the marker SSOT at $FIXTURES_GO" >&2
    exit 1
fi

# The same shape pinned-fixtures.test.ts reads: a tab-indented
# `FixtureMarkerXxx = "token"` const line.
MARKERS="$(sed -n 's/^	FixtureMarker[A-Za-z]*[[:space:]]*=[[:space:]]*"\([a-z0-9]*\)"$/\1/p' "$FIXTURES_GO")"
if [ -z "$MARKERS" ]; then
    echo "check-tour-markers: found NO marker constants in $FIXTURES_GO — the extraction is broken, not the world" >&2
    exit 1
fi

CURL_ERR="$(mktemp)"
trap 'rm -f "$CURL_ERR"' EXIT

curl_args=(-sS -f)
if [ -n "$API_KEY" ]; then
    curl_args+=(-H "X-API-Key: $API_KEY")
fi

# count_matches <marker> : rows whose full_name contains the marker, or ERR.
count_matches() {
    python3 -c '
import json, sys
marker = sys.argv[1]
try:
    payload = json.load(sys.stdin)
    rows = payload.get("data") or []
    if not isinstance(rows, list):
        raise ValueError("data is not a list")
except Exception:
    print("ERR")
    sys.exit(0)
print(sum(1 for r in rows if isinstance(r, dict) and marker in (r.get("full_name") or "")))
' "$1"
}

marker_count=0
bad=0

for marker in $MARKERS; do
    marker_count=$((marker_count + 1))

    body="$(curl "${curl_args[@]}" "$API_URL/api/v1/contacts?limit=200&search=$marker" 2>"$CURL_ERR")"
    rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "check-tour-markers: HTTP failure resolving '$marker' (curl exit $rc: $(tr -d '\n' <"$CURL_ERR"))" >&2
        exit 1
    fi

    count="$(printf '%s' "$body" | count_matches "$marker")"
    if [ "$count" = "ERR" ]; then
        echo "check-tour-markers: unreadable response body resolving '$marker'" >&2
        exit 1
    fi
    if [ "$count" != "1" ]; then
        echo "check-tour-markers: marker '$marker' resolves to $count contacts, want exactly 1" >&2
        bad=$((bad + 1))
    fi
done

if [ "$bad" -ne 0 ]; then
    echo "check-tour-markers: $bad of $marker_count markers did not resolve uniquely — the world is not tourable" >&2
    exit 1
fi

echo "check-tour-markers: all $marker_count markers resolve to exactly one contact"
exit 0
