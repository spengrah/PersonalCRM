# GitHub Issue Workflow

This directory contains issue templates optimized for agent-driven development.

## Issue Templates

### 🚀 Feature (`feature.yml`)
For new features or enhancements. Includes:
- Category (Upstream vs Pi-specific)
- Priority levels
- Architecture layers affected
- Acceptance criteria checklist
- Testing requirements

### 🐛 Bug (`bug.yml`)
For bugs and issues. Includes:
- Severity levels
- Steps to reproduce
- Component identification
- Error logs section
- Investigation notes

### ♻️ Refactor (`refactor.yml`)
For code improvements without behavior changes. Includes:
- Refactor type (organization, extraction, deduplication, etc.)
- Current problem description
- Safety checklist
- Risk mitigation

### ✨ Improvement (`improvement.yml`)
For improvement tasks with a clear implementation plan.
Streamlined template for well-defined enhancements.

## Agent-Friendly Features

All templates include:
- ✅ **Structured data** - Dropdowns and checkboxes for consistency
- ✅ **Context links** - References to `AGENTS.md` for guidelines
- ✅ **Checklists** - Clear acceptance criteria agents can follow
- ✅ **Labels** - Auto-tagged with `agent-ready` for easy filtering

## Quick Start

### Creating Issues

**From GitHub UI:**
1. Go to Issues → New Issue
2. Choose a template
3. Fill out the form
4. Submit

**From GitHub CLI:**
```bash
# List available templates
gh issue create

# Create feature issue
gh issue create --template feature.yml

# Quick bug report
gh issue create --title "Bug: X is broken" --label bug

# From IMPROVEMENTS.md
gh issue create --template improvement.yml \
  --title "A.1.1 - Auto-run migrations"
```

### Creating Issues

```bash
# Interactive (choose template)
gh issue create

# Direct with specific template
gh issue create --template feature.yml

# Quick issue creation
gh issue create \
  --title "Add rate limiting middleware" \
  --label "enhancement,agent-ready" \
  --body "Add rate limiting to protect API endpoints"
```

## Development Setup

### Git Hooks

This project uses git hooks to enforce code quality locally:

1. **Install hooks:**
   ```bash
   ./scripts/install-git-hooks.sh
   # Or use the setup command:
   make setup
   ```

2. **What gets checked:**
   - Pre-commit: `gofmt` (auto-formats Go files) + `prettier` (auto-formats frontend files)
   - Pre-push: `make lint` (runs golangci-lint) + `bun run lint` (runs ESLint + Prettier)

3. **Manual checks:**
   ```bash
   make lint        # Check for issues
   make lint-fix    # Auto-fix some issues
   ```

**For AI Agents:** Run `./scripts/install-git-hooks.sh` or `make setup` as part of environment setup.

## Agent Workflow

### For Agents (AI)

When picking up work:

```bash
# Find ready issues
gh issue list --label "agent-ready" --state open

# View issue details
gh issue view 123

# Create branch from issue (use appropriate prefix)
gh issue develop 123 --name "feat/auto-migrations"  # for features
gh issue develop 45 --name "fix/health-check"       # for bugs
gh issue develop 67 --name "refactor/handlers"      # for refactors

# Work on code...

# Reference issue in commits (always sign with -S)
git commit -S -m "feat: auto-run migrations on startup

Implements golang-migrate auto-migration.

Fixes #123"

# Create PR (auto-links to issue)
gh pr create --fill
```

> **Note:** See [`.ai/rules.md`](.ai/rules.md#2-git-workflow-rules) for full branch naming conventions.

### For Humans

**Reviewing agent work:**

```bash
# Check agent-created PRs
gh pr list --label "agent-ready"

# Review PR
gh pr view 45
gh pr diff 45

# Approve or request changes
gh pr review 45 --approve
gh pr review 45 --request-changes --body "Please add tests"

# Merge when ready
gh pr merge 45 --squash
```

## Labels

Standard labels for agent workflow:

| Label | Purpose |
|-------|---------|
| `agent-ready` | Task is ready for agent pickup |
| `agent-in-progress` | Agent is working on it |
| `agent-review` | Needs human review |
| `agent-blocked` | Needs human help/decision |
| `upstream` | Can be contributed back |
| `pi-specific` | Pi deployment only |

**Add labels:**
```bash
gh issue edit 123 --add-label "agent-ready"
gh pr edit 45 --add-label "agent-review"
```

## Project Board (Optional)

Create a project board for tracking:

```bash
# Create project
gh project create --title "Personal CRM Development"

# Add items
gh project item-add 1 --url "https://github.com/USER/repo/issues/123"

# View board
gh project view 1 --web
```

## Tips

**For efficient agent work:**

1. **Be specific** - Good issue titles help agents find relevant work
2. **Use checklists** - Agents can track progress by updating checkboxes
3. **Reference docs** - Link to `AGENTS.md` for context
4. **Include examples** - Code snippets, similar features, etc.
5. **Define done** - Clear acceptance criteria

**Issue quality checklist:**
- [ ] Title is clear and specific
- [ ] Description explains the "why"
- [ ] Implementation approach is outlined
- [ ] Acceptance criteria are listed
- [ ] Files to modify are identified
- [ ] Time estimate is provided

## Examples

### Good Feature Issue
```
Title: Add API key authentication middleware

Priority: P1 (High)

Description:
Pi backend needs basic authentication when exposed via Tailscale.

Layers: Handler, Infrastructure

Acceptance:
- [ ] Middleware created
- [ ] Applied to /api/v1 routes
- [ ] Tests pass
- [ ] Documentation updated

Estimate: 2 hours
```

### Good Bug Issue
```
Title: Biweekly cadence reminders not generated

Severity: High
Component: Backend - Scheduler

Reproduce:
1. Create contact with cadence="biweekly"
2. Trigger scheduler
3. No reminder created

Expected: Reminder generated every 2 weeks

Investigation:
ParseCadence() missing "biweekly" case
```

---

## Contributing Back to Upstream

This fork uses a "build first, contribute later" strategy:

1. **Build for your needs** - Don't worry about upstream contribution while developing
2. **Deploy and test** - Run it on your Pi, use it daily
3. **Identify stable improvements** - After features are battle-tested, identify what's worth sharing
4. **Isolate changes** - Use git to cherry-pick or create clean branches for upstream PRs

### Identifying Upstream-Ready Changes

Good candidates for upstream contribution:
- Bug fixes
- Performance improvements
- Better error handling
- Test coverage improvements
- Documentation improvements
- Refactors that improve code quality
- Generic features that aren't deployment-specific

NOT good candidates:
- Pi-specific deployment scripts
- Personal integrations (your Gmail, Telegram, etc.)
- Mac LLM worker architecture
- Tailscale-specific networking
- Personal customizations

### Preparing Upstream PRs

When ready to contribute:

1. **Create a clean branch from upstream main:**
   ```bash
   git remote add upstream https://github.com/UPSTREAM/personal-crm
   git fetch upstream
   git checkout -b upstream-feature upstream/main
   ```

2. **Cherry-pick relevant commits:**
   ```bash
   git cherry-pick <commit-hash>
   # Or manually apply changes
   ```

3. **Ensure it's self-contained:**
   - No dependencies on your Pi-specific changes
   - Passes all existing tests
   - Includes new tests if needed
   - Follows upstream's style

4. **Submit PR to upstream:**
   ```bash
   gh pr create --repo UPSTREAM/personal-crm --title "..." --body "..."
   ```

### Tracking Upstream Contributions

Use GitHub labels on your issues/PRs:
- `upstream-candidate` - Might be worth contributing
- `upstream-ready` - Tested and ready to PR upstream
- `upstream-submitted` - PR sent to upstream
- `pi-only` - Not for upstream (deployment-specific)

---

*This workflow is designed for both human developers and AI agents working together on the Personal CRM project.*

