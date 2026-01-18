# Learnings Skill

Extract and apply session learnings to improve future agent work.

## extract-learnings.py

Extract project-relevant learnings from session transcripts using headless Claude.

**Features:**
- Mines session transcripts including thinking blocks
- Incremental extraction (only processes new content since last call)
- Outputs structured learnings (type, summary, detail, actionable, suggested_location)
- Deduplicates by session + summary

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
- Learnings appended to `.ai/log/learnings.yaml`
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

### When to run

- **Before pushing**: Agent rule in CLAUDE.md instructs running `/learnings` pre-push
- **Session end**: Human runs `/learnings` manually before closing

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

## Future: Hook integration

For automated extraction, hooks can be added later:

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
