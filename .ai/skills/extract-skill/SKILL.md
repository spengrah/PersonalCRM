---
name: extract-skill
description: |
  Create or update a SKILL.md from a technique or solution. Use when: (1) /extract-skill
  command, (2) "save this as a skill", (3) invoked by another skill or agent with a
  technique to persist. Outputs to .ai/skills/ (project) or ~/.claude/skills/ (general).
metadata:
  author: Claude Code
  version: "1.0.0"
allowed-tools: Read Write Glob Bash AskUserQuestion
---

# Extract Skill

Create or update a SKILL.md file from a technique that should auto-activate in future sessions.

## Input

You'll receive either:
- A technique description (from another skill, agent, or user)
- Context to analyze for extractable techniques

And a scope: `project` (.ai/skills/) or `general` (~/.claude/skills/)

If scope isn't provided, determine it: "Would this help in other codebases?"

## Quality Gate

Only proceed if the technique is:
- **Procedural**: Describes *how* to solve something
- **Non-obvious**: Required investigation to discover
- **Verified**: Actually worked
- **Has clear triggers**: Specific activation conditions

## Process

### 1. Check for Existing Skills

Search for similar skills to avoid duplication:

```bash
# Check project skills
ls .ai/skills/

# Check user skills
ls ~/.claude/skills/
```

Read any skills that might overlap. If a similar skill exists:
- **Update it** with the new technique (add to Solution, expand Triggers, etc.)
- Bump the version number
- Don't create a duplicate

### 2. Write or Update the Skill

```markdown
---
name: <kebab-case-name>
description: |
  <Optimize for semantic matching:>
  <- What this solves>
  <- Trigger conditions (error messages, symptoms)>
  <- Context markers (frameworks, environments)>
author: Claude Code
version: 1.0.0
allowed-tools:
  - <tools needed>
---

# <Name>

## Problem
<What this addresses>

## Triggers
<When to activate - specific error messages, symptoms, context>

## Solution
<Step-by-step technique>

## Verification
<How to confirm it worked>

## Notes
<Caveats, edge cases>
```

### 3. Save

```bash
# Project-specific
mkdir -p .ai/skills/<name>
# write SKILL.md

# General
mkdir -p ~/.claude/skills/<name>
# write SKILL.md
```

### 4. Report

State: skill name, location (created or updated), and what triggers will activate it.

## Description Quality

The `description` drives retrieval. Be specific.

**Bad:** `Helps with database problems`

**Good:**
```
Fix for PrismaClientKnownRequestError: Too many database connections
in serverless (Vercel, Lambda). Use when connection errors appear
after ~5 concurrent requests.
```

Include: exact error messages, framework names, specific symptoms.
