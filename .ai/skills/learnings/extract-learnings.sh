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

# Capture state before extraction (hash of learnings directory listing)
BEFORE_HASH=""
if [ -d .ai/log/learnings ]; then
    BEFORE_HASH=$(ls -la .ai/log/learnings/ 2>/dev/null | md5sum 2>/dev/null | cut -d' ' -f1 || ls -la .ai/log/learnings/ 2>/dev/null | md5 -q 2>/dev/null || echo "")
fi

# Run extraction
uv run --with pyyaml python3 "$SCRIPT_DIR/extract-learnings.py" --recommend

exit_code=$?

if [ $exit_code -ne 0 ]; then
    echo "[extract-learnings] Extraction failed with code $exit_code" >&2
    exit $exit_code
fi

# Check if learnings directory was modified
AFTER_HASH=""
if [ -d .ai/log/learnings ]; then
    AFTER_HASH=$(ls -la .ai/log/learnings/ 2>/dev/null | md5sum 2>/dev/null | cut -d' ' -f1 || ls -la .ai/log/learnings/ 2>/dev/null | md5 -q 2>/dev/null || echo "")
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
  .ai/log/learnings/

  Before compaction, you should:
  1. Read .ai/log/learnings/
  2. For each actionable learning, judge whether and where
     to apply it to project docs/rules
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
  .ai/log/learnings/

  You should:
  1. Read .ai/log/learnings/
  2. For each actionable learning, judge whether and where
     to apply it to project docs/rules
==============================================================

EOF
        exit 2
        ;;
    pre-push)
        # Exit code 2 + stderr = agent sees the message
        cat >&2 << 'EOF'

==============================================================
                    AGENT: LEARNINGS EXTRACTED
==============================================================
  New learnings from this session have been written to:
  .ai/log/learnings/

  Before pushing, you should:
  1. Read the new learnings in .ai/log/learnings/
  2. For each actionable learning, judge whether and where
     to apply it to project docs/rules
  3. Commit learnings (and any applied changes)
  4. Push again
==============================================================

EOF
        exit 2
        ;;
    session-end)
        # Brief message for human visibility, exit 0 (agent can't act anyway)
        echo ""
        echo "[extract-learnings] Session learnings saved to .ai/log/learnings/"
        echo ""
        exit 0
        ;;
    *)
        # For manual or unknown triggers, just note that learnings were extracted
        echo ""
        echo "[extract-learnings] New learnings written to .ai/log/learnings/"
        echo ""
        exit 0
        ;;
esac
