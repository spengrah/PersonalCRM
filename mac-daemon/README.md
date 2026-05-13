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
