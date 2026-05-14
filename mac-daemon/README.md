# crm-mac — Personal CRM Mac daemon

Single-binary Swift daemon that ingests Apple Messages + iCloud
Contacts from a Mac and pushes events to the Pi-side Personal CRM
API. Phase 1 (this PR is PR6) lands the daemon framework only —
no source readers yet. PR7 adds the `messages` source; PR8 adds
`icloud_contacts`.

See `../.ai/spec/mac-daemon.md` for the authoritative scope. This
README covers local install + smoke procedures only.

## Requirements

- macOS 13+ (the package targets `macOS(.v13)`).
- Xcode Command Line Tools or a full Xcode install (the latter is
  required for `swift test` — Command Line Tools ship Swift but not
  XCTest).
- A running Pi-side `crm-api` reachable from the Mac (typically via
  Tailscale).

## Build

From the repo root:

```bash
make mac-daemon
```

This runs `swift build -c release` from `mac-daemon/` and ad-hoc
codesigns the resulting binary. Output:
`mac-daemon/.build/release/crm-mac`.

## Pair + install (manual smoke)

The PR6 Definition of Done. Replace `<pi-host>` with your Pi's
reachable hostname (typically a Tailscale name) and `<pi-url>` with
the Pi's HTTPS base URL.

**On the Pi:** mint a single-use pairing token.

```bash
ssh <pi-host> "cd /opt/personal-crm && ./backend/crm-admin --mint-pairing-token --hostname-label mac-1"
# token=<plaintext-base64-rawurl>
# expires_at=2026-05-13T15:42:18Z
# hostname_label=mac-1
# note: paste into `crm-mac install --pair <token>` within 10 minutes
```

The pairing token is single-use and short-lived (10 minutes). The
`--hostname-label` flag is operator-side terminal context only — it
is NOT persisted server-side. The Mac daemon supplies its own
hostname at pair time via `crm-mac install --hostname`.

**On the Mac:** install the daemon.

```bash
./mac-daemon/.build/release/crm-mac install \
    --pair <token> \
    --pi-url <pi-url> \
    --hostname mac-1
```

`--hostname` is REQUIRED on fresh install. Pick a non-PII label
(`mac-1`, `work-mac`, `home-laptop`). The value is stored in the
Pi's `mac_host` table and shown in the settings UI.

The first launch may trip Gatekeeper because the binary is ad-hoc
signed. If it does, the daemon will print a hint pointing at
**System Settings → Privacy & Security → "crm-mac was blocked..." →
Open Anyway.**

## Verify

```bash
./mac-daemon/.build/release/crm-mac doctor
# PASS  keychain: api-key present
# PASS  launchctl: service registered
# PASS  config_state: host=mac-1 schemaVersion=1
# PASS  pi_reachability: phones=N emails=N
```

Exit code = number of FAIL entries.

```bash
launchctl print gui/$(id -u)/xyz.spengrah.crm-mac >/dev/null && echo "registered"
# registered
```

We deliberately do NOT parse any specific line from `launchctl
print` output — the human-readable format is informational, not API.

## Status

```bash
./mac-daemon/.build/release/crm-mac status
# installed=true
# registered=true
# hostname=mac-1
# pi_url=<pi-url>
# host_id=<uuid>
# state_schema_version=1
# last_heartbeat_at=2026-05-13T16:00:00Z
```

## Reboot smoke

To verify Keychain access from the launchd context (the daemon runs
under launchd; the Keychain is unlocked after first interactive
login of the boot session per
`kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`):

```bash
sudo shutdown -r now
# After login, wait ~60s for the heartbeat tick:
./mac-daemon/.build/release/crm-mac status
# last_heartbeat_at should be recent.
```

## Upgrade

To replace the installed binary in place without re-pairing:

```bash
make mac-daemon
./mac-daemon/.build/release/crm-mac install --upgrade
```

`--upgrade` reads the existing `config.json` + Keychain api-key,
bootouts the running daemon, atomic-renames the new binary into
place, and re-bootstraps launchd. It does NOT call `POST /host`.

## Recovery scenarios

**Pi unreachable during fresh install (network drop after Pi
commit).** The pair tx may have committed before the daemon got the
response back. The Pi-side host row exists but the daemon doesn't
know its `host_id` or `api_key`:

```bash
ssh <pi-host> "cd /opt/personal-crm && ./backend/crm-admin --list-hosts"
# id=<uuid> hostname=mac-1 created_at=... last_heartbeat_at=never
ssh <pi-host> "cd /opt/personal-crm && ./backend/crm-admin --revoke-host <uuid>"
# revoked host_id=<uuid>
# Then re-mint a token and re-install.
```

**Post-pair persistence failure (Keychain write, state write, or
atomic-rename failed).** Local state is partially populated:

```bash
./mac-daemon/.build/release/crm-mac uninstall --purge
ssh <pi-host> "cd /opt/personal-crm && ./backend/crm-admin --revoke-host <host_id>"
# Re-mint a token and re-install.
```

**`launchctl bootstrap` failed (binary + config + Keychain + state
all in place; only launchd registration failed).** The binary IS
installed:

```bash
# Address the underlying issue (System Settings -> Privacy & Security).
./mac-daemon/.build/release/crm-mac install --register-only
```

## Uninstall

```bash
./mac-daemon/.build/release/crm-mac uninstall --purge
# bootouts launchd, removes plist, deletes Keychain entry, removes
# config.json + state.json + installed binary.
```

The Pi-side `mac_host` row is NOT touched — run `crm-admin
--revoke-host <uuid>` on the Pi if you want to remove the row.

## Where things live

| Item | Path |
|---|---|
| Binary (after install) | `~/Library/Application Support/crm-mac/bin/crm-mac` |
| LaunchAgent plist | `~/Library/LaunchAgents/xyz.spengrah.crm-mac.plist` |
| config.json | `~/Library/Application Support/crm-mac/config.json` |
| state.json | `~/Library/Application Support/crm-mac/state.json` |
| stdout/stderr logs | `~/Library/Logs/crm-mac/{stdout,stderr}.log` |
| Keychain entry | service `xyz.spengrah.crm-mac`, account `api-key` |
| Unified log subsystem | `xyz.spengrah.crm-mac` |

To tail the unified log:

```bash
log stream --predicate 'subsystem == "xyz.spengrah.crm-mac"' --info
```

## Spec deviation notes

- **`cursor_epoch` is NOT in Keychain** (spec lists it; PR6 puts it
  in `state.json` instead). It's an opaque integer the Pi increments
  on backup-restore — not a secret, no security guarantee from
  Keychain storage, and putting it on disk avoids a Keychain write
  per source-poll. The Pi's cursor + epoch remain the source of
  truth; PR7/PR8 refresh from heartbeat responses and handle
  `cursor_epoch` mismatch by refetching Pi state rather than
  trusting local cache.
- **Bare CLI binary, not an `.app` bundle.** PR6 uses classic
  `launchctl bootstrap gui/<uid> <plist>` rather than
  `SMAppService.agent`, which expects the plist to be a resource of
  the calling main app bundle. See `.ai/log/plan/mac-daemon-phase-1-pr6-daemon-skeleton.md` D14 for the rationale.
- **CI pinned to `macos-15`**, not `macos-latest`. Eliminates the
  silent image-migration risk; an image deprecation will surface as
  a loud CI failure in a follow-up PR.

## Testing

Unit + lifecycle tests run on CI via `.github/workflows/mac-daemon.yml`
on `macos-15`. To run locally:

```bash
cd mac-daemon
swift test
```

`swift test` requires XCTest, which ships with the full Xcode (not
the Command Line Tools subset). The `KeychainProductionTests` are
skipped in CI (`XCTSkipIf(CI=true)`) — they exercise SecItem* against
the developer's keychain using a per-run test account.

You can also run via the project Makefile:

```bash
make test-daemon-local
```

## Operator commands (PR7)

### `crm-mac messages backfill --restart`

Reset the messages cursor to install-time state so the daemon re-walks
all historical messages back to the 2026-01-01 backfill floor. The
command refuses while the daemon is running (it would race with the
daemon's own cursor commits). Stop the daemon, run the command, then
restart:

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/xyz.spengrah.crm-mac.plist
crm-mac messages backfill --restart       # prompts for "yes"
crm-mac messages backfill --restart --yes # scripted / no prompt
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/xyz.spengrah.crm-mac.plist
```

This produces duplicate-but-deduplicated events on the Pi (event-log
`(source, source_id)` dedup absorbs the overlap; no contact-side
effect, but visible in `/api/v1/host/:id` logs).

### `crm-mac messages scan --identifier <handle> [--since 30d]`

Queue a one-shot backwards scan for a specific phone/email handle —
useful when a contact was added while the daemon was offline and the
heartbeat-driven diff didn't fire. The scan is persisted Pi-side via
the cursor JSON; the next daemon tick drains the queue. Same
daemon-running guard as `backfill`.

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/xyz.spengrah.crm-mac.plist
crm-mac messages scan --identifier "+1-555-123-4567" --since 30d
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/xyz.spengrah.crm-mac.plist
```

`--since` accepts a simple duration: `30d`, `12h`, `60m`, `3600s`.

## Daemon-running guard

PR7 introduces a POSIX advisory lock on
`~/Library/Application Support/crm-mac/daemon.pid`. The daemon
acquires it on start, the ops subcommands acquire-or-refuse it before
mutating cursor state. Stale PID recovery is automatic — if the
daemon crashed without releasing the lock, the next daemon startup
(or CLI op) detects the dead PID, unlinks the pidfile, and re-acquires.

## Live smoke test (PR7)

Pre-req: PR6-installed daemon paired to a live Pi; messages tick
disabled or running on its scheduled cadence.

1. **Pick a fixture contact** — one whose primary phone/email is in
   the canonical `known-identifiers` set.

2. **Pre-state — capture last_contacted on the Pi:**

   ```bash
   ssh raspberet 'docker exec crm-postgres psql -U crm_user -d personal_crm \
       -c "SELECT id, last_contacted FROM contact WHERE id = '<uuid>';"'
   ```

3. **Install the PR7 daemon binary:**

   ```bash
   make mac-daemon
   launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/xyz.spengrah.crm-mac.plist
   cp .build/release/crm-mac ~/Library/Application\ Support/crm-mac/bin/
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/xyz.spengrah.crm-mac.plist
   ```

4. **Send a real iMessage** in both directions to the fixture contact
   (outbound + inbound). Two-direction proves direction inference is
   wired.

5. **Wait two messages-tick cadences** (~3 minutes).

6. **Verify events arrived on the Pi:**

   ```bash
   ssh raspberet 'sudo journalctl -u personalcrm-backend --since "10 min ago" --no-pager' \
       | grep -i raw_message
   ```

   Expect: at least two `raw_message.received` / `raw_message.sent`
   events with `accepted=1`.

7. **Verify the interaction row** was created on the Pi:

   ```bash
   ssh raspberet 'docker exec crm-postgres psql -U crm_user -d personal_crm \
       -c "SELECT id, contact_id, source, direction, occurred_at \
           FROM interaction \
           WHERE contact_id = '<uuid>' AND source = '"'"'messages'"'"' \
           ORDER BY occurred_at DESC LIMIT 5;"'
   ```

   Expect: ≥ 1 rows with `source=messages`, sane direction, recent
   `occurred_at`.

8. **Verify last_contacted advanced:**

   ```bash
   ssh raspberet 'docker exec crm-postgres psql -U crm_user -d personal_crm \
       -c "SELECT last_contacted FROM contact WHERE id = '<uuid>';"'
   ```

9. **Verify the messages source health:**

   ```bash
   crm-mac status
   ```

   Expect: messages source `live_cursor` advanced; `backfill_complete`
   either false (still descending) or true; no `last_error`.

## Limitations (v1)

- **Daemon-down + contact added → no auto-scan.** Plan §R9: the
  daemon's known-identifiers cache hash detects offline contact-list
  changes but does NOT auto-queue a 30-day scan for every new
  identifier (it would defeat the optimization on every restart). Run
  `crm-mac messages scan --identifier <X>` manually if you want
  backfill for a specific newly-added contact.
- **Outbound group attribution.** For outbound group messages the
  daemon attributes the outreach to the first non-self handle by
  `chat_handle_join.ROWID` order. This is a v1 simplification: every
  outbound group message attributes to the same arbitrary peer, so
  one group member's `last_outreach_at` advances on every outbound
  group message while others' do not.
- **Outbound messages currently not emitted.** The reader's fetch
  query joins on `message.handle_id`, which is NULL for outbound rows
  in chat.db; outbound peer resolution requires a separate path
  through `outboundGroupPeer`, deferred to a follow-up. Inbound
  emission is fully wired.
