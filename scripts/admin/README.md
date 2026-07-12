# Admin scripts

Operator scripts for one-off recovery and host-standup scenarios that
the production code path deliberately does not automate. Destructive
scripts prompt for confirmation before running; idempotent,
non-destructive installers (e.g. `setup-staging-reseed-host.sh`) do not.

## reset_icloud_contacts.sh

Hard-deletes all iCloud Contacts state (`external_contact` rows and
event-log rows where `source = 'icloud_contacts'`).

**When to run:** the Mac daemon has been re-paired onto the same Pi
under a fresh `host_id` (typically because the previous Mac was
replaced or its Keychain was wiped) and the new daemon's full
CNContactStore resync appears to be a no-op — i.e. `/known-ids`
returns empty but the daemon's emitted upserts are being absorbed by
the event log's `(source, source_id)` dedup.

This happens because the `external_contact` upsert uses
`COALESCE(host_id, EXCLUDED.host_id)`: rows already owned by the
previous host's non-NULL `host_id` are preserved unchanged, so the
new host's `/known-ids` filter (`WHERE host_id = new_host_id`)
returns empty. The new host's emits dedup-absorb at the event log
because the entity content hasn't changed, so they never overwrite
the prior ownership. (Rows that happen to be legacy NULL — created
before migration 052 or by sources that don't set `host_id` — self-
heal on first new-host emit and do not need this script.)

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

## setup-staging-reseed-host.sh

Provisions the **staging** host (`STAGING_HOST`) for the develop→staging
auto-reseed: installs the three reseed scripts to `/usr/local/sbin`
(`root:root`, `0755`) and grants the staging GitHub Actions runner the
**two** NOPASSWD sudoers lines it invokes from `deploy-staging.yml`:

```
<RUNNER_USER> ALL=(root) NOPASSWD: /usr/local/sbin/staging-reseed.sh
<RUNNER_USER> ALL=(root) NOPASSWD: /usr/local/sbin/staging-deployed-sha.sh
```

`staging-reset.sh` is installed too (it is `exec`'d by
`staging-reseed.sh`, already root) but gets **no** sudoers line — the
runner never sudo-invokes it directly, so only the two wrappers it
actually calls are granted. The sudoers lines are **args-free** (any-args
form; both wrappers ignore args and `staging-deployed-sha.sh` is
read-only) and carry **no** `SETENV`/`env_keep`, preserving the env-trust
seam that pins the staging tenant identity inside the wrappers (sudo
resets the environment; nothing is passed from the workflow).

**When to run:** once, after the staging code-deploy standup is complete
and before the first seed-touching staging deploy is expected to actually
reseed.

**Two modes (mirrors `staging-reset.sh`):** there is no repo checkout on
the staging host, so the **default** mode runs from a dev Mac and
provisions `STAGING_HOST` over ssh — it ships the
installer + the three source scripts to a temp dir on the host and
re-invokes itself there with `--local` (the temp dir is removed
afterward). `ssh -t` allocates a TTY so `sudo` can prompt for a password.
The `--local` mode does the real install/sudoers work on the host itself
(this is what the ssh mode calls on the far side); run it directly only
if you already have a checkout on the host.

**Preconditions (fail-loud):**
- *ssh mode:* `ssh` + `tar` on PATH; the three source scripts present in
  this checkout; `STAGING_HOST` reachable over ssh.
- *`--local` mode:* must run as root; the staging runner user must exist
  (default `gha-runner`; override with `RUNNER_USER=…` — the account the
  `[self-hosted, staging]` agent runs as); and
  `/usr/local/sbin/deploy-staging.sh` must already be installed (this
  script does **not** install it — a host missing it is only partially
  stood up, so the script refuses).

This is the **staging** standup, not the Pi/prod runner install — the
runbook (`infra/runner-installation-runbook.md`) is only a *pattern
reference* for the `install`/`visudo` mechanics; staging differs
(`[self-hosted, staging]` label, `deploy-staging.sh`, the reseed
wrappers).

**Usage:**
```bash
# From a dev Mac (default): provisions STAGING_HOST over ssh
./scripts/admin/setup-staging-reseed-host.sh

# Non-default host and/or runner account:
STAGING_HOST=my-staging RUNNER_USER=my-runner ./scripts/admin/setup-staging-reseed-host.sh

# On the staging host itself, from a checkout, as root:
sudo ./scripts/admin/setup-staging-reseed-host.sh --local
```

In ssh mode `RUNNER_USER` is threaded to the host as a `--runner-user`
flag (args survive `sudo`; env does not — no `SETENV` needed). The script
is idempotent (install overwrites in place; the sudoers drop-in is a
fixed-name file overwritten atomically — no appends, no duplicate lines)
and safe to re-run. The sudoers drop-in is validated with `visudo -cf`
before install and re-validated after.

## Same-host reinstall (no script needed)

The Mac daemon stores its `host_id` in macOS Keychain. A standard
reinstall preserves the Keychain, so the daemon comes up with the
same `host_id`, `/known-ids` returns the existing rows, and the
full-resync is a no-op (every emit dedup-absorbs). No script needed.

Only Keychain-loss (a wipe-and-reinstall) triggers the re-pair flow
that the operator script handles.
