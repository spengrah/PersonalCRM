# crm-mac — Personal CRM Mac daemon

Single-binary Swift daemon that ingests Apple Messages + iCloud Contacts from a Mac and pushes events to the Pi-side Personal CRM API.

See `../.ai/spec/mac-daemon.md` for the authoritative scope. This README covers local install + smoke procedures only.

## Requirements

- macOS 14+ (the package targets `macOS(.v14)`; SMAppService requires macOS 13+ at minimum).
- Xcode Command Line Tools or a full Xcode install. `swift test` requires XCTest (full Xcode); `swift build` plus the bundle-assembly script work on CLT only.
- A running Pi-side `crm-api` reachable from the Mac (typically via Tailscale).

## Build

From the repo root:

```bash
make mac-daemon
```

This runs `swift build -c release` from `mac-daemon/`, then assembles + codesigns `crm-mac.app` via `mac-daemon/Scripts/assemble_bundle.sh`. Output: `mac-daemon/.build/release/crm-mac.app`. The script uses CLT-shipped tools only (no `xcodebuild`).

For Full Disk Access grants that survive rebuilds without an Apple Developer ID, create a local Code Signing certificate in **Keychain Access → Certificate Assistant → Create a Certificate…**:

- Name: `CRM Mac Local Code Signing`
- Identity Type: `Self Signed Root`
- Certificate Type: `Code Signing`

Then build with the same identity every time:

```bash
CRM_MAC_CODESIGN_IDENTITY="CRM Mac Local Code Signing" make mac-daemon
```

If `CRM_MAC_CODESIGN_IDENTITY` is unset, the bundle is ad-hoc signed. Ad-hoc builds work, but every rebuild changes the CDHash and the designated requirement, so both FDA and Contacts grants must be re-granted after every rebuild.

Empirical behavior of cert-backed signing (validated against macOS Sequoia):

- **FDA grants persist** across rebuilds — the designated requirement is anchored on the certificate leaf, not the CDHash. Add the bundle to FDA once after the first cert-backed install, and subsequent rebuilds keep working without touching that pane.
- **Contacts grants do NOT persist** — TCC's Contacts subsystem appears to re-evaluate on CDHash change regardless of the designated requirement. Expect the system Contacts permission dialog after every rebuild. The daemon triggers this prompt automatically on its first iCloud tick (no menu hunting); click Allow once per rebuild.

If `xyz.spengrah.crm-mac` already appears toggled-on in the Contacts pane but a dialog still fires, that's the same quirk — let the prompt drive the regrant.

## Pair + install (manual smoke)

Replace `<pi-host>` with your Pi's reachable hostname and `<pi-url>` with the Pi's HTTPS base URL.

**On the Pi:** mint a single-use pairing token.

```bash
ssh <pi-host> "sudo -u crm bash -lc '
set -a
. /srv/personalcrm/.env
set +a
cd /srv/personalcrm
./backend/bin/crm-admin --mint-pairing-token --hostname-label mac-1
'"
# token=<plaintext-base64-rawurl>
# expires_at=2026-05-13T15:42:18Z
# hostname_label=mac-1
# note: paste into `crm-mac install --pair <token>` within 10 minutes
```

The pairing token is single-use and short-lived (10 minutes).

**On the Mac:** install the daemon. Run the binary from inside the freshly-built bundle so SMAppService picks up the bundle context. The installer re-runs bundle assembly + codesigning, so the env var MUST match what was used at build time — otherwise the installed bundle silently downgrades to ad-hoc and the first FDA grant is bound to a CDHash that the next rebuild invalidates:

```bash
CRM_MAC_CODESIGN_IDENTITY="CRM Mac Local Code Signing" \
    ./mac-daemon/.build/release/crm-mac.app/Contents/MacOS/crm-mac install \
    --pair <token> \
    --pi-url <pi-url> \
    --hostname mac-1
```

`--hostname` is REQUIRED on fresh install. Pick a non-PII label (`mac-1`, `work-mac`, `home-laptop`).

After install, macOS will surface the agent in **System Settings → General → Login Items → Allow in Background**. If the daemon fails to start, check that the "crm-mac" entry is toggled on. Ad-hoc fallback builds may surface a Gatekeeper prompt; allow it once.

The same env-var requirement applies to upgrades — the installer reassembles + signs the installed bundle in place:

```bash
CRM_MAC_CODESIGN_IDENTITY="CRM Mac Local Code Signing" \
    ./mac-daemon/.build/release/crm-mac.app/Contents/MacOS/crm-mac install --upgrade
```

Before testing the `messages` source, grant **Full Disk Access** to the installed bundle:

1. Open **System Settings → Privacy & Security → Full Disk Access**.
2. Click **+** and add `~/Library/Application Support/crm-mac/crm-mac.app`.
3. Ensure the new `crm-mac` entry is enabled.

Without this permission, the daemon cannot read `~/Library/Messages/chat.db` and the messages source reports `fda_required`.

## Verify

```bash
~/Library/Application\ Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac doctor
# PASS  api-key: present
# PASS  agent_service: registered (enabled)
# PASS  config_state: host=mac-1 schemaVersion=1
# PASS  pi_reachability: phones=N emails=N
```

Exit code = number of FAIL entries.

```bash
~/Library/Application\ Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac doctor | grep agent_service
```

We deliberately do NOT shell out to `launchctl print` for verification — SMAppService is the authoritative API and `agentService.currentStatus` surfaces the same state.

## Status

```bash
~/Library/Application\ Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac status
# installed=true
# registered=true
# registration_status=enabled
# hostname=mac-1
# pi_url=<pi-url>
# host_id=<uuid>
# state_schema_version=1
# last_heartbeat_at=2026-05-13T16:00:00Z
```

`registration_status` distinguishes `enabled`, `requires_approval`, `not_registered`, `not_found`.

## Reboot smoke

```bash
sudo shutdown -r now
# After login, wait ~60s for the heartbeat tick.
crm-mac status
```

## Upgrade

```bash
CRM_MAC_CODESIGN_IDENTITY="CRM Mac Local Code Signing" make mac-daemon
CRM_MAC_CODESIGN_IDENTITY="CRM Mac Local Code Signing" \
    ~/Library/Application\ Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac install --upgrade
```

`--upgrade` reads the existing `config.json` + api-key, stops the running daemon (SMAppService.unregister + SIGTERM-and-poll), backs up the existing bundle, assembles the new one at a tmp path, atomic-renames it into place, substitutes the install-prefix placeholder in the embedded LaunchAgents plist, and re-registers via SMAppService. It does NOT call `POST /host`.

The bundle ID (`xyz.spengrah.crm-mac`) is the TCC attribution key, but cross-rebuild grant persistence depends on the designated requirement. Use the same `CRM_MAC_CODESIGN_IDENTITY` for build and upgrade if you want **FDA** grants to survive rebuilds. **Contacts grants will still re-prompt** on every rebuild regardless (see Build section for details).

## Stop / start (maintenance windows)

Operator workflows that mutate cursor state (e.g. `messages backfill --restart`, `messages scan`) refuse while the daemon is running. Stop the daemon, run the op, then start:

```bash
crm-mac stop
crm-mac messages backfill --restart
crm-mac start
```

`crm-mac stop` calls SMAppService.unregister + SIGTERM the daemon process + polls the pidfile. `crm-mac start` re-registers via SMAppService + polls for the `.enabled` status.

## Recovery scenarios

**Pi unreachable during fresh install (network drop after Pi commit).** The pair tx may have committed before the daemon got the response back:

```bash
ssh <pi-host> "sudo -u crm bash -lc '
set -a
. /srv/personalcrm/.env
set +a
cd /srv/personalcrm
./backend/bin/crm-admin --list-hosts
./backend/bin/crm-admin --revoke-host <uuid>
'"
# Then re-mint a token and re-install.
```

**Post-pair persistence failure (api-key write, state write, or atomic-rename failed).**

```bash
crm-mac uninstall --purge
# Then on the Pi: --revoke-host <uuid>, re-mint, re-install.
```

**SMAppService.register failed (bundle in place; only registration step failed).** Address the underlying issue (typically: approve the agent in System Settings → Login Items), then:

```bash
crm-mac install --register-only
```

**Legacy bare-binary install detected.** The first run of a post-rewrite binary against an existing pre-rewrite install will trigger automatic migration: stop the legacy daemon, bootout the legacy launchd registration, assemble the new bundle, register, then delete the legacy plist + bare binary. Operator-facing UX: `crm-mac install --upgrade` Just Works. If the migration fails partway (e.g. legacy bootout reports the registration is still loaded), the operator manually runs `launchctl bootout gui/$(id -u)/xyz.spengrah.crm-mac` and re-runs `--upgrade`.

## Uninstall

```bash
crm-mac uninstall
# daemon_stopped=true
# unregister_invoked=true
# bundle_deleted=true
# keychain_deleted=true
# legacy_plist_deleted=false
# legacy_binary_deleted=false
# purged=false

crm-mac uninstall --purge
# Also removes config.json + state.json + logs + icloud hash cache.
```

The Pi-side `mac_host` row is NOT touched — run `crm-admin --revoke-host <uuid>` on the Pi if you want to remove the row.

## Where things live

| Item | Path |
|---|---|
| Bundle (after install) | `~/Library/Application Support/crm-mac/crm-mac.app` |
| Daemon binary inside bundle | `~/Library/Application Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac` |
| Embedded LaunchAgent plist | `~/Library/Application Support/crm-mac/crm-mac.app/Contents/Library/LaunchAgents/xyz.spengrah.crm-mac.plist` |
| config.json | `~/Library/Application Support/crm-mac/config.json` |
| state.json | `~/Library/Application Support/crm-mac/state.json` |
| api-key file | `~/Library/Application Support/crm-mac/api-key` |
| icloud_contacts_hashes.json | `~/Library/Application Support/crm-mac/icloud_contacts_hashes.json` |
| stdout/stderr logs | `~/Library/Logs/crm-mac/{stdout,stderr}.log` |
| Bundle identifier (TCC keying) | `xyz.spengrah.crm-mac` |
| Unified log subsystem | `xyz.spengrah.crm-mac` |

To tail the unified log:

```bash
log stream --predicate 'subsystem == "xyz.spengrah.crm-mac"' --info
```

## Troubleshooting

**FDA re-prompts after `make mac-daemon && crm-mac install --upgrade`.** TCC keys FDA grants on the bundle ID + designated requirement. Confirm the designated requirement is certificate-backed and stable across rebuilds:

```bash
codesign --display -r - ~/Library/Application\ Support/crm-mac/crm-mac.app 2>&1
# designated => identifier "xyz.spengrah.crm-mac" and certificate leaf = H"..."
```

If the requirement contains `cdhash`, the bundle was ad-hoc signed. Rebuild AND reinstall with `CRM_MAC_CODESIGN_IDENTITY="CRM Mac Local Code Signing"` set on both commands — the installer signs the bundle it copies into place, so missing the env var on the install step also produces an ad-hoc DR. If the requirement shows a different identifier (e.g. `crm-mac-<random>`), the two-pass codesign didn't take effect — debug `mac-daemon/Scripts/assemble_bundle.sh`.

**Contacts re-prompts after a rebuild.** Expected even with cert-backed signing — TCC's Contacts subsystem appears to bind to the CDHash regardless of the designated requirement. The daemon will fire the prompt itself on its next iCloud tick; click Allow once. Tracked separately; would require a real Apple Developer ID to resolve.

If everything looks stable but FDA still re-prompts, try moving the bundle to `~/Applications/crm-mac.app` (some macOS internals treat `~/Library/Application Support/` differently from `~/Applications/` for SMAppService-managed agents — undocumented).

## Testing

Unit + lifecycle tests run on CI via `.github/workflows/ci.yml` on `macos-15`. To run locally:

```bash
cd mac-daemon
swift test
```

`swift test` requires XCTest, which ships with the full Xcode (not the Command Line Tools subset).

A subset of opt-in tests exercises the real Foundation FilesystemAdapter + the bundle-assembly shell script. They are gated by an env var:

```bash
cd mac-daemon
CRM_MAC_INTEGRATION_TESTS=1 swift test \
    --filter 'BundleAssemblyParityTests|BundleSwapAtomicityTests'
```

CI runs these in a dedicated step after the regular `swift test` invocation.

You can also run the full local suite via the project Makefile:

```bash
make test-daemon-local
```

## Operator commands

### `crm-mac messages backfill --restart`

Reset the messages cursor. Stop the daemon, run the command, then start:

```bash
crm-mac stop
crm-mac messages backfill --restart       # prompts for "yes"
crm-mac messages backfill --restart --yes # scripted / no prompt
crm-mac start
```

### `crm-mac messages scan --identifier <handle> [--since 30d]`

Queue a one-shot backwards scan for a specific phone/email handle.

```bash
crm-mac stop
crm-mac messages scan --identifier "+1-555-123-4567" --since 30d
crm-mac start
```

`--since` accepts a simple duration: `30d`, `12h`, `60m`, `3600s`.

## Daemon-running guard

The daemon acquires a POSIX advisory lock on `~/Library/Application Support/crm-mac/daemon.pid` on start; the ops subcommands acquire-or-refuse it before mutating cursor state. Stale PID recovery is automatic — if the daemon crashed without releasing the lock, the next startup detects the dead PID, unlinks the pidfile, and re-acquires.

## Spec deviation notes

- **CI pinned to `macos-15`**, not `macos-latest`. Eliminates the silent image-migration risk; an image deprecation will surface as a loud CI failure in a follow-up PR.
- **TCC stability.** The bundle identifier `xyz.spengrah.crm-mac` is the responsible-process key TCC uses for the daemon. With `CRM_MAC_CODESIGN_IDENTITY` set to a stable self-signed Code Signing certificate, the designated requirement is cert-leaf-anchored and **FDA grants survive rebuilds**. **Contacts grants do not survive rebuilds** under cert-backed signing — TCC's Contacts subsystem appears to bind to the CDHash regardless of designated requirement (would need a real Apple Developer ID to resolve). Ad-hoc fallback builds get a CDHash-based requirement, so both FDA and Contacts grants reset on every rebuild.

## Limitations (v1)

- **Daemon-down + contact added → no auto-scan.** The daemon's known-identifiers cache hash detects offline contact-list changes but does NOT auto-queue a 30-day scan for every new identifier. Run `crm-mac messages scan --identifier <X>` manually if you want backfill for a specific newly-added contact.
- **Outbound group attribution.** For outbound group messages the daemon attributes the outreach to the first non-self handle by `chat_handle_join.ROWID` order. v1 simplification: every outbound group message attributes to the same arbitrary peer.
- **Outbound messages currently not emitted.** The reader's fetch query joins on `message.handle_id`, which is NULL for outbound rows in chat.db.
