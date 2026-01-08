# PersonalCRM - Claude Code Context

For comprehensive development guidelines, see:
- @.ai/rules.md - Complete development rules and architecture
- @.ai/reviewers.md - Code review standards (for PR reviews)
- @.github/README.md - GitHub workflow and issue management
- @AGENTS.md - Agent-specific guidelines and context

This project is already well-documented. Follow the guidelines in those files.

## Key Information
- This project uses bun as the package manager, so you should use bun instead of npm.

## Continuous Claude

**Directories:**
- `thoughts/ledgers/` - Session state (synced via git)
- `thoughts/shared/research/` - Investigation notes (synced via git)
- `thoughts/shared/plans/` - Implementation designs (synced via git)

**Usage:**
- Hooks auto-load ledger on session start and save before compaction
- Save important findings to `thoughts/shared/research/` or `thoughts/shared/plans/`
- Use `/clear` when context gets long; ledger preserves continuity
- Research/plans sync to Pi with regular `git push/pull`
