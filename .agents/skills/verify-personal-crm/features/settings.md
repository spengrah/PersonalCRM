# Settings

Settings holds the integration sections, backup and restore, paired Macs and system information. Locally no integration is connected, so most of this page is verified in its unconfigured state. Expected behavior lives in `spec/settings.yaml` and `spec/mac-host.yaml`; this file says how to drive it here.

## Sub-features

- `SET-019` provider sections, `SET-023` unconfigured sections, `SET-027` Telegram and `SET-036` WhatsApp when their features are off.
- `SET-028` backup and restore, `SET-030` what the backup holds.
- `SET-035` system information.
- `MAC-018` the paired-Mac list, minting a pairing token and revoking a host, `MAC-001` how the token is stored, reached from `/settings/mac`.
- Not reachable here: `SET-020`, `SET-021`, `SET-022`, `SET-024`, `SET-025` and `SET-026`, which need a connected or connecting provider.

## How to get to it (user POV)

- Choose the `Settings` nav link (`/settings`).
- Choose `Manage paired Macs →` to reach `/settings/mac`.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. No integration is connected and Telegram and WhatsApp sync are disabled.

- **Load.** `page.goto('/settings')`. Sections are regions: `getByRole('region', { name: 'Google Accounts' | 'Todoist' | 'Telegram' | 'WhatsApp' | 'System Information' })`. Screenshot each region and judge it against `SET-023`, `SET-027` and `SET-036`. Do not click the connect buttons.
- **System information.** `getByTestId('app-version')` sits inside the `System Information` region.
- **Backup download.** `const [dl] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Download Backup' }).click()])`, then `await dl.saveAs(path.join(process.env.VERIFY_EVIDENCE_DIR, dl.suggestedFilename()))`. Find `POST /api/v1/export` in `api.jsonl`. Compare the saved JSON's contacts with `bin/psql -c "SELECT count(*) FROM contact WHERE deleted_at IS NULL"` and judge its shape against `SET-030`.
- **Backup validate.** Register `page.once('dialog', d => { record d.message(); d.accept() })`. The file input has no label: use `page.locator('input[type="file"]').setInputFiles(<saved backup>)`. The `Validate` button appears once a file is chosen; click it. Find `POST /api/v1/import` in `api.jsonl` (its multipart body logs as `request: null`), record the alert text, and judge it against `SET-028`.
- **Pair a Mac.** Choose `Manage paired Macs →`, click `Pair new Mac`, and wait for `getByTestId('pairing-token-value')` in `getByRole('dialog', { name: 'Pair new Mac' })`. Find `GET /api/v1/host` and `POST /api/v1/host/pairing-token` in `api.jsonl` and read `bin/psql -c "SELECT token_hash, expires_at - created_at, consumed_at FROM mac_host_pairing_token ORDER BY created_at DESC LIMIT 1"`. Judge the page against `MAC-018` and the row against `MAC-001`. Without a Mac daemon the pairing cannot complete; stop at the token.
- **Rotate and uninstall.** The seeded host's card has `getByRole('button', { name: 'Rotate pair-key for <hostname>' })`, which mints a token (`POST /api/v1/host/pairing-token`) into `getByRole('dialog', { name: 'Rotate pair-key for <hostname>' })`, and `getByRole('button', { name: 'Uninstall <hostname>' })`, which opens `getByRole('dialog', { name: 'Uninstall Mac host' })`. Its `Uninstall` button sends `DELETE /api/v1/host/<id>`; read `bin/psql -c "SELECT api_key_revoked_at FROM mac_host WHERE id = '<id>'"` and judge against `MAC-018`.

## Gotchas

- Never click these locally:
  - Google `Connect Google Account`, `Add Account`, `Reconnect` and `Chat — reconnect required` (they leave for Google's consent screen), `Disconnect <account>`, and any `Sync Gmail|Calendar|Contacts|Chat` button.
  - Todoist `Connect Todoist Account`, `Connect Account`, `Reconnect`, `Disconnect <account>`, `Sync Tasks`, and the `Project` and `Label` selects.
  - Telegram `Connect Telegram`, `Send Code`, `Verify` and `Disconnect`.
  - WhatsApp `Link WhatsApp`, `Scan a QR code`, `Use a phone pairing code`, `Send Pairing Code`, `Link WhatsApp again` and `Disconnect`.
- Connected states in the E2E specs (account rows, Telegram chats, WhatsApp QR codes) are `page.route` mocks and cannot be reproduced here.
- `Validate` on a backup opens a native `alert()`; register a dialog handler first.
- The `standard` world seeds one paired Mac. Uninstalling it revokes it for the rest of the instance's life; relaunch to get it back. Pairing cannot complete without a Mac daemon.
