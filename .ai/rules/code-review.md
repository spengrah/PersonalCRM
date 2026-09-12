# Code Review Standards

Review whether the change fulfills its requirements and is fit to merge.
Block on concrete defects and material risks relevant to the change.
Optional improvements may accompany a passing review.

## Blocking Findings

Request changes for:

- Correctness bugs, race conditions, or relevant edge cases with a plausible failure path.
- Credible security issues, privacy violations, exposed secrets, or data-loss risks.
- Broken compatibility or incomplete migrations that prevent safe operation.
- Unmet requirements or unfinished work needed for the change to function correctly.
- Missing coverage for changed behavior or meaningful regression risks.
- Material performance or resource-use regressions under plausible workloads.
- Violations of explicit repository requirements, including required checks.

For each blocker, identify the triggering condition, resulting failure or unmet
requirement, and relevant file and line. Explain the evidence and any assumptions.
Investigate uncertainty where practical; a hypothetical concern without a plausible
failure path is a question, not an automatic blocker.

Ask for the smallest sufficient correction. Do not turn a narrow fix into an
architectural rewrite unless that is necessary to resolve the identified problem.

## Nonblocking Suggestions

Label stylistic preferences, alternative abstractions, optional optimizations,
extra comments, and unrelated cleanup as nonblocking. A TODO or acknowledged
limitation is not itself a blocker; explain what required behavior remains
unfinished if it prevents approval.

Do not require new tests for documentation, formatting, or mechanical edits
without a meaningful regression risk. Integration tests may replace unit tests
when they exercise the real behavior and avoid heavy mock infrastructure.
See `.ai/rules/testing.md` for proportional verification guidance.

## Review Process

1. Read the requirements, diff, relevant code, and applicable repository conventions.
2. Identify concrete defects and material risks; consider existing tests and verification evidence.
3. Present blockers first, ordered by severity, with file/line references and the triggering condition and impact.
4. Separate nonblocking suggestions and open questions from blockers. State verification gaps without treating every gap as a defect.
5. On subsequent reviews, verify fixes and inspect their consequences. Reopen unchanged code when new evidence warrants it; do not require fresh findings each round.
6. Approve when no blocking findings remain. Review approval does not replace required CI checks or production approval.

## Review Output Format

All AI reviewers MUST end their review with one of these verdicts:

```
## Final Recommendation
RESULT=PASS
```

When blocking findings remain:

```
## Final Recommendation
RESULT=FAIL
```

- Preserve the exact `RESULT=` line (no spaces around `=`) for automated integration.
- Use `PASS` when no blocking findings remain, even if nonblocking suggestions are present.
- Use `FAIL` when at least one finding meets the blocking criteria above.
- A passing review means no blockers were found in the review's scope; it does not claim the code cannot be improved.

## Convention Reference

- `.ai/rules/core.md` - Critical rules and patterns
- `.ai/rules/testing.md` - Testing requirements
- `.ai/guides/feature-development.md` - Feature development guide
- `.ai/guides/architecture.md` - Architecture context
- `.ai/patterns/` - Common code patterns
