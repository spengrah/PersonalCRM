# Agent Orchestration Patterns

Patterns for coordinating multiple agents within a session.

## Async Agent Spawning

### Spawn Verification After Interruption

When spawning an agent with `run_in_background: true`, user interruption/rejection of the tool call doesn't prevent the agent from launching. The agent may start in the background before the rejection is processed.

**Before re-spawning after interruption:**

1. Check git history for unexpected commits:
   ```bash
   git log --oneline -10
   git reflog -10
   ```
   Look for commits with recent timestamps from unfamiliar authors or messages you didn't create.

2. Check working tree state:
   ```bash
   git status
   ```
   Look for unexpected changes or new files.

3. Try SendMessage to the agent name:
   ```bash
   SendMessage(to: "agent-name", message: "status check")
   ```
   If it fails with "No agent named X is currently addressable," the agent either never spawned or has already terminated.

**Two failure modes require different responses:**

| Failure Mode | Evidence | Action |
|--------------|----------|--------|
| Spawn never started | No new commits, clean tree, no reflog entries | Safe to re-spawn |
| Spawn started but terminated | Has commits/changes on target branch | DO NOT re-spawn - examine the work done |

The reflog pattern `refs/heads/BRANCH@{N}: commit: MESSAGE` followed by work you didn't initiate indicates a background agent already ran.

### Agent Addressability

Agent names are only addressable via SendMessage while the agent is actively running. Once an agent completes or terminates:
- SendMessage returns "No agent named X is currently addressable"
- This is true even if the agent successfully completed work
- Check git history and filesystem artifacts to verify completion

### Re-spawning Decision Tree

```
User rejected async agent spawn
↓
Check git reflog for recent commits
↓
Found commits from agent? ──YES──→ Don't re-spawn, examine work
↓ NO
Check working tree status
↓
Has unexpected changes? ──YES──→ Don't re-spawn, examine changes
↓ NO
Try SendMessage status check
↓
Agent addressable? ──YES──→ Agent still running, wait
↓ NO
Safe to re-spawn
```
