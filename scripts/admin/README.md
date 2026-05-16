# Admin scripts

Destructive operator scripts for one-off recovery scenarios that the
production code path deliberately does not automate. Each script
prompts for confirmation before running.

## reset_icloud_contacts.sh

Hard-deletes all iCloud Contacts state (`external_contact` rows and
event-log rows where `source = 'icloud_contacts'`).

**When to run:** the Mac daemon has been re-paired onto the same Pi
under a fresh `host_id` (typically because the previous Mac was
replaced or its Keychain was wiped) and the new daemon's full
CNContactStore resync appears to be a no-op — i.e. `/known-ids`
returns empty but the daemon's emitted upserts are being absorbed by
the event log's `(source, source_id)` dedup.

This happens because `external_contact.host_id` is set on first
INSERT only, so the previous host's rows survive revoke with the
original ownership. The new host's `/known-ids` filter (`WHERE host_id
= new_host_id`) returns empty, but the entity content hasn't changed,
so the new emits dedup-absorb and never create rows owned by the new
host.

**What gets deleted:**
- Every `external_contact` row with `source = 'icloud_contacts'`
  (live and tombstoned). User-curated state (`crm_contact_id`,
  `match_status='imported'/'ignored'`) is destroyed.
- Every `event` row with `source = 'icloud_contacts'`. The cursor
  history for the iCloud Contacts source is reset; the daemon's next
  sync starts from scratch.

**What stays:** other sources (gcontacts, gcal_attendee, telegram,
messages), the `mac_host` table, identity rows. The daemon's next
full resync repopulates identities via the normal upsert handler.

**Why not automatic:** automating this on `RevokeHost` would (a)
violate the append-only event-log invariant the bus design depends on,
and (b) destroy user decisions that PR5's revive contract goes out of
its way to preserve. The trade-off is documented: re-pair onto a
fresh `host_id` is a documented operator step.

**Usage:**
```bash
# From the developer machine (default — assumes SSH to the Pi):
PI_HOST=raspberry-pi ./scripts/admin/reset_icloud_contacts.sh

# Or from the Pi itself:
LOCAL=1 ./scripts/admin/reset_icloud_contacts.sh
```

The script:
1. Prints the row counts about to be deleted.
2. Prompts for `yes` confirmation.
3. Runs the two `DELETE` statements via `docker exec crm-postgres
   psql`.

After the script finishes, the daemon's next full resync repopulates
the rows with `host_id` set to the currently-paired host.

## Same-host reinstall (no script needed)

The Mac daemon stores its `host_id` in macOS Keychain. A standard
reinstall preserves the Keychain, so the daemon comes up with the
same `host_id`, `/known-ids` returns the existing rows, and the
full-resync is a no-op (every emit dedup-absorbs). No script needed.

Only Keychain-loss (a wipe-and-reinstall) triggers the re-pair flow
that the operator script handles.
