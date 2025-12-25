# Git Hooks

This directory contains git hook templates that enforce code quality locally.

## Available Hooks

### pre-commit (gofmt + prettier)
- **When:** Before each commit
- **What:** Auto-formats Go files with `gofmt` and frontend files with `prettier`
- **Speed:** Very fast (~100-200ms)
- **Behavior:** Never fails, just auto-fixes

### pre-push (lint)
- **When:** Before each push
- **What:** Runs `make lint` (golangci-lint) and `bun run lint` (ESLint + Prettier)
- **Speed:** ~8-10 seconds
- **Behavior:** Blocks push if linting fails

## Installation

Run from project root:
```bash
./scripts/install-git-hooks.sh
```

## Bypassing Hooks

**Not recommended**, but you can bypass hooks with:
```bash
git commit --no-verify
git push --no-verify
```

## For AI Agents

These hooks work with AI agents that commit code directly. The pre-commit hook auto-fixes formatting, and the pre-push hook ensures code quality before changes reach the remote.
