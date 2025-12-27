# Agent Guidelines for Personal CRM

This project uses AI-specific documentation in the `.ai/` directory for better organization and token efficiency.

---

## 📚 Core References

- **[`.ai/rules.md`](.ai/rules.md)** - Critical rules and workflow **(START HERE)**
- **[`.ai/development.md`](.ai/development.md)** - Feature development guide
- **[`.ai/patterns.md`](.ai/patterns.md)** - Common code patterns
- **[`.ai/architecture.md`](.ai/architecture.md)** - Architecture context
- **[`.github/README.md`](.github/README.md)** - Issue workflow and templates

---

## 🚀 Quick Start for Agents

1. **Pick up work:** `gh issue list --label agent-ready`
2. **Read:** [`.ai/rules.md`](.ai/rules.md) for critical rules
3. **Follow:** Layered architecture (Handler → Service → Repository → DB)
4. **Reference:** Existing code is source of truth
5. **Test:** Always write tests

---

## ⚠️ Critical Rules

- Never use `time.Now()` → use `accelerated.GetCurrentTime()`
- Never skip architectural layers
- Never write raw SQL in Go → use sqlc
- Always run `./smoke-test.sh` before major commits
- Always sign commits → `git commit -S -m "..."`

---

## 📖 Documentation Hierarchy

1. **Existing code** - Always the source of truth
2. **[`.ai/rules.md`](.ai/rules.md)** - Development rules and workflow
3. **GitHub Issues** - Current work and context
4. **[`.ai/development.md`](.ai/development.md)** - How to implement features
5. **[`PLAN.md`](docs/PLAN.md)** - Historical context (may be outdated)

---

## Development Commands

See [README.md](README.md#development-commands) for full command reference.

**Most used:**
```bash
make dev                    # Start dev environment
make reload                 # ⚠️ IMPORTANT: Rebuild + restart (use after code changes)
make test-unit              # Unit tests
make test-integration       # Integration tests
./smoke-test.sh            # Full system test
```

### ⚠️ Critical: Use `make reload` After Code Changes

**Never use `make build` alone** when services are running. The old processes will keep running with stale code.

```bash
# ❌ WRONG - old processes keep running
make build                  # Builds but doesn't restart!

# ✅ CORRECT - rebuilds AND restarts
make reload                 # Builds and restarts apps (keeps DB)
```

---

**For comprehensive guidelines, see [`.ai/rules.md`](.ai/rules.md)**
