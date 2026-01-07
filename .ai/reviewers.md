# Code Review Standards

**Philosophy:** "If Claude Code can fix it, request changes."

Since implementation cost is low with AI coding tools (Claude Code, Codex, etc.), maintain a HIGH bar for approval. Request changes for anything that could be improved.

---

## Approval Criteria (ALL must be true)

AUTO-APPROVE only if the code meets ALL of these criteria:

- ✓ **No security concerns** (SQL injection, XSS, auth issues, secrets in code, etc.)
- ✓ **No bugs or unhandled edge cases**
- ✓ **Comprehensive test coverage** for new/changed code
- ✓ **Clear, well-documented code** (comments where needed)
- ✓ **Follows repository conventions** (check `.ai/rules.md`)
- ✓ **Proper error handling and validation**
- ✓ **Good performance** (no obvious inefficiencies)
- ✓ **No TODOs or technical debt introduced**
- ✓ **No code style inconsistencies**

---

## Request Changes (DEFAULT)

REQUEST CHANGES if ANY of these apply:

**Security:**
- Security concerns (even potential or minor ones)
- Secrets or sensitive data in code
- Missing authentication or authorization checks
- Vulnerable dependencies

**Code Quality:**
- Bugs, race conditions, or edge cases not handled
- Unclear code needing comments or documentation
- Opportunities for refactoring or simplification
- Code style inconsistencies
- Doesn't follow repository best practices

**Testing:**
- Missing or insufficient test coverage
- Tests don't cover edge cases
- No integration tests for cross-component changes

**Performance:**
- Performance concerns or inefficiencies
- N+1 queries or excessive database calls
- Memory leaks or resource exhaustion risks

**Architecture:**
- Poor error handling or validation
- Missing logging for critical operations
- Breaks architectural patterns (see `.ai/architecture.md`)
- Violates conventions in `.ai/rules.md`

**Completeness:**
- TODOs or unfinished work
- Missing documentation for new features
- Incomplete migrations or rollback paths

---

## Review Process

1. **Read conventions:** Review `.ai/rules.md` for project-specific patterns
2. **Check ALL criteria:** Go through each approval criterion
3. **Default to request changes:** When in doubt, request improvements
4. **Be specific:** Point to exact files and line numbers
5. **Explain why:** Help developers understand the reasoning

---

## Review Output Format

**All AI reviewers MUST include this at the end of their review:**

```
## Final Recommendation
RESULT=PASS
```

Or if issues are found:

```
## Final Recommendation
RESULT=FAIL
```

**Requirements:**
- The `RESULT=` line must appear exactly as shown (no spaces around `=`)
- Use `PASS` if code meets ALL approval criteria
- Use `FAIL` if ANY issue warrants changes
- This enables automated status check integration

---

## Convention Reference

For detailed development conventions, see:
- **[`.ai/rules.md`](.ai/rules.md)** - Critical rules and patterns
- **[`.ai/development.md`](.ai/development.md)** - Feature development guide
- **[`.ai/architecture.md`](.ai/architecture.md)** - Architecture context
- **[`.ai/patterns.md`](.ai/patterns.md)** - Common code patterns
