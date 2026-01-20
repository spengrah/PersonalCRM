---
description: Extract learnings from current session before push or session end
allowed-tools: Bash, Read, Edit, Glob
---

# Extract Session Learnings

**Goal:** Each session should improve the next. This skill capture insights from your current session so problems become patterns, mistakes become rules, and future agents benefit from past work.

## 1. Run Extraction

```bash
./.ai/skills/learnings/extract-learnings.sh --trigger manual
```

This analyzes the session transcript (including thinking blocks) and writes structured learnings to `.ai/log/learnings/{session_id}.yaml`.

## 2. Review Extracted Learnings

Read the per-session file in `.ai/log/learnings/` and review the latest entries. Each learning has:
- `type`: gotcha | distinction | architecture | rule | documentation_gap
- `summary`: One-line description
- `detail`: Full context
- `actionable`: Whether it can be applied now

Learning types:
- **gotcha**: Non-obvious things that trip up agents
- **distinction**: Critical differences that affect decisions
- **architecture**: How the system works in non-obvious ways
- **rule**: Mistakes that should become rules
- **documentation_gap**: Existing docs that weren't followed and need strengthening

## 3. Apply Learnings

**Important:** Learnings enhance project documentation and rules, not production code. You decide where (if anywhere) each learning belongs based on full context.

For each actionable learning, decide:
- Should it be applied now?
- Where should it go?
- What's the specific change?

Possible destinations:
- `.ai/rules/*.md` - Add to relevant rule
- `.ai/patterns/*.md` - Create or update pattern
- `.ai/guides/*.md` - Create or update guide
- `AGENTS.md` - Update agent instructions

For `documentation_gap` types, focus on:
- Finding the existing doc that failed
- Understanding why it was missed (buried, unclear, too weak)
- Strengthening it so future agents won't miss it

Not every learning needs immediate action. Use your judgment.

## 4. Commit Learnings

Stage and commit separately from code changes:

```bash
git add .ai/log/learnings/ .ai/patterns/ .ai/rules/ .ai/guides/ .claude/CLAUDE.md
git commit -S -m "docs: session learnings"
```

Only include files that were actually modified.
