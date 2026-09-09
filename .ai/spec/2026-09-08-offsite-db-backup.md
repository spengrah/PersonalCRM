# Off-device encrypted database backups

Date: 2026-09-08
Status: BUILT (PR #846). Rollout on the Pi follows `infra/backup/README.md`; [#15](https://github.com/spengrah/PersonalCRM/issues/15) tracks it and closes when the acceptance criteria below are met.
Implements: [#15](https://github.com/spengrah/PersonalCRM/issues/15).

## Context & problem

Prod runs on one Raspberry Pi. The only backups are cold volume copies taken by `scripts/backup-db.sh` before each deploy and two hand-made dumps in `/srv/personalcrm/backups`. All of them live on the same NVMe as the database, so a disk failure, theft, or a bad `rm` loses the CRM and every copy of it at once. The README's backup section still documents a `docker exec pg_dump` command that does not match the rootless Podman runtime.

The database is small. After the sync-log cleanup (migration 081) it is about 100 MB on disk; a plain `pg_dump` measured 63 MB and 12 MB after `zstd -3` (11 MB at level 9, 9.6 MB at level 19, so higher levels buy little). That rules out any need for deduplicating backup tools: a full dump per night for a month is under 400 MB, inside Backblaze B2's 10 GB free tier with room to grow by more than an order of magnitude.

## What matters (priority order)

1. **Privacy.** The dump is encrypted on the Pi before it leaves. The storage provider holds ciphertext and a filename; the provider's own encryption is not trusted or relied on.
2. **Survives loss of the Pi.** Restoring needs only the B2 bucket, the age private key from the password manager, and a machine with `age`, `zstd`, and `psql`.
3. **Survives compromise of the Pi.** The credential on the Pi can write and read the bucket but cannot destroy history. Retention is enforced by the bucket, not by the client.
4. **Proven restorable.** A backup nobody has restored is a hope. A weekly job restores the newest object into a scratch database and asserts on it.
5. **Zero cost, minimal machinery.** Four standard tools in a pipe, two systemd user timers, no daemon, no repository format, no new Go code.

## Goals

- A nightly job on the Pi that streams `pg_dump` through `zstd` and `age` into a uniquely named object in a private B2 bucket, with no plaintext or ciphertext written to local disk.
- Retention of 30 days enforced server-side by a B2 lifecycle rule.
- A B2 application key restricted to one bucket with no `deleteFiles` capability.
- A weekly verify job that downloads the newest object, decrypts, restores into a scratch database, asserts on it, and fails if the newest object is older than 36 hours.
- A restore script and README runbook covering restore to the Pi and restore to a fresh machine.
- All secrets live on the Pi under the `crm` tenant and in the password manager; the repo carries only an example env file.

## Non-Goals

- No point-in-time recovery, WAL archiving, pgBackRest, or WAL-G. One consistent snapshot per night is the granularity.
- No kopia or restic. Revisit only if 31 compressed dumps approach the free tier, which needs a compressed dump near 300 MB.
- No S3 object lock. The no-delete key plus lifecycle rule covers the threat model; object lock adds S3-API configuration for a guarantee this single-user deployment does not need.
- No change to `scripts/backup-db.sh` or `scripts/restore-db.sh`. The pre-deploy cold volume copy is a fast local rollback anchor and stays as it is.
- No backup of the staging tenant. Staging is reseeded from synthetic data.
- No alerting channel beyond the systemd unit state and an optional dead-man's-switch ping (see Open decisions).

## Design

### Pipeline

The backup job runs as the `crm` user and is one pipe:

```sh
podman exec crm-postgres pg_dump -U crm_user -Fp personal_crm \
  | zstd -3 \
  | age -r "$AGE_RECIPIENT" \
  | rclone rcat "b2:${B2_BUCKET}/personal_crm-$(date -u +%Y%m%dT%H%M%SZ).sql.zst.age"
```

`pg_dump` runs inside the postgres container so the client version always matches the server and no Postgres client is installed on the host. Plain format is used instead of custom format because `zstd` compresses it better than `pg_dump`'s built-in gzip and the restore side is `psql`, which the verify job needs anyway. `rclone rcat` streams stdin to the object, so no temp file exists on the Pi at any point; for an unknown-length stream above `--streaming-upload-cutoff` rclone buffers one chunk (`--b2-chunk-size`, default 96 MiB) in RAM at a time, which is the job's memory ceiling. `set -o pipefail` makes a failure anywhere in the pipe fail the unit.

The pipe and its restore inverse were exercised end to end against a real prod dump through an rclone `local` remote: the restored plaintext hashed identical to the original, a single flipped byte in the stored object fails `age -d` with an authentication error, and a truncated object fails both `age -d` and `zstd -d`. Corruption in transit or at rest is therefore detected by the format, not by a separate checksum.

A second, much smaller object `personalcrm-env-<timestamp>.age` carries `/srv/personalcrm/.env` encrypted the same way, so a restore to a fresh machine has the database password and OAuth client configuration without a second recovery path. `/etc/caddy/crm.env` is root-owned and holds one API key that is recreated by hand; it is not backed up.

### Encryption

`age` with an X25519 keypair. The recipient (public key) is configuration and lives in the env file. The identity (private key) lives at `/var/lib/personalcrm/.config/personalcrm-backup/age.key`, mode 0600, owned by `crm`, and in the password manager. The key on the Pi exists so the verify job can decrypt; an attacker with `crm` access already has the live database, so the on-Pi key does not widen exposure. The key's job is to make the B2 copy useless to Backblaze or to anyone who obtains the B2 credential.

### Storage, retention, and immutability

One private B2 bucket, accessed through rclone's `b2` backend. The application key is restricted to that bucket with capabilities `listBuckets`, `listFiles`, `readFiles`, `writeFiles` and no `deleteFiles`. Object names carry a UTC timestamp, so a run never overwrites a previous object.

Retention is a bucket lifecycle rule: `daysFromUploadingToHiding` 30, `daysFromHidingToDeleting` 1, prefix `personal_crm-` and a matching rule for the env objects. Lifecycle rules run once per day and every field has a minimum of 1, so objects live 30 to 32 days. B2 buckets default to "keep all versions" with no lifecycle rule, which would grow forever; setting the rule is a required setup step and the acceptance test checks for it. Deleting a file version needs `deleteFiles`, which the key lacks. Hiding a file (B2's soft delete) needs only `writeFiles`, and rclone's `b2` backend hides by default rather than hard-deleting. A hidden file is listable with `rclone --b2-versions` and copyable back out by its versioned name until the lifecycle rule deletes it, so the worst an attacker or a mistaken command can do with the Pi's credential is a reversible hide.

All of the above was observed against a real bucket with a key holding exactly `listBuckets, listFiles, readFiles, writeFiles`, using both rclone 1.71.2 and 1.60.1 (the Debian 12 package on the Pi): a 50 MB unknown-length stream and a 2 KB stream both upload; `rclone delete` succeeds as a hide; the hidden version appears under `--b2-versions` as `name-v<timestamp>.ext` and copies back out; `delete --b2-hard-delete`, `cleanup`, a lifecycle write, and bucket deletion are all refused with 401; a second upload to the same name creates a new version and keeps the old one. Bucket-restricted keys require `b2_list_buckets` to name the bucket, and both rclone versions do so from the remote path.

`rclone backend lifecycle` does not exist in rclone 1.60.1, so the runbook's lifecycle readback uses `b2_list_buckets` directly with `curl` and the restricted key, which returns the rules for the named bucket.

### Verification

A weekly job proves restorability with a real restore:

1. List the bucket, pick the newest `personal_crm-*.sql.zst.age`. If its timestamp is older than 36 hours, fail.
2. Stream it through `rclone cat`, `age -d`, `zstd -d`, and `psql` into a fresh scratch database `personal_crm_verify` in the live postgres container.
3. Assert the restored `schema_migrations` version equals the live database's version and the live-contact count is greater than zero and within a few percent of the live count.
4. Drop the scratch database. The unit fails on any step, and a failed unit is visible in `systemctl --user --failed` in the `crm` tenant.

The verify job downloads about one dump a week, well inside B2's free egress allowance.

### Scheduling

Two systemd user units and timers in the `crm` tenant, installed next to the Quadlets: `personalcrm-backup.timer` (daily, `Persistent=true` so a missed night runs on next boot) and `personalcrm-backup-verify.timer` (weekly). The tenant already has lingering enabled for the Quadlet services. Both services read `/srv/personalcrm/backup.env` for the bucket name and age recipient and use `/var/lib/personalcrm/.config/rclone/rclone.conf` for the B2 credential.

### Restore

`scripts/restore-offsite.sh <object-name> [<target-db>]` streams one object from B2 through `age -d` and `zstd -d` into `psql` against a named database, defaulting to a scratch name so a careless invocation cannot overwrite the live database. Restoring over prod is a documented sequence in the README, not a default: stop the backend, drop and recreate `personal_crm`, run the script with the live name, start the backend. Restore to a fresh machine needs the age key from the password manager, an rclone remote configured with a read-capable B2 key, and the same script.

## Components

| Piece | Location |
| --- | --- |
| Backup job | `scripts/backup-offsite.sh` |
| Verify job | `scripts/verify-offsite-backup.sh` |
| Restore | `scripts/restore-offsite.sh` |
| Units and timers | `infra/backup/personalcrm-backup.{service,timer}`, `infra/backup/personalcrm-backup-verify.{service,timer}` |
| Config template | `infra/backup/backup.env.example` (bucket name and age recipient placeholders) |
| Setup runbook | `infra/backup/README.md`: create bucket, lifecycle rule, restricted key, age keypair, rclone remote, install units |
| Operator docs | README backup section rewritten to point at the runbook and the restore sequence |

Secrets that never enter the repo: the B2 application key, the age private key, the bucket name.

## Testing

The three scripts share one round-trip test in the deploy-scripts lane (`make test-deploy-scripts`): against the local test Postgres, with a throwaway age keypair and an rclone `local` remote standing in for B2, back up a seeded database, run verify against the local remote, restore into a second database, and assert the row counts match. Falsification for the verify job: corrupt one byte of the stored object and confirm verify exits non-zero; set the object's timestamp 48 hours back and confirm the freshness check fails. Record the injected defects and exit codes in the PR body per the repo's ephemeral second-order rule.

Setup steps that only exist on B2 (lifecycle rule, key capabilities) are checked once by hand during rollout with `b2_list_buckets` and `b2_list_keys` calls and recorded in the runbook, not tested in CI. The console's key presets all include `deleteFiles`, so the restricted key is created through `b2_create_key` with an explicit capability list; the runbook carries that script.

## Acceptance criteria

Mapped to #15's checklist:

- Backup script created: `scripts/backup-offsite.sh` runs nightly from the timer and a new object appears in the bucket.
- Restore script created: `scripts/restore-offsite.sh` restores a chosen object into a named database.
- Cron setup documented: `infra/backup/README.md` covers unit installation and B2 setup.
- Backup verification works: `personalcrm-backup-verify.service` has run green at least once against a real B2 object.
- 30-day retention: the lifecycle rule is set and confirmed via `b2_list_buckets`.
- Privacy: the bucket contains only `.age` objects; a fresh-machine restore has been performed once from the bucket plus the password-manager key.

## Settled decisions

- **Pre-deploy offsite copy.** Every prod deploy starts a separate oneshot backup with `BACKUP_KIND=predeploy` after both new image pulls succeed and before migrate-check, and continues after reporting a failure through the deploy notification path. Its database objects use the `personal_crm_predeploy-` prefix with the same 30-day lifecycle rule as the other two prefixes, while the environment object keeps its existing name. The underscore keeps the B2 lifecycle prefixes disjoint and leaves the verifier's `personal_crm-*.sql.zst.age` listing blind to pre-deploy copies, so a dead nightly timer still fails the freshness check.
- **Failure notification: ntfy, on failure only.** Both units declare `OnFailure=personalcrm-ntfy-failure@%n.service`, which posts the unit name and host to the same ntfy topic the deploy script uses. The topic has one writer, `/etc/personalcrm/ntfy.env`, which the installer makes readable by the tenant rather than copying the value into a second file. Bodies carry no database facts, because verify's error text quotes row counts and an ntfy topic is a bearer token; the reader is pointed at the journal instead.
- **No dead-man's switch.** A push relay reports events and cannot report silence, so a run that never happens sends nothing. Accepted deliberately: the timers live in the same user tenant as the CRM containers, so the failure modes that would stop them silently (the tenant down, the Pi down, lingering off) take the CRM down too and announce themselves. What remains is deliberate action, and at that point a missed backup is not the problem. The weekly verify's 36-hour freshness check still converts a quiet run of missed backups into a real failure, which does notify.
- **Nightly window.** 03:30 local with up to 10 minutes of jitter, after the sync jobs' quiet period. Verify runs Sundays at 05:00.
