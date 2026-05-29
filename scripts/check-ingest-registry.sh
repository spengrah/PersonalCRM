#!/usr/bin/env bash
# check-ingest-registry.sh — grep guard for #342's descriptor table.
#
# Enforces that the IngestBatch function body in
# backend/internal/service/ingest.go routes EVERY daemon-push kind
# through the daemonFamily descriptor table (kindToFamily lookups) and
# never re-introduces a parallel routing seam. The body must contain:
#   (a) no events.Kind* constant references (events\.Kind[A-Z]);
#   (b) no dotted kind string literals (double-quoted OR backtick-raw),
#       e.g. "raw_message.received" / `call.sent`;
#   (c) no legacy per-family predicate calls
#       (isRawMessageKind / isExternalContactKind / isMeetingNoteKind /
#        isCallKind) — these were deleted in favor of kindToFamily and
#        must not come back as a parallel routing path.
#
# Scope is ONLY the IngestBatch body (between its signature line and its
# lone top-level closing brace). Handler/verifier functions OUTSIDE
# IngestBatch legitimately name kinds (e.g. handleRawMessage's IsOutgoing,
# the verify switches, shouldRunInlineOnDuplicate) and are NOT scanned.
#
# New routing must go through ingest_registry.go's descriptor table; see
# .ai/patterns/sync.md "Daemon event families".
#
# Exits 0 when the IngestBatch body is kind-reference-free, 1 otherwise.

set -euo pipefail

cd "$(dirname "$0")/.."

TARGET="backend/internal/service/ingest.go"

if [[ ! -f "$TARGET" ]]; then
  echo "❌ ingest-registry guard: $TARGET not found" >&2
  exit 1
fi

# Extract the IngestBatch body: from the function signature line through
# its lone top-level closing brace (a `}` at column 0). Inner braces are
# indented, so `^}$` matches only the function's own close.
BODY=$(awk '
  /^func \(s \*IngestService\) IngestBatch\(/ { f = 1 }
  f { print }
  f && /^}$/ { exit }
' "$TARGET")

if [[ -z "$BODY" ]]; then
  echo "❌ ingest-registry guard: could not locate the IngestBatch function body in $TARGET" >&2
  echo "   (the awk region extractor keys on 'func (s *IngestService) IngestBatch(' and the" >&2
  echo "   function's lone top-level closing brace — check that signature/format is intact)" >&2
  exit 1
fi

# Three forbidden patterns, scoped to the extracted body.
CONST_PATTERN='events\.Kind[A-Z]'
LITERAL_PATTERN='["`](raw_message|external_contact|meeting_note|call|message|task|calendar|interaction|contact_methods)\.[a-z_]+'
PREDICATE_PATTERN='\b(isRawMessageKind|isExternalContactKind|isMeetingNoteKind|isCallKind)\b'

COMBINED="${CONST_PATTERN}|${LITERAL_PATTERN}|${PREDICATE_PATTERN}"

HITS=$(printf '%s\n' "$BODY" | grep -nE "$COMBINED" || true)

if [[ -n "$HITS" ]]; then
  echo "❌ ingest-registry guard: kind reference(s) found inside the IngestBatch body:" >&2
  printf '%s\n' "$HITS" | sed 's/^/   /' >&2
  echo >&2
  echo "IngestBatch routing must go through the daemonFamily descriptor table" >&2
  echo "(kindToFamily lookups in backend/internal/service/ingest_registry.go)." >&2
  echo "Do NOT name event kinds, dotted kind literals, or per-family predicates" >&2
  echo "in the IngestBatch body. See .ai/patterns/sync.md 'Daemon event families'." >&2
  exit 1
fi

echo "✓ ingest-registry guard: IngestBatch body is kind-reference-free"
