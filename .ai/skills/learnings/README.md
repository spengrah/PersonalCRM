# Learnings Skill

Extract and apply session learnings to improve future agent work.

## extract-learnings.py

Extract project-relevant learnings from session transcripts using headless Claude.

**Features:**
- Mines session transcripts including thinking blocks
- Incremental extraction (only processes new content since last call)
- Outputs structured learnings (type, summary, detail, actionable)
- Deduplicates by summary within each session file

```bash
# Preview transcript (no extraction)
python extract-learnings.py --dry-run

# Extract and show recommendations
python extract-learnings.py --recommend

# Extract and append to log
python extract-learnings.py

# Force full extraction (ignore incremental state)
python extract-learnings.py --full
```

**Dependencies:**
- PyYAML (auto-installed via `uv run --with pyyaml`)
- Requires `uv` to be installed

**Output:**
- Learnings appended to `.ai/log/learnings/{session_id}.yaml`
- Extraction state saved to `.ai/log/extraction-state.json` (gitignored)

## extract-learnings.sh

Shell wrapper for triggering extraction.

```bash
# Manual trigger
./extract-learnings.sh --trigger manual

# Only run if uncommitted changes exist
./extract-learnings.sh --trigger manual --if-dirty
```

## Usage

### Primary: /learnings skill

The `/learnings` skill (defined in `SKILL.md`) is the primary interface. It:
1. Runs extraction
2. Prompts agent to review and reflect
3. Agent applies learnings to appropriate locations
4. Agent commits learnings separately

### When extraction runs

- **Before pushing**: Automatically via git pre-push hook
- **Manual**: Run `/learnings` skill or `./extract-learnings.sh --trigger manual`

Future triggers (not yet implemented):
- Before compaction (PreCompact hook)
- After subagents complete (SubagentStop hook)

### Incremental extraction

State is tracked in `.ai/log/extraction-state.json`:
```json
{
  "session_id": "abc123",
  "last_extracted_timestamp": "2026-01-16T15:10:33",
  "last_extracted_line": 450
}
```

Each `/learnings` call only processes content after the last extraction, keeping transcript size bounded.

## Agent Compatibility

| Agent | Supported | Notes |
|-------|-----------|-------|
| Claude Code | Yes | Full transcript access including thinking blocks |
| Codex CLI | No | Reasoning content is encrypted; only summaries available |
| Cursor | Unknown | Not yet investigated |

Codex CLI stores sessions in `~/.codex/sessions/` as JSONL, but reasoning blocks are encrypted (`encrypted_content` field). Only brief summaries like "**Considering exploration plan**" are accessible. Without full reasoning, extraction value is limited.

## Hook Integration

### Implemented

Git pre-push hook (`.git/hooks/pre-push`) runs extraction automatically before push.

### Planned

Claude Code hooks could automate extraction at other points:

```json
{
  "hooks": {
    "SubagentStop": [{
      "hooks": [{ "type": "command", "command": "./.ai/skills/learnings/extract-learnings.sh --trigger subagent-stop" }]
    }],
    "PreCompact": [{
      "hooks": [{ "type": "command", "command": "./.ai/skills/learnings/extract-learnings.sh --trigger pre-compact" }]
    }]
  }
}
```

## Migration Script

`migrate-learnings.py` converts the old monolithic `learnings.yaml` to per-session files:

```bash
# Preview what would be created
python migrate-learnings.py --dry-run

# Run migration
python migrate-learnings.py

# Run migration and delete old file
python migrate-learnings.py --delete-old
```
