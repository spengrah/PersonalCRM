# Issue 41 - Multiple Contact Methods Plan

This plan captures the agreed UX decisions and the implementation sequence for
adding multiple contact methods with an optional primary designation.

## Goals

- Support multiple contact methods per contact with one optional primary.
- Migrate existing email/phone data into the new contact_method table.
- Update search and UI surfaces to use methods instead of contact.email/phone.
- Preserve layered architecture and sqlc usage.

## Decisions

- Primary is optional; selecting a primary clears others; re-click clears primary.
- Max one method per type per contact (unique type constraint).
- Display ordering: primary first, then by type priority:
  email_personal -> email_work -> phone -> telegram -> signal -> discord -> twitter.
- List views show primary + one secondary using the same priority order.
- Deep links: email (mailto), phone (tel), telegram (t.me), twitter (twitter.com),
  signal (signal.me). Discord and GChat are display-only.
- Normalization: trim whitespace for all; strip leading "@" for telegram/twitter in
  frontend and backend; store without "@", render with "@".
- Strict handles only for telegram/twitter (no URL parsing).

## Plan (A -> C -> B)

### A) UX Design (complete)

- Dynamic "Contact Methods" list with add/remove rows.
- Row fields: type select, value input (type-specific placeholder), primary star.
- Primary row styled with badge and subtle highlight.
- Disable already-used types in the selector.

### C) Backend / Data / API

- Migration: add contact_method table, migrate email/phone, drop columns.
- sqlc: add queries for contact_method CRUD and update contact queries/search.
- Repository/service/handler: expose methods, enforce primary + unique type.
- Update response/request structs for contact and reminders.
- Update search to include method values.

### B) Frontend UI

- Types + validation + API client updated for methods.
- Contact form: dynamic methods list with validation + primary toggle.
- List/detail/dashboard/reminders/selector show primary + secondary.
- Add method icon and link components for display + deep links.

## Tests

- Migration up/down coverage.
- Repository CRUD integration tests for contact_method.
- Service tests for method ordering and primary handling.
- API tests for create/update contact with methods.
- Frontend validation tests for method normalization.
- E2E test for adding contact with multiple methods.
