# Frontend Rules

Follow [core rules](../.ai/rules/core.md). These are the essential Next.js/React constraints.

- Use the shared `apiClient` and `lib/contact-list-params.ts` helpers for API calls and contact-list URL state.
- Follow existing components and design patterns. Prefer single-view forms; a standalone design prototype is optional when direction is uncertain.
- Preserve accessible roles and names; use explicit input text colors and avoid unnecessary data-fetching waterfalls.
- Pre-push uses whole-frontend ESLint/type checks and changed-file Prettier checks. CI retains the full lint command.
- New worktrees do not install frontend dependencies automatically. Run `make worktree-deps` when frontend tooling is needed; in the main checkout use `bun install --frozen-lockfile` from `frontend/`.
- Use the Makefile E2E targets for environment setup, and `PLAYWRIGHT_GREP` for focused runs. Wait for rendered consequences, not just network responses; use `domcontentloaded` rather than `networkidle`.

Read relevant entries in [frontend troubleshooting](../.ai/guides/frontend-troubleshooting.md)
when changing URL state, forms, accessible menus, E2E selectors or isolation,
or tour/judge evidence. It covers database-reset conflicts, React Query timing,
and intent capture binding.
