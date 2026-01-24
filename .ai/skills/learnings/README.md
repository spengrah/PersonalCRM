# Learnings Skill

Extract and apply session learnings to improve future agent work.

## Learning Categories

Learnings fall into a 2x2 matrix based on type and scope:

```
                    Project-specific          General
                    ─────────────────────────────────────────
Rules/Guidelines    │ A: Project rules       │ (out of scope)
("always/never X")  │ → .ai/rules/           │
                    │ → .ai/patterns/        │
                    │ → AGENTS.md            │
                    ├─────────────────────────────────────────
Techniques          │ B: Project skills      │ C: Portable skills
("how to solve X")  │ → .ai/skills/          │ → ~/.claude/skills/
```

- **A (Rules/Guidelines, Project)**: Constraints, conventions, gotchas specific to this codebase → project docs
- **B (Techniques, Project)**: How to debug/solve problems in this codebase → `.ai/skills/`
- **C (Techniques, General)**: Portable techniques that work across codebases → `~/.claude/skills/`

The `/learnings` skill routes each learning to the appropriate destination.

## extract-learnings.py

Extract project-relevant learnings from session transcripts using headless Claude.

**Features:**
- Mines session transcripts including thinking blocks
- Incremental extraction (only processes new content since last call)
- Outputs structured learnings (type, summary, detail, actionable)
- Deduplicates by summary within each session
- Per-session files in `.ai/log/learnings/{session_id}.yaml`

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

**Headless execution note:**
The extraction uses `--permission-mode bypassPermissions` to allow automated/headless execution. Without this, headless Claude hangs waiting for Read permission approval that never comes. This is safe because extraction only reads its own transcript file.

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

### When to run

- **Before pushing**: Agent rule in AGENTS.md instructs running `/learnings` pre-push
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
| Claude Code | ✅ Yes | Full transcript access including thinking blocks |
| Codex CLI | ❌ No | Reasoning content is encrypted; only summaries available |
| Cursor | ❓ Unknown | Not yet investigated |

The script detects the agent type via the `CLAUDE_CODE` environment variable. When not running under Claude Code, extraction silently exits with code 0 (no error). This allows the same hooks to work across different agents without failing.

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
