# Settings

Settings holds the integration sections, backup and restore, paired Macs and system information. Locally no integration is connected, so most of this page is verified in its unconfigured state. Expected behavior lives in `spec/settings.yaml` and `spec/mac-host.yaml`; this file says how to drive it here.

## Sub-features

- `SET-019` provider sections, `SET-023` unconfigured sections, `SET-027` Telegram and `SET-036` WhatsApp when their features are off.
- `SET-028` backup and restore.
- `SET-035` system information.
- `MAC-001` pairing tokens, reached from `/settings/mac`.
- Not reachable here: `SET-020`, `SET-021`, `SET-022`, `SET-024`, `SET-025` and `SET-026`, which need a connected or connecting provider.

## How to get to it (user POV)

- Choose the `Settings` nav link (`/settings`).
- Choose `Manage paired Macs →` to reach `/settings/mac`.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. No integration is connected and Telegram and WhatsApp sync are disabled.

- **Load.** `page.goto('/settings')`. Sections are regions: `getByRole('region', { name: 'Google Accounts' | 'Todoist' | 'Telegram' | 'WhatsApp' | 'System Information' })`. Screenshot each region and judge it against `SET-023`, `SET-027` and `SET-036`. Do not click the connect buttons.
- **System information.** `getByTestId('app-version')` sits inside the `System Information` region.
- **Backup download.** `const [dl] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Download Backup' }).click()])`, then `await dl.saveAs(path.join(process.env.VERIFY_EVIDENCE_DIR, dl.suggestedFilename()))`. Find `POST /api/v1/export` in `api.jsonl` and check that the saved JSON holds the `Verify <stamp>` contacts you created.
- **Pair a Mac.** Choose `Manage paired Macs →`, click `Pair new Mac`, and wait for `getByTestId('pairing-token-value')` in `getByRole('dialog', { name: 'Pair new Mac' })`. Find `POST /api/v1/host/pairing-token` in `api.jsonl` and read `bin/psql -c "SELECT * FROM mac_host_pairing_token ORDER BY created_at DESC LIMIT 1"`. Without a Mac daemon the pairing cannot complete; stop at the token.

## Gotchas

- Never click these locally: Google `Add Account`, `Connect Google Account` or `Reconnect`, and Todoist connect (they leave for the provider's consent screen); Telegram `Connect Telegram`, `Send Code` and `Verify`; WhatsApp `Link WhatsApp`, `Send Pairing Code` and the QR flow; any `Sync Gmail|Calendar|Contacts|Chat` button.
- Connected states in the E2E specs (account rows, Telegram chats, WhatsApp QR codes) are `page.route` mocks and cannot be reproduced here.
- `Validate` on a backup opens a native `alert()`; register a dialog handler first.
- The `standard` world has no paired Macs, so host rows and `Uninstall <hostname>` are unreachable.
