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

This analyzes the session transcript (including thinking blocks) and writes structured learnings to `.ai/log/learnings.yaml`.

## 2. Review Extracted Learnings

Read `.ai/log/learnings.yaml` and review the latest entries. Each learning has:
- `type`: gotcha | distinction | architecture | rule | documentation_gap
- `summary`: One-line description
- `detail`: Full context
- `actionable`: Whether it can be applied now
- `suggested_location`: Where it might go (use your judgment)

Learning types:
- **gotcha**: Non-obvious things that trip up agents
- **distinction**: Critical differences that affect decisions
- **architecture**: How the system works in non-obvious ways
- **rule**: Mistakes that should become rules
- **documentation_gap**: Existing docs that weren't followed and need strengthening

## 3. Apply Learnings

For each learning, decide:
- Should it be applied now?
- Where should it go? (may differ from suggested_location)
- What's the specific change?

For `documentation_gap` types, focus on:
- Finding the existing doc that failed
- Understanding why it was missed (buried, unclear, too weak)
- Strengthening it so future agents won't miss it

Apply changes directly:
- `AGENTS.md`
- `.ai/patterns/*.md` -> Create or update pattern
- `.ai/rules/*.md` -> Add to relevant rule
- `.ai/guides/*.md` -> Create or update guide

Not every learning needs immediate action. Use your judgment.

## 4. Commit Learnings

Stage and commit separately from code changes:

```bash
git add .ai/log/learnings.yaml .ai/patterns/ .ai/rules/ .ai/guides/ .claude/CLAUDE.md
git commit -S -m "docs: session learnings"
```

Only include files that were actually modified.
