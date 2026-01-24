---
name: learnings
description: Extract learnings from current session before push or session end
allowed-tools: Bash Read Edit Glob Skill
---

# Extract Session Learnings

Capture insights so problems become patterns, mistakes become rules, techniques become skills.

## 1. Run Extraction

```bash
./.ai/skills/learnings/extract-learnings.sh --trigger manual
```

Outputs structured learnings to `.ai/log/learnings/{session_id}.yaml`.

## 2. Self-Reflection

Before reviewing, reflect:
- What did I discover that wasn't obvious before starting?
- What would I wish I knew if facing this problem again?
- Did I find a workaround through trial-and-error?

These may surface insights the automated extraction missed.

## 3. Review & Classify

Read the session file and classify each learning:

**Rules/Guidelines** ("always/never do X"):
- gotcha, distinction, architecture, rule, documentation_gap
- Route to: `.ai/rules/`, `.ai/patterns/`, `.ai/guides/`, `AGENTS.md`

**Techniques** ("how to solve X"):
- Debugging methods, workarounds, step-by-step solutions
- Ask: "Would this help in other codebases?"
  - No → project skill (`.ai/skills/`)
  - Yes → general skill (`~/.claude/skills/`)
- Route via `/extract-skill`

## 4. Apply

**Rules/Guidelines**: Edit the appropriate project doc directly.

**Techniques**: Invoke `/extract-skill` with the technique and scope.

For `documentation_gap`: find the failing doc, understand why it was missed, strengthen it.

## 5. Commit

```bash
git add .ai/log/learnings/ .ai/skills/ .ai/patterns/ .ai/rules/ .ai/guides/ AGENTS.md
git commit -S -m "docs: session learnings"
```

Only include modified files. User-level skills (`~/.claude/skills/`) are not committed.
