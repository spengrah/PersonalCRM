#!/bin/bash
# Wrapper script for extract-learnings.py
# Designed to be called from hooks (PreCompact, SubagentStop, git pre-push)
#
# Usage:
#   ./extract-learnings.sh [--trigger TRIGGER_NAME] [--if-dirty]
#
# Options:
#   --trigger NAME    Name of the trigger (for logging and output format)
#   --if-dirty        Only run if there are uncommitted changes since last push

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

TRIGGER=""
IF_DIRTY=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --trigger)
            TRIGGER="$2"
            shift 2
            ;;
        --if-dirty)
            IF_DIRTY=true
            shift
            ;;
        *)
            shift
            ;;
    esac
done

# Check if dirty flag is set
if [ "$IF_DIRTY" = true ]; then
    # Check for uncommitted changes
    if git diff --quiet && git diff --cached --quiet; then
        echo "[extract-learnings] No uncommitted changes, skipping." >&2
        exit 0
    fi
fi

echo "[extract-learnings] Running extraction (trigger: ${TRIGGER:-manual})..." >&2

cd "$PROJECT_ROOT"

# Capture state before extraction
BEFORE_HASH=""
if [ -f .ai/log/learnings.yaml ]; then
    BEFORE_HASH=$(md5sum .ai/log/learnings.yaml 2>/dev/null | cut -d' ' -f1 || md5 -q .ai/log/learnings.yaml 2>/dev/null)
fi

# Run extraction
uv run --with pyyaml python3 "$SCRIPT_DIR/extract-learnings.py" --recommend

exit_code=$?

if [ $exit_code -ne 0 ]; then
    echo "[extract-learnings] Extraction failed with code $exit_code" >&2
    exit $exit_code
fi

# Check if learnings.yaml was modified
AFTER_HASH=""
if [ -f .ai/log/learnings.yaml ]; then
    AFTER_HASH=$(md5sum .ai/log/learnings.yaml 2>/dev/null | cut -d' ' -f1 || md5 -q .ai/log/learnings.yaml 2>/dev/null)
fi

if [ "$BEFORE_HASH" = "$AFTER_HASH" ]; then
    echo "[extract-learnings] No new learnings." >&2
    exit 0
fi

# Output agent-facing message based on trigger
case "$TRIGGER" in
    pre-compact)
        # Exit code 2 + stderr = agent sees the message
        cat >&2 << 'EOF'

==============================================================
                    AGENT: LEARNINGS EXTRACTED
==============================================================
  New learnings from this session have been written to:
  .ai/log/learnings.yaml

  Before compaction, you should:
  1. Read .ai/log/learnings.yaml
  2. For each actionable learning, judge whether and how to
     apply it to the suggested_location
  3. Apply learnings now - context will be lost after compaction
==============================================================

EOF
        exit 2
        ;;
    subagent-stop)
        # Exit code 2 + stderr = agent sees the message
        cat >&2 << 'EOF'

==============================================================
                    AGENT: LEARNINGS EXTRACTED
==============================================================
  New learnings from this session have been written to:
  .ai/log/learnings.yaml

  You should:
  1. Read .ai/log/learnings.yaml
  2. For each actionable learning, judge whether and how to
     apply it to the suggested_location
==============================================================

EOF
        exit 2
        ;;
    session-end)
        # Brief message for human visibility, exit 0 (agent can't act anyway)
        echo ""
        echo "[extract-learnings] Session learnings saved to .ai/log/learnings.yaml"
        echo ""
        exit 0
        ;;
    *)
        # For manual or unknown triggers, just note that learnings were extracted
        echo ""
        echo "[extract-learnings] New learnings written to .ai/log/learnings.yaml"
        echo ""
        exit 0
        ;;
esac
