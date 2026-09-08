# Offsite backup runbook

The Pi uploads encrypted database and environment snapshots every night, and a weekly user timer restores the newest database object into a scratch database before checking its migration version and live-contact count.

## Prerequisites

Run these commands on the Debian Pi as an administrator:

```bash
sudo apt update
sudo apt install age rclone zstd
```

The backup service runs as the `crm` user through rootless Podman. Confirm that the postgres container is reachable from that user's session before installing the timers.

## Create the private bucket and retention rules

Create one Backblaze B2 bucket with these settings:

- Bucket type: private.
- Object lock: disabled.
- Default encryption: off.
- Lifecycle rules: exactly the two rules below.

| Prefix | Hide after upload | Delete after hiding |
| --- | ---: | ---: |
| `personal_crm-` | 30 days | 1 day |
| `personalcrm-env-` | 30 days | 1 day |

B2 buckets default to keeping all versions. Replace that default with the two lifecycle rules, or the bucket will grow without the intended retention limit.

Apply the rules with an administrator credential through the B2 console or the `b2_update_bucket` API. The request must use the named bucket's id and include both prefixes, `daysFromUploadingToHiding: 30`, and `daysFromHidingToDeleting: 1`. Keep object lock disabled and the bucket default encryption mode set to `none`.

## Create the restricted application key

The console presets include `deleteFiles`, so create the key through the API with an explicit capability list. Generate a master application key in the console (Application Keys, "Generate New Master Application Key") and store it in a protected two-line file: key id on the first line, application key on the second. The commands below read secrets from that file and never echo them.

```bash
umask 077
B2_MASTER_KEY_FILE=/secure/path/b2-master-key
B2_ACCOUNT_ID="$(sed -n 1p "$B2_MASTER_KEY_FILE")"
B2_APPLICATION_KEY="$(sed -n 2p "$B2_MASTER_KEY_FILE")"
B2_BUCKET_NAME='<bucket>'

B2_AUTH_JSON="$(curl -fsS https://api.backblazeb2.com/b2api/v2/b2_authorize_account \
  -u "$B2_ACCOUNT_ID:$B2_APPLICATION_KEY")"
B2_AUTH_TOKEN="$(printf '%s' "$B2_AUTH_JSON" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["authorizationToken"])')"
B2_API_URL="$(printf '%s' "$B2_AUTH_JSON" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["apiInfo"]["storageApi"]["apiUrl"])')"
B2_BUCKET_JSON="$(curl -fsS "$B2_API_URL/b2api/v2/b2_list_buckets" \
  -H "Authorization: $B2_AUTH_TOKEN" \
  --data "{\"accountId\":\"$B2_ACCOUNT_ID\",\"bucketName\":\"$B2_BUCKET_NAME\"}")"
B2_BUCKET_ID="$(printf '%s' "$B2_BUCKET_JSON" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["buckets"][0]["bucketId"])')"

curl -fsS "$B2_API_URL/b2api/v2/b2_create_key" \
  -H "Authorization: $B2_AUTH_TOKEN" \
  --data "{\"accountId\":\"$B2_ACCOUNT_ID\",\"keyName\":\"personalcrm-backup\",\"capabilities\":[\"listBuckets\",\"listFiles\",\"readFiles\",\"writeFiles\"],\"bucketId\":\"$B2_BUCKET_ID\"}" \
  > /secure/path/restricted-backup-key.json
unset B2_AUTH_JSON B2_AUTH_TOKEN B2_API_URL B2_BUCKET_JSON B2_BUCKET_ID
```

Protect the response file because it contains the new key secret (`applicationKeyId` and `applicationKey`). Use only `listBuckets`, `listFiles`, `readFiles`, and `writeFiles`; do not add `deleteFiles`. Once the restricted key exists, delete the master key file and revoke or vault the master key.

## Configure rclone for the crm user

Write the remote directly as the service user; `rclone config` is interactive and adds nothing here:

```bash
sudo -u crm install -d -o crm -g crm -m 0700 /var/lib/personalcrm/.config/rclone
sudo -u crm install -m 0600 /dev/null /var/lib/personalcrm/.config/rclone/rclone.conf
sudoedit -u crm /var/lib/personalcrm/.config/rclone/rclone.conf
```

```ini
[<rclone-remote>]
type = b2
account = <restricted applicationKeyId>
key = <restricted applicationKey>
```

The service reads `/var/lib/personalcrm/.config/rclone/rclone.conf` through the crm user's `HOME` and the remote name from `BACKUP_REMOTE`.

## Create the age keypair

Create the X25519 identity at the default path and keep its permissions private:

```bash
sudo -u crm install -d -o crm -g crm -m 0700 /var/lib/personalcrm/.config/personalcrm-backup
sudo -u crm HOME=/var/lib/personalcrm age-keygen -o /var/lib/personalcrm/.config/personalcrm-backup/age.key > /dev/null
sudo chown crm:crm /var/lib/personalcrm/.config/personalcrm-backup/age.key
sudo chmod 0600 /var/lib/personalcrm/.config/personalcrm-backup/age.key
sudo -u crm age-keygen -y /var/lib/personalcrm/.config/personalcrm-backup/age.key
```

Put the final command's recipient in `AGE_RECIPIENT`. Copy the identity file into the password manager as a recovery secret, and keep the password-manager copy independent of the Pi.

## Configure the environment and install the timers

Copy the example and edit it as the `crm` user or administrator:

```bash
sudo install -o crm -g crm -m 0600 infra/backup/backup.env.example /srv/personalcrm/backup.env
sudoedit /srv/personalcrm/backup.env
sudo ./infra/backup/install.sh
```

Set `BACKUP_REMOTE`, `BACKUP_BUCKET`, and `AGE_RECIPIENT` to the values created above. The example contains every supported variable and keeps the remaining defaults. The installer refuses to run until `/srv/personalcrm/backup.env` exists, copies the scripts and user units, reloads the user manager, enables both timers, and prints their schedules.

## Run the first backup and verify

Run a manual backup as the service user from a directory it can access:

```bash
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; /srv/personalcrm/bin/backup-offsite.sh'
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; /srv/personalcrm/bin/verify-offsite-backup.sh'
```

The backup command prints one line for each non-empty object. The verify command prints the selected object, its age, the migration version, and both contact counts. Confirm that the service and verify timers remain enabled after this first run.

## Read back the lifecycle rules

rclone 1.60.1 does not provide `rclone backend lifecycle`. Read the bucket rules with `b2_list_buckets` while authenticated as the restricted key:

```bash
umask 077
B2_KEY_ID="$(sed -n 's/^account = //p' /var/lib/personalcrm/.config/rclone/rclone.conf)"
B2_APPLICATION_KEY="$(sed -n 's/^key = //p' /var/lib/personalcrm/.config/rclone/rclone.conf)"
B2_BUCKET_NAME='<bucket>'
B2_AUTH_JSON="$(curl -fsS https://api.backblazeb2.com/b2api/v2/b2_authorize_account \
  -u "$B2_KEY_ID:$B2_APPLICATION_KEY")"
B2_AUTH_TOKEN="$(printf '%s' "$B2_AUTH_JSON" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["authorizationToken"])')"
B2_ACCOUNT_ID="$(printf '%s' "$B2_AUTH_JSON" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["accountId"])')"
B2_API_URL="$(printf '%s' "$B2_AUTH_JSON" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["apiInfo"]["storageApi"]["apiUrl"])')"
curl -fsS "$B2_API_URL/b2api/v2/b2_list_buckets" \
  -H "Authorization: $B2_AUTH_TOKEN" \
  --data "{\"accountId\":\"$B2_ACCOUNT_ID\",\"bucketName\":\"$B2_BUCKET_NAME\"}"
```

Check that the response names the bucket and shows exactly the `personal_crm-` and `personalcrm-env-` rules with the intended hide and delete delays. This confirms the lifecycle policy without granting the Pi credential any lifecycle-management capability.

## Restore to the Pi

A scratch restore leaves the live database untouched:

```bash
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; /srv/personalcrm/bin/restore-offsite.sh personal_crm-<timestamp>.sql.zst.age'
```

The over-production sequence is intentionally manual because it replaces live data. Confirm the object name and its age first, stop the backend, drop and recreate `personal_crm` while connected to `postgres`, restore with the live object name, and start the backend:

```bash
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; systemctl --user stop personalcrm-backend.service'
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; podman exec -i crm-postgres psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS personal_crm"'
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; podman exec -i crm-postgres psql -U "$PG_USER" -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE personal_crm"'
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; /srv/personalcrm/bin/restore-offsite.sh personal_crm-<timestamp>.sql.zst.age personal_crm'
sudo -u crm HOME=/var/lib/personalcrm bash -c 'cd /tmp; set -a; . /srv/personalcrm/backup.env; set +a; systemctl --user start personalcrm-backend.service'
```

The restore script never drops a database. It creates a target only when that target does not exist, and it defaults to `personal_crm_restore`.

## Recover a hidden object

`rclone deletefile` and a failed upload's cleanup hide objects rather than deleting them, and the lifecycle rule hides objects 30 days after upload. A hidden object stays recoverable until the rule deletes it one day later. List hidden versions and copy one back under a plain name, then restore it as usual:

```bash
sudo -u crm HOME=/var/lib/personalcrm rclone lsf --b2-versions <rclone-remote>:<bucket>
sudo -u crm HOME=/var/lib/personalcrm rclone copyto --b2-versions <rclone-remote>:<bucket>/personal_crm-<timestamp>.sql.zst-v<version>.age /tmp/personal_crm-<timestamp>.sql.zst.age
```

The restore script reads from the bucket only, so re-upload the recovered file with `rclone copyto /tmp/<name> <rclone-remote>:<bucket>/<name>` before running it, or decrypt the local copy by hand with `age -d -i <identity> | zstd -d | psql`.

## Restore to a fresh machine

Install PostgreSQL, `age`, `rclone`, and `zstd` on the replacement machine. Recover the age identity from the password manager, configure an rclone remote that can read the bucket, and copy `scripts/restore-offsite.sh` to the machine. Set `AGE_IDENTITY_FILE`, `BACKUP_REMOTE`, and `BACKUP_BUCKET` in the environment. Set `PG_EXEC=env` when the replacement uses a local `psql` command rather than Podman, then set `PG_USER` to the local PostgreSQL role. Run the script with a timestamped database object and a new target database name.

## Check failed timer units

Unit state is read as the `crm` user; unit logs are read as root, because the crm user has no journal access and journald on the Pi is volatile (logs do not survive a reboot):

```bash
cd /tmp && sudo -u crm HOME=/var/lib/personalcrm XDG_RUNTIME_DIR=/run/user/<uid> DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus systemctl --user --failed
sudo journalctl _SYSTEMD_USER_UNIT=personalcrm-backup.service -n 50 --no-pager
sudo journalctl _SYSTEMD_USER_UNIT=personalcrm-backup-verify.service -n 50 --no-pager
```

The backup service log identifies upload, cleanup, and object-size failures. The verify service log identifies stale objects, restore failures, migration mismatches, and contact-count failures. Re-run either job by hand with the commands under "Run the first backup and verify".
