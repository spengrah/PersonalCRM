# Design: a visible "Back to list" control that keeps the user's place

- **Date:** 2026-07-21
- **Status:** approved (brainstorming)
- **Source:** GitHub issue #712 (label `ux-qa-agent` + `enhancement`); UXQA judge trace `judge-CON-051-2936fb4315716b37-item0` (runId `20260721T092414Z`, gitSha `8695685`)
- **Intent served:** CON-051 — *"Browsing contacts never loses the user's place"*

## Problem

The contact detail page carries list context (sort, order, search, two filters) correctly into prev/next navigation, and the keyboard **Escape** key returns to the list with that context preserved. But there is **no visible, clickable affordance** to return to the list. The `ContactNavigationBar` renders only `‹ N of M ›`. The single visible route back is the top navigation's "Contacts" link, which resets sort/search/filter to defaults. This breaks CON-051's "keyboard and mouse as equals" clause on the return trip: a mouse user who sorted/filtered/searched, opened a contact, and wants to go back has no discoverable way to return to the exact list state they left. Additionally, the list's page position is lost even via Escape, because page is not part of the carried context.

## Goal & scope

**In scope**
1. A visible "Back to list" control on the contact detail page that returns to the list with sort, order, search, and both filters preserved — at parity with the existing keyboard Escape route.
2. **Page restoration:** returning also lands the user on the list page that contains the contact they were viewing, rather than page 1.

**Explicitly out of scope (not planned)**
- Scroll-into-view and a transient highlight of the originating row on return. Considered and excluded — not planned.
- Any change to the top navigation's "Contacts" link — it deliberately stays a context-free reset.
- Slide-over/peek record browsing (a larger architectural direction surfaced in research) — not pursued.

## Design decisions

Decisions were validated against research into leading apps (Linear, Gmail, Airtable, Notion, Salesforce, HubSpot, Attio) and design-system guidance (Material 3, Apple HIG, Shopify Polaris, NN/g, Atlassian). The consistent finding: back-to-list belongs on the **leading (left) edge** and is a distinct affordance from prev/next paging ("leave this level" vs "next sibling"); it is semantically an **Up** action (a known parent with state restored), best presented as a **labeled** control rather than a bare arrow (a lone `←` collides visually with the pager's `‹`).

- **Layout — column-aligned bar.** The navigation bar's contents are constrained to the same `max-w-4xl` (~900px) column as the detail body. "Back to list" sits at the column's left edge (aligned with the contact name); the `‹ N of M ›` pager sits at the column's right edge (aligned with the action buttons). The grey background stays full-bleed; only its inner content is column-constrained. This resolves the original disharmony where bar items floated to the screen edges, unrelated to the centered content.
- **Icon — back arrow** (`M19 12H5M12 19l-7-7 7-7`, lucide `arrow-left`), not a list glyph (which competes with the adjacent pager chevron).
- **Label — static "Back to list."** The honest right-generic label: correct whether or not a filter/search is active. Dynamic per-context labels ("Search results", etc.) were considered and rejected as unnecessary complexity.
- **Alignment detail:** the control is a single flex row — `display:inline-flex; align-items:center; gap:6px; line-height:1`, with the SVG and text span both `display:block` — so the arrow and label share one centered baseline and cannot drift vertically.

## Technical design

### Which page "Back to list" targets

The detail page already loads the **full ordered ID list** for the current context via `useContactIDs(listContext)` and computes `currentIndex` / `total`. The target page is therefore computed, not carried: `page = Math.floor(currentIndex / PAGE_SIZE) + 1`, with `PAGE_SIZE = 20` (the list's `limit`). This needs no "entry page" plumbing and has a better property than carrying an entry page: after the user arrows prev/next across a page boundary, "Back to list" follows the contact currently in view. When `currentIndex < 0` (ID list not yet loaded), the page is omitted and the list opens on page 1 (graceful degradation).

### Page as a URL parameter on the list

Today the list's page is ephemeral local state (`useState(1)` in `contacts/page.tsx`); it is never reflected in the URL, so nothing downstream can restore it. The fix makes `?page=N` the source of truth for the list surface, mirroring how sort/search/filters already work:
- The list reads its initial page from `searchParams` on mount and writes it back via `router.replace(..., { scroll: false })` on page change (same pattern as the existing search sync).
- Page is **not** added to `ContactListContext`. It is meaningful only for the list surface and the back-target, never for prev/next or detail URLs. Keeping it out of the context means detail and prev/next URLs stay page-free.
- `buildContactListUrl(context, page?)` gains an optional `page` argument and appends `&page=N` only when `page > 1`. `parseListContext` is unchanged (page is read directly by the list, not through the context type).
- On sort/order/search/filter change the list already resets to page 1; those call sites pass no page (→ omitted → page 1), which stays correct.

Bonus: URL-syncing page also makes the contact list linkable and refresh-stable (today a refresh silently snaps back to page 1).

### Return-to-list call sites

The only return-to-list path today is the Escape handler (`contacts/[id]/page.tsx` → `buildNavigationUrl()` → `buildContactListUrl(listContext)`). Both the new button and Escape should share one page-aware helper:
- Add `buildBackToListUrl()` = `buildContactListUrl(listContext, pageFromIndex(currentIndex))`.
- The new "Back to list" button `onClick` calls `router.push(buildBackToListUrl())`.
- The Escape handler's no-id branch calls the same helper (so keyboard and mouse restore identically, including page).
- `buildContactDetailUrl` (prev/next, with an id) is unchanged.

### `ContactNavigationBar`

Add a left-aligned "Back to list" control and restructure the bar into `back-left / pager-right` within a column-constrained inner wrapper. The control takes an `onBack` callback and is hidden/disabled in edit mode (consistent with how prev/next is disabled while editing). Accessibility: it is a real `<button>` with visible text (no `aria-label` override needed), a visible focus ring, and the arrow marked decorative.

## Files to change

- `frontend/src/lib/contact-list-params.ts` — `buildContactListUrl` optional `page` param.
- `frontend/src/components/contacts/contact-navigation-bar.tsx` — add back control; column-constrained back-left/pager-right layout.
- `frontend/src/app/contacts/[id]/page.tsx` — `pageFromIndex` + `buildBackToListUrl`; wire button `onBack`; update Escape handler.
- `frontend/src/app/contacts/page.tsx` — read/write `?page` from/to the URL instead of pure local state.
- `spec/contacts.yaml` — new behavior (next free id **CON-065**) serving CON-051 covering the visible context-preserving return control + page restoration.
- `frontend/tests/e2e/test-map.json` — map the new/updated E2E spec to `@area` if needed.
- `frontend/tests/tours/judge/README.md` — add the `ux-qa-agent` label convention note (see below).

## Spec & test obligations

- **Behavior (SSOT):** add CON-065 to `spec/contacts.yaml`, `type: ux`, `status: current`, `surface: ui`, `serves: [CON-051]`, then-list covering (a) a visible control returns to the list with sort/order/search/filter preserved, and (b) it lands on the page containing the current contact. Run `make spec-lint` + `make spec-coverage`. Because the domain is `e2e_settled: true`, the new `surface: ui` behavior must land with its citing E2E test in the same PR.
- **E2E:** a Playwright test that sorts/filters the list, opens a contact from a non-first page, clicks "Back to list", and asserts the list request carries the same `sort`/`order`/filter params and the correct `page` (assert via `page.waitForResponse` params, not row positions — accelerated dates collapse). Add a keyboard-parity assertion that Escape restores identically.
- **Unit:** `contact-list-params.test.ts` for `buildContactListUrl` with/without `page`.

## Side task in this PR — `ux-qa-agent` label convention

Per the issue-triage convention established alongside #712, add a short note to `frontend/tests/tours/judge/README.md`: issues filed from a UXQA judge finding should carry the **`ux-qa-agent`** source label plus a type label (`bug` or `enhancement`) — the source label is orthogonal to type, matching the existing `improvement-audit` precedent; filter judge-derived work with `label:ux-qa-agent`.
