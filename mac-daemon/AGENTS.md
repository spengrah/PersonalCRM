# Mac Daemon Rules

Gotchas specific to `mac-daemon/` (Swift). Cross-cutting rules (git practices, code quality, testing policy, safety-critical prohibitions) live in `.ai/rules/core.md` at the repo root — read that first. This file follows the same multi-agent convention as the root `AGENTS.md`: `mac-daemon/CLAUDE.md` is a symlink to this file so Claude Code loads it automatically when working under `mac-daemon/`, and other agents (Codex, etc.) can read `AGENTS.md` directly.

## Common Gotchas

| Mistake | Fix |
|---------|-----|
| Setting `CI=1` to skip mac-daemon real-system tests in a script | The Swift suite gates real-Keychain/notification tests on `environment["CI"] == "true"` (string `"true"`, NOT `"1"`). Use `CI=true`; `CI=1` leaves those tests running against the real login Keychain |
