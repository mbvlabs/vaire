# Phase 5 work units: master backup and disaster recovery

Source: MASTER's Phase 5 row, its decisions table (master backup), its risks (single point of failure, split brain on a rebuild) and "Still to decide"; IMPLEMENTATION's master backup and secrets sections; the RFC's "What the master backup holds", "The bundle", "Rebuilding the master" and its later-phases table; and the "What Phase 5 needs from Phase 4" list in [`PHASE-4-WORK-UNITS.md`](./PHASE-4-WORK-UNITS.md). This splits Phase 5 into small units we take one at a time. Each unit is one stacked branch off the previous one.

v1 (alpha) is Phases 0-5, so this is the last phase before a first release. The last section lists what else that release needs.

**Rules for every unit**

- Ends with `just lint` and a build for all targets. No local test harnesses, no docker. Pure functions (the bundle format, retention, the epoch decision, records pruning, ordering) get Go unit tests in the existing `_test.go` style, run with `go test ./...`; everything that needs a server goes in the manual test.
- Compiles and keeps the CLI coherent, but **doesn't have to be deployable** between units. The manual test runs once at the end (unit 18), on the server the Phase 4 manual test leaves behind, then on a VPS that replaces it.
- Linux and macOS for the CLI; Debian 13 and Ubuntu 26.04 only on servers. No compatibility code for older releases.
- Each step stays `-v1` (no release yet); a changed script or template goes into the step's input hash so the next `apply` re-runs it.
- "To verify on the VPS" items are listed on the unit they belong to and again in the checklist at the end.

---

## Implementation record (2026-10-15)

Current status is in [MASTER.md](MASTER.md): main alpha live qualification completed 2026-10-15–19, with coverage limits in [MANUAL-TESTING.md](MANUAL-TESTING.md). The dated build notes and units below are historical, not an active pending-test list. Draft release notes are now in [RELEASE-v1.0.0-alpha.1.md](RELEASE-v1.0.0-alpha.1.md).

Units 1-16 are built on `phase-5-master-backup` (not verified on a server; the manual test is unit 18). Every "Decided (recommended)" item was approved as written. Where the build differs from the units below:

- **The fence marker is `/etc/vaire/fenced`**, not `/var/lib/vaire/fenced` as first planned: the archive wrapper runs as `postgres`, which can't enter `/var/lib/vaire` (root 0700). `/etc/vaire` is 0755 and the file 0644.
- **`masterbackup` is a leaf package** (format, names, retention, dirty flag, epoch decision); the bucket calls live in `internal/backup/master.go` beside the other minio code, so `backup.UpdateRecords` can mark the state dirty without an import cycle. The epoch decision treats a node without an instance id as unknown, never fenced.
- **The fence check also runs before `restore` and `db-fork` jobs** (a fork creates a stanza and a full; a restore's promote archives). A fenced node still answers `ssh.confirm` and `upgrade.install`.
- **`db recover` builds an empty scaffold first** instead of a "no live cluster" branch in promote: `kernel-tuning-v1`, `postgres-cluster-v1@<db>` and `postgres-config-v1@<db>` with archiving off make `18/<db>` empty, so materialise copies its configuration (the hot-standby floors) exactly as a Phase 3 restore does, and promote swaps the copy in unchanged. The scaffold is dropped right after. The engine keeps a database awaiting recovery for that run through `engine.Options.Recovering`.
- **`db recover --dump` isn't built.** Loading a dump needs the live cluster's roles, which a rebuilt server doesn't have; `no_backup_in_stanza`'s hint is `db drop`, `db create`, then `restore --dump`.
- **Timers come back per recovered database**, not once at the end of `--all`; the fulls of `--all` still run at the end (the CLI runs `backup now --type full` for each, one after another).
- **`metrics-v1` failing during `server init --restore` fails the database phase** as it does in init (`--resume` continues); the `metrics_deferred` downgrade is not built.
- **Unit 4 also fixed `backup.Probe`**: it refused every stanza not named after the server, so a second `backup setup` or a rebuild would have failed `repo_in_use`. It now follows the epoch's server, or the server's own stanzas when there's no epoch.
- **Unit 12:** a rebuild whose bundle lacked the WireGuard key updates the context's node key too (`context_wg_key_updated`).
- **Unit 15:** `schema/vaire-v1.json` also gained `doctor_failed`, `database_protected` and `protect_code_rejected`, which earlier phases missed.
- **Unit 17 (release notes):** draft alpha notes added during release preparation on 2026-10-19; final version/commit/publication evidence remains part of the authorized release cut.
- **Review fixes (safety):** the fence decision fences only on positive proof (a well-formed epoch of this server, another instance, an epoch newer than the node's own); anything else is unknown, and the master backup refuses to upload beside a foreign epoch (`epoch_conflict`). `backup.Probe` takes the node's instance id: a prefix whose epoch names an earlier install of the server is refused (`repo_in_use`) for a fresh `server init` or `backup setup`, which would otherwise fence the new server at its first upload; a resumed init and the configured prefix are accepted. `archive-push` refuses when it can't stat the fence marker, and falls back to `/usr/bin/pgbackrest`. Placement refuses a host that is already a node unless it is this rebuild's own earlier placement, writes modes without execute or setuid bits and owners below uid 1000. `db recover` writes the real `40-vaire-backup.conf` (and stops the empty scaffold) before promote, so the new timeline's history file reaches the stanza; the database stays awaiting recovery until promote's swap; its full backup starts after the heavy-I/O lock is released; the stale-WAL check uses the bucket's own time of `epoch.json`. A resumed `server init --restore` keeps its first epoch write. New `vaire node unfence --confirm <server>/unfence [--reclaim]`. A leftover `[backup.vps]` gives the warning `legacy_config_ignored`.
- **From "v1 alpha readiness", done here because they were wording only:** `vaire upgrade`'s `nodes_not_upgraded` warning names `vaire apply --auto-upgrade-nodes` instead of the unbuilt `vaire node upgrade`, and the README marks `vaire wg export --install` as not implemented yet.

---

## Scope

**In Phase 5**

- **The master bundle:** an age-encrypted tar of what's born on the master and can't be regenerated, built on the node after every change (debounced), kept locally (`local_keep`) and uploaded to `<prefix>/master/` when there's a bucket, with retention, a daily safety net and a sidecar manifest. A new step `master-backup-v1` (the empty step 15) prepares the node for it.
- **The epoch fence:** `<prefix>/master/epoch.json` names the one instance allowed to write to the bucket. An old master that comes back sees it, stops its timers and poolers, refuses `archive-push`, and refuses mutating RPCs.
- **Commands:** `server backup now|list|verify|export`, `server init --restore [--bundle <id|time> | --bundle-file <file>]`, `db recover <db>|--all`, and `context set-address`.
- **Rebuild:** a new VPS takes over the hostname, the certificate, the WireGuard key and the SSH host keys, so the operator's context keeps working; every database comes back `awaiting_recovery` until `db recover` materialises it from its stanza.
- **Phase 4 hooks:** doctor checks, `vaire-master-backup.prom`, a master-backup row in `status`, the metrics credential on rebuild, doctor skipping databases awaiting recovery.
- **Removed:** restic, `vps backup|restore`, `--vps-backup-schedule`, `--restore-time`, `[backup.vps]`.

**Not in Phase 5**

- `server takeover --node <worker>` (Phase 10), a standby master or a replicated store (Phase 11).
- Followers in the bundle and in `db recover` (Phase 6); reinstalling extension packages on rebuild beyond what `postgres-packages-v1` already installs (Phase 7).
- `server init --local --restore`: the bundle is decrypted on the operator's machine only, and a server console has no identity to decrypt with.
- Un-fencing as a routine. A fenced master is destroyed, or rebuilt again with `--restore`; `vaire node unfence` (added in review) exists only for a rebuild that was abandoned or a mistake.
- Pulling bundles to the operator's machine automatically. Without a bucket, `server backup export` is explicit (D13).
- More than one operator racing a rebuild. One operator is assumed (MASTER's decisions table).

---

## Decisions

Each is **Decided (recommended)**: approve it or change it before the unit that needs it starts.

### D1. What the bundle holds

**Decided (recommended): only what's born on the master and can't be regenerated** (the RFC's class A), as it exists today. Phase 10's `nodes.toml` and fleet key join it then; the manifest lists files generically, so they need no new schema.

| In the bundle | Path | Why it can't be regenerated |
| --- | --- | --- |
| Desired state | `/etc/vaire/cluster.toml` | Every database's port, stanza, size, settings, protection |
| Node identity | `/etc/vaire/node.toml` | Server, node name, role, `installed_at` |
| Operator registry | `/etc/vaire/operators.toml` | WireGuard peers and SSH keys of every operator |
| Durable records | `/var/lib/vaire/records.json` | Timeline switches, dropped and replaced stanzas and their expiry, drills, last results. Pruned on restore (D9) |
| TOTP secret | `/etc/vaire/protect.json` | Protected databases stay protected, and the authenticator app keeps working. Failure count and lockout are reset on restore; the last accepted step is kept |
| WireGuard key | `/etc/wireguard/vaire.key` | The operator's peer config names its public key (`vaire.pub` is derived) |
| SSH host keys | `/etc/ssh/ssh_host_{ed25519,ecdsa,rsa}_key{,.pub}` | Pinned in the operator's `known_hosts` |
| Certificate | `/etc/vaire/tls/server.crt`, `server.key` | Clients connect at once with `verify-full`, no issuance burst |
| ACME account | `/etc/vaire/acme/` | Renewals continue on the same account |
| Manifest | `manifest.json` (inside the tar) | Schema, vaire version, server, node, epoch, desired-state revision, created at, and per file: path, owner, group, mode, size, sha256 |

| Not in the bundle | Why |
| --- | --- |
| `/etc/vaire/runtime/*` (bucket keys, service passwords, dump recipient), the pgBackRest cipher, the `/metrics` htpasswd, the sudo password | Supplied by the operator: the top-up re-sends them from `credentials.json` (D11). The bucket never holds the keys to itself |
| Everything steps render (`wg0.conf`, sshd drop-ins, ufw, nginx, poolers, `pgbackrest.conf`, units, sysctl, `userlist.txt`, `targets.json`) | Rendered again from the files above |
| Data, WAL, temporary clusters, restore leftovers, stanzas, dumps | pgBackRest and the dumps cover them |
| `journal.json`, `state.json` (jobs, lockdown flags, deferred restarts, `secrets_confirmed_at`), `doctor.json`, `maint.json`, textfiles, logs, `/var/lib/vaire/jobs` | A fresh host runs every step; the rest is regenerable or ephemeral |
| The instance id and the local epoch file (D5), the local bundles themselves | Each install has its own; a restored copy would defeat the fence |

### D2. Format and encryption: age to the recovery identity, nothing new in the kit

**Decided (recommended):**

- **Format:** an uncompressed POSIX tar (kilobytes; compression adds nothing) with entries in a fixed order, `manifest.json` first, mtimes set to the bundle time, so the same content gives the same bytes. Schema `vaire-master-bundle/v1`.
- **Encryption:** age X25519 to the **recovery kit's recipient**, the one `server init` creates and the dumps use (`ensureRecoveryIdentity`). Encrypted in-process on the node with `filippo.io/age`, streamed straight into the upload; no cleartext on disk, no secret in argv. The identity stays in the kit and `credentials.json`; neither the node nor the bucket can read a bundle. age's payload is authenticated, so any corruption fails decryption.
- **The node needs the recipient without a bucket.** Today it reaches the node only in `backup.env`, which `pgbackrest-v1` writes only with a bucket. The top-up sends `backup.age_recipient` whenever the kit has an identity (not only with a bucket), and `master-backup-v1` writes `/etc/vaire/runtime/recovery.recipient` (root 0600) when missing. The recipient isn't a secret, but it lives beside the others so a stale operator can't swap it (same set-once rule as the service passwords).
- **Names:** `<UTC ts>-e<epoch>-r<first 12 of the revision>`, for example `20261015T101502Z-e2-r3f9a1c0b77d2`. The timestamp sorts; the epoch lets a restore ignore bundles an old master wrote (D5).
- **Sidecar:** `<name>.json` next to `<name>.tar.age`, unencrypted, written **after** the bundle: schema, server, node, epoch, revision, created at, vaire version, file paths and sizes, the bundle's size and the **sha256 of the ciphertext**. No per-file plaintext hashes (they would let a reader confirm guesses about `cluster.toml`); those are only inside the encrypted manifest.
- **Content hash:** sha256 over the canonical file set (paths, modes, contents), kept with the last bundle in `/var/lib/vaire/master-backup/last.json`. It decides whether there's anything to bundle.

### D3. When it builds and uploads: after every change, debounced, plus a daily safety net

**Decided (recommended):**

- **One hook at the writers, not a list of commands.** `masterbackup.MarkDirty(reason)` touches `/var/lib/vaire/master-backup/dirty`. It's called by the code that writes a class-A file: `state.FileStore.Save` (every `cluster.toml` write: `config set`, `config edit`, `db create/drop/fork/resize`, `db protect/unprotect`, restore promote and undo with a stanza switch), `writeBootstrapFiles` (`node.toml`, `operators.toml`), the certificate install in `certs-renew` (only when a new certificate is installed), the ACME account creation, the protect store (init, confirm, every accepted code), and `backup.UpdateRecords` when a **durable** section changed (dropped stanzas, timelines, awaiting recovery). Backup facts (last backups, verifies, repo facts, drills, dumps) don't mark dirty: they change after every backup and ride along in the next bundle or the daily one. A new writer can't be forgotten by a new command.
- **A path unit, so no trigger is lost.** `vaire-master-backup.path` with `PathExists=/var/lib/vaire/master-backup/dirty` starts `vaire-master-backup.service` (`vaire node run master-backup`). The job waits 5 s of quiet (the dirty file's mtime unchanged), removes the flag, snapshots the files and builds. A change during the build re-creates the flag, and systemd re-checks `PathExists` when the service stops, so it runs again. A burst makes one or two bundles; writers never call `systemctl` (works from RPCs and jobs alike).
- **No lock.** Each writer writes atomically (rename), so every file is whole. Files changed together (`cluster.toml` and `records.json` on a dump-restore promote) may be read between the two writes; the second write marks dirty again and the next round, seconds later, has both. The job doesn't wait for the global lock, so a long `apply` or `db create` never delays a bundle.
- **Skip when unchanged:** the content hash equals the last bundle's → nothing to do.
- **Daily safety net:** `vaire-master-backup.timer` (daily, in the maintenance window, 30 min jitter) runs the same job. It builds only when the content changed, and with a bucket it also checks that the newest bundle in the bucket is ours and complete (a `HEAD` of its sidecar), re-uploading the local copy if not. That sets `last_verified_at`, which is what doctor's age check reads (D12), so a master with no changes for months doesn't look stale.
- **Locally:** every bundle is kept in `/var/lib/vaire/master-backup/` (root 0700, files 0600), the newest `[master_backup] local_keep` (default 10), with or without a bucket.

### D4. Retention in the bucket

**Decided (recommended):** keep every bundle from the last `retention_days` (30), then the newest of each ISO week for `retention_weeks` (52), and **always the newest three** whatever their age (a master without changes for a year still has a bundle). Pruning runs at the end of an upload, at most once a day (`last_pruned_at`), deletes the sidecar first and then the bundle, and only touches names it parses. Bundles from an older epoch are pruned by the same rules once the current epoch has three of its own.

### D5. The epoch and the fence

**Decided (recommended):**

- **Instance id:** each install has a random 128-bit id in `/var/lib/vaire/instance` (root 0600, not in the bundle). `master-backup-v1` makes one when missing. A restore places one the operator generated (D7).
- **The bucket's epoch:** `<prefix>/master/epoch.json` = `{schema, server, epoch, instance_id, written_at, written_by, reason}`. The node keeps its copy in `/var/lib/vaire/master-backup/epoch.json`. A node with a bucket and no epoch object writes `epoch = 1` with its own instance id on its first upload, and only if the object is still absent (read, write, read back). `backup setup` triggers that first upload, as the RFC asks.
- **Fenced** means the bucket's `epoch.json` exists and names another instance or a higher epoch. A bucket that can't be read is **unknown**, never fenced: a bucket outage mustn't stop a healthy master, and the job that needs the bucket fails anyway.
- **Who checks:** every job that writes to or deletes from the bucket, before it starts: `backup-full/diff/incr` (which also run pgBackRest's `expire`), `db-dump`, `stanza-expiry`, `master-backup`, the dump-before-drop of `db drop` and `db recover`. Plus `vaire-fence.service` at boot (`After=network-online.target`, `Before=` both poolers and every `postgresql@18-*` unit, a 20 s budget so a bucket outage never blocks a boot) and `vaire-fence.timer` every 5 minutes (one GET of a tiny object, about 9,000 a month).
- **What a fenced node does,** once, recorded in `/etc/vaire/fenced` (`epoch`, `instance_id`, `seen_at`): stops and disables every `vaire-*.timer` except the fence timer; stops and disables both poolers, so clients that still reach the old IP get "connection refused" instead of a zombie that accepts writes; leaves the clusters running on their sockets for break-glass inspection; makes `archive-push` refuse (below); and the RPC dispatcher refuses every mutating method with the new code `node_fenced` (reads, `doctor`, `status`, `logs` and `ssh.status` still answer). `doctor` fails `master.fenced`; `vaire_master_fenced 1` in metrics.
- **`archive-push` is fenced too.** The RFC left a gap here: pgBackRest pushes each WAL segment itself. **Decided:** `40-vaire-backup.conf`'s `archive_command` becomes `/usr/local/bin/vaire node archive-push %p --stanza <stanza>`, a hidden node command that stats `/etc/vaire/fenced` and then `exec`s `pgbackrest --stanza=<stanza> archive-push %p` (same options as today; async archiving and the spool stay pgBackRest's). It reads a local file only, never the bucket, so it costs one `exec` per segment. A fenced node's clusters keep WAL locally (`archive-push-queue-max` still bounds it). An `archive_command` change is a reload, never a restart.
- **The remaining window:** an old master that boots with the bucket unreachable, or between two fence checks, can push WAL for up to 5 minutes. Two more guards: pgBackRest refuses a segment that already exists with a different checksum, and `db recover` refuses to open a database whose stanza received WAL after the fence was written (D8). The rebuild still says "power off or destroy the old VPS first", and checks it (D7).
- **Without a bucket** there's no epoch and nothing to fence; the rule is all there is.

### D6. Taking over the identity: hostname, certificate, WireGuard, host keys

**Decided (recommended):**

- **WireGuard:** the restored `/etc/wireguard/vaire.key` is in place before `wireguard-v1` runs; `wireguard-key.sh` keeps an existing key, so the node's public key is unchanged and the operator's peer config matches.
- **SSH host keys:** placed (with `ssh-keygen -l` checked on each) over the first-contact connection, then `systemctl reload ssh`; the open session survives. Over WireGuard the operator's pinned `known_hosts` matches. The pending `known_hosts` entry for the new public address is re-pinned to the restored keys.
- **Certificate and ACME account:** placed before `tls-v1`, which installs a placeholder only when no certificate exists, so the poolers and nginx start with the real certificate. Clients see the same certificate as soon as DNS points at the new IP. `certs-renew` renews on the restored account.
- **Hostname:** `db1.<domain>` doesn't change. In `acme` mode, if the new public address differs, the restore shows the A/AAAA change and waits for it (the existing DNS step, with the public-resolver check); clients reconnect once the record has moved. **Recommended in the docs:** a provider IP that can move between servers (Hetzner Primary IP, a floating IP) makes the rebuild invisible to DNS too.
- **The operator's context:** it dials WireGuard at the node's `public_address`, an IP (Phase 3 unit 2 deferred dialing the hostname to this phase). `server init --restore` runs on the operator's machine with the context, so it rewrites the master's `public_address` in `context.toml` itself (`context_endpoint_updated`). For other machines that have the same context: **in `acme` mode,** when the WireGuard handshake to the stored address fails, the tunnel resolves the master's hostname through the public resolvers the DNS check uses; if that gives another address it retries there, and on success updates `context.toml` with a `context_endpoint_updated` warning. That's safe because WireGuard authenticates the peer by its pinned key, not its address. **In `self-signed` mode,** or before DNS has moved: `vaire context set-address <ip[:port]> [--name]`.
- **`cluster.toml`'s `nodes[].public_address`** is rewritten to the new address before placement (it isn't an immutable key), and the secrets document's node entry and break-glass text follow (D7, last phase).

### D7. The rebuild: `server init --restore`

**Decided (recommended):** a variant of the remote init flow with its own phases in `pending/<name>/progress.json`, resumable with `--resume`.

- **Needs the context.** The operator's context for the server (its keys, `known_hosts` and the recovery kit) is the input. With only the secrets document, `vaire login` first. The usual init refusal "a context named X already exists" is inverted: restore refuses without one.
- **Remote only.** `--local --restore` is refused (`usage_error`); see Scope.
- **Phases:**
  1. `bundle_fetched`: list `<prefix>/master/` with the kit's keys, pick the newest complete bundle (sidecar present) of the current epoch, or `--bundle <id>` (an id or a time: the newest at or before it), or read `--bundle-file <path|->`. Check the sidecar's ciphertext sha256, decrypt with the identity, validate the manifest and every file's sha256. Refuse a bundle of another server (`bundle_server_mismatch`), one whose `operators.toml` lacks the context's operator (`operator_not_in_bundle`), or one written by a newer vaire (`update_required`). The **ciphertext** is saved in the pending directory (0600) for `--resume`, and decrypted again from it; nothing in clear on disk. The summary shows created at, epoch, revision, the databases, vaire version.
  2. `old_master_checked`: try the context's tunnel to the old address for 5 s. If the old master answers, refuse (`precondition_failed`, reason `old_master_alive`, hint: power it off or destroy it). `--old-master-unreachable` skips the check when the operator knows better (the check can't see a master behind a partition).
  3. `fenced`: with a bucket, needs `--confirm <server>/replace` (or the prompt). Generate the new instance id, read `epoch.json`, write `epoch + 1` with the new id and `reason = "restore"`, read it back. Refuse if it changed in between (`epoch_conflict`). Nothing on the new VPS has been touched yet; a failure here leaves everything as it was. Without a bucket, the same token is still required and the step only prints the "power off the old VPS" rule.
  4. `first_contact`, `binary_installed`: as init today (TOFU for the new VPS's provider host key, OS and RAM preflight, the binary push). The new VPS must be fresh: `bootstrapPreflight` already refuses existing PostgreSQL or a foreign `node.toml`.
  5. `bundle_placed`: the new RPC `cluster.bootstrap phase=restore` over the first-contact connection (unit 13): writes the bundle's files with their owners and modes, the instance id and the local epoch, prunes `records.json` (D9) and marks every database `awaiting_recovery`, swaps the host keys and reloads sshd.
  6. `host_bootstrapped` … `locked_down`: `cluster.bootstrap phase=host` with the bundle's `cluster.toml` (new `public_address`) and the context's operator, then the switch to WireGuard with the context's own keys, and the commit-confirm lockdown, all as init.
  7. `dns_confirmed`: `acme` mode and a changed address only (D6).
  8. `database_bootstrapped`: `cluster.bootstrap phase=database` with the full top-up (`vaire_admin`, service passwords, the metrics hash, the recovery kit). Databases awaiting recovery are left out of every step (D9). `metrics-v1` failing here (it downloads the exporters from GitHub) is the warning `metrics_deferred`, not a failure; the next `apply` installs them. Doctor gate as init, with awaiting databases skipped.
  9. `first_bundle`: `master-backup.now` on the new master, so the bucket's newest bundle carries the new epoch and the new address.
  10. `context_updated`: rewrite the context's `public_address` and pins. If the public address changed, show a new secrets handoff (same secrets, the new address and break-glass text) with the `stored` gate, as init; otherwise skip it. Either way call `cluster.confirm-secrets`, so doctor's `secrets.confirmed` passes (Phase 4 asked for this).
- **Result:** the timings of each phase (for the RTO), the databases awaiting recovery, and next steps: `vaire db recover --all` (and the DNS change, when pending).

### D8. `db recover`: each database from its stanza

**Decided (recommended): `db recover` is a restore into a database that has no live cluster**, on the Phase 3 path that passed its exit test: materialise `<db>__restoring` from the database's stanza, then the promote cut-over with a branch for "no live cluster" (no `PAUSE`, no aside, a rename instead of a swap). `18/<db>` starts on its own port with its own configuration and limits, archiving on, reaches the target and promotes on a new timeline that archives to the same stanza.

- **Target:** `--latest` (the default): the copy follows the archive as a standby, as `restore --latest` does, and is promoted once it has replayed everything in the archive. `--time <ts>`: paused at the target, then promoted; it leaves out what came after, so it needs `--confirm <db>/recover`. The timeline is `records.TargetTimeline(db, stanza)`, so a database that went through `restore undo` comes back on its recorded branch.
- **Inspect first, if wanted:** `--no-promote` stops at ready (`<db>__restoring` in the session pooler, as `vaire restore` does); `restore status` and `restore promote <db>` finish it.
- **Serial, under the heavy-I/O lock.** One database at a time; each restore uses pgBackRest's `--process-max=4` (`networkProcessMax`), which already parallelises within one stanza, so the network is the limit either way and a serial order gives a predictable RTO. The recover job **waits** for the heavy-I/O lock (up to 6 h) instead of failing with `lock_held` at once, since a recovered database's first timer could hold it.
- **`--all` order:** protected databases first (the ones that matter most), then by size ascending from `records.json`'s repo facts, so most databases are back early; `--db a,b` puts named databases first. `timers-v1` and `metrics-v1` run once at the end, not after each database.
- **Before opening (the split-brain check):** read the newest object under the stanza's archive path. If any segment is newer than the epoch's `written_at`, the old master is still writing: refuse (`precondition_failed`, reason `stanza_still_written`, the segment and its time in details). Checked once before materialising and again just before promote.
- **No backup in the stanza** (a database created moments before the loss, or a dump-restored database whose new stanza's first full never finished): `precondition_failed`, reason `no_backup_in_stanza`, with the alternatives in the hint: `--dump <id>` (the existing `loadFromDump` path, which gives the database a new stanza on promote), the replaced stanza named in the records, or `db drop` and create it again.
- **After recovery:** the database's steps (cluster, config, roles), the poolers, then once at the end the timers, metrics and a full backup of each recovered database, one after another (a full is a new baseline on the new timeline). `--no-full` skips them; doctor's `backup.full_age` reminds.
- **Roles and passwords** come back with the data (SCRAM verifiers in `pg_authid`), so applications keep their credentials. `userlist.txt` is re-read from the recovered clusters.
- **Retry:** a failed recover leaves the database `awaiting_recovery`; running it again drops the failed `<db>__restoring` first (`clearFailedRestore`).
- **Without a bucket** there's no stanza to recover from: `precondition_failed`, reason `no_offsite_backup`, hint: `vaire db drop <db> --confirm <db>` and `vaire db create <db>` (the data is lost; that's what a bucket is for).

### D9. Records, stanzas and leftovers on a rebuild

**Decided (recommended):** placement prunes `records.json` and reports what it dropped (`restore_leftovers_lost` warning with the names):

| Record | On restore | Why |
| --- | --- | --- |
| `dropped_stanzas` (dropped and `replaced`) | Kept | The stanzas are in the bucket; `stanza-expiry` keeps expiring them on the new master, and `backup list` keeps listing them |
| `timelines` | Kept | Later restores and `db recover` need the recorded branch |
| `restore_leftovers` (`<db>__pre_restore_*`) | Dropped | Their data was on the lost disk |
| `restores` (open `<db>__restoring`), `drilling`, `undoing` | Dropped | Same; an undo under way at the loss leaves the database on `cluster.toml`'s stanza, which is what `db recover` uses |
| `last_backups`, `last_verifies`, `repo`, `last_dump`, `drills`, failures | Kept | History for `backup list` and doctor; the next backup refreshes them |
| `awaiting_recovery` (new) | Set for every database in `cluster.toml` | Cleared by `db recover` or `db drop` |

- **Awaiting recovery** is operational state, not desired state, so it lives in `records.json`, never in `cluster.toml`. The node gives the engine a **steps view** of the configuration without those databases (`node.stepsView(cfg)`), used by `plan`, `apply` and the bootstrap phases only; what's saved is always the whole file. The memory budget and `db create`'s port and name checks still count them.
- **What refuses a database awaiting recovery** (`precondition_failed`, reason `awaiting_recovery`, hint `vaire db recover <db>`): `restore` and its subcommands, `db fork`, `dump`, `backup now|verify`, `restore drill`, `db resize`, `db credentials`, `maint`, `apply --db`. `db list` and `status` show the state; `db drop` is allowed (the stanza is recorded as dropped, the protection rule still applies); `config set clusters.<db>.*` is allowed.

### D10. Protected databases

**Decided (recommended):** `protect.json` is in the bundle (D1), so a rebuilt master keeps the TOTP secret and the authenticator app keeps working; the failure count and lockout are reset, the last accepted step is kept (a code used before the loss can't be replayed). The `protected` flags are in `cluster.toml`.

- `db recover <db>` at latest is allowed on a protected database: it creates data, it doesn't replace any.
- `db recover <db> --time` on a protected database is refused (`database_protected`): leaving out the newest data is exactly what protection guards against. A human lifts it first with `vaire db unprotect <db>` (a TOTP code), which works on a database awaiting recovery since it's a `config.set`.
- `db drop` of a protected database awaiting recovery is refused as today.

### D11. Operator-supplied secrets on a rebuild, including the Phase 4 metrics credential

**Decided (recommended):** nothing operator-supplied is in the bundle. The database phase of the restore sends everything from `credentials.json` through the existing top-up (`ensureClusterSecrets`, `addMetricsSecret`, the recovery kit):

- The `/metrics` credential: the operator sends the bcrypt hash of the **same** password, so existing scrapers keep working without an edit. The htpasswd is written because it's missing.
- `vaire_admin`, `pgbouncer_auth`, `vaire_monitor`: the verifiers come back with each database's data; the runtime `.pass` files are written from the request.
- The bucket keys, cipher pass and the recipient: `backup.env`, `pgbackrest.conf` and `recovery.recipient` are written because they're missing.
- The admin user's sudo password: from the context, as init.
- A context missing any of them (logged in before the bucket existed): `input_required`, hint: `vaire login` from a current secrets document.

### D12. Failure modes

**Decided (recommended):**

| Failure | What happens |
| --- | --- |
| Upload interrupted | A bundle is one `PutObject` (kilobytes, never multipart), and the sidecar goes up **after** it. A bundle without a sidecar is incomplete: `list` marks it, restore never picks it, pruning removes it after a day. The dirty flag stays set, so the path unit retries; the daily timer re-uploads the local copy if the newest in the bucket isn't ours |
| Upload keeps failing | The local copy is kept, `last_failure` is recorded, `vaire_master_backup_last_failure_timestamp_seconds` set; doctor's `master.backup` warns after 1 h of unuploaded changes and fails after 24 h |
| Corrupt bundle (sidecar sha256, age, or a file hash) | Restore stops with `bundle_corrupt` before touching the new VPS and names the next older complete bundle in the hint (`--bundle <id>`). Never a silent fallback. `server backup verify` reports the same |
| No bundle in the bucket | `bundle_not_found` with the hint `--bundle-file` from an export |
| Bucket unreachable during the restore's fetch or fence | `endpoint_unreachable` before anything on the new VPS changes; `--bundle-file <export>` restores without the bucket (and without the fence, with the "power it off" rule) |
| Bucket unreachable during `db recover` | The job fails (pgBackRest's error), the database stays `awaiting_recovery`, and a retry is safe |
| Connection lost mid-restore | `server init --restore --resume` continues from the last phase; placement and every step are idempotent |
| Epoch changed while restoring | `epoch_conflict`: another restore is running; stop |
| Fence check can't read the bucket | Unknown, not fenced; the next check retries |

### D13. Without a bucket

**Decided (recommended):** as the RFC decided, plus a doctor nudge.

- Bundles are built and kept locally after every change, encrypted the same way (the recipient file, D2).
- `vaire server backup export [--bundle <id>] [--out <file>]` copies one, still encrypted, to the operator's machine (0600, refuses an existing file) and records `last_exported_at` on the node.
- doctor's `master.offsite` warns when there's no bucket and the newest local bundle is newer than the last export, with the fix `vaire server backup export`. `no_offsite_backup` stays as it is.
- `server init --restore --bundle-file <file>` rebuilds; there's no fence and the command says so.
- `backup setup` later uploads the newest bundle at once and writes epoch 1.

### D14. Commands, tokens and codes

**Decided (recommended):**

- `vaire server backup now [--dry-run]`: mutating, not destructive.
- `vaire server backup list [--local|--bucket]`: from the bucket with the kit (works while the master is down) and from the node when reachable; id, created at, epoch, revision, size, vaire version, complete, uploaded.
- `vaire server backup verify [--bundle <id>]`: on the operator's machine: download, check, decrypt, validate; when the node is reachable, compare with its live content hash (`current` or `behind`).
- `vaire server backup export [--bundle <id>] [--out <file>]`.
- `vaire server init --restore --host <user@ip> [--bundle <id|time> | --bundle-file <file|->] [--old-master-unreachable] --confirm <server>/replace`: destructive (it takes over the server's identity and fences the old one).
- `vaire db recover <db>… | --all [--latest | --time <ts> --confirm <db>/recover] [--no-promote] [--dump <id>] [--no-full]`: long-running jobs (Ctrl-C detaches, `job follow`).
- `vaire context set-address <ip[:port]> [--name <context>]`.
- Removed: `vps backup now|list`, `vps restore`, `server init --vps-backup-schedule`, `server init --restore-time` (a point in time is `db recover --time`; a bundle at a time is `--bundle <time>`). `--restore` becomes a bool.
- New error codes: `bundle_corrupt` (exit 13, with the secrets errors: it's an integrity failure), `bundle_not_found` (exit 11), `node_fenced` (exit 11). New `precondition_failed` reasons: `old_master_alive`, `epoch_conflict`, `bundle_server_mismatch`, `operator_not_in_bundle`, `awaiting_recovery`, `stanza_still_written`, `no_backup_in_stanza`, `no_offsite_backup` (for `db recover`). New warnings: `master_backup_failed`, `context_endpoint_updated`, `restore_leftovers_lost`, `metrics_deferred`.

### D15. `[backup.vps]`

**Decided (recommended):** the type and its validation go. `ParseCluster` drops a `[backup.vps]` table without error (the one exception to strict decoding, with a comment to remove it after the alpha), so the Phase 4 test server and bundles taken before this phase still load; the next save writes the file without it. The alternative is a rebuild of the test server before the manual test.

---

## A. The bundle

### 1. `internal/masterbackup`: file set, manifest, tar and age
- `Files()`: the class-A paths from D1 (globs resolved, missing optional files such as the ACME account skipped and listed as absent); `Snapshot()` reads each file with owner, group, mode and content; `ContentHash`.
- `Build(snapshot, meta) ([]byte tar)` in fixed order with `manifest.json` first; `Encrypt(w, recipient)`; `Open(r, identity) (Bundle, error)` decrypts, untars (refusing paths outside the set, links, duplicates, extra files), and checks every sha256 against the manifest.
- `Name(meta)`, `ParseName`, the sidecar type, and `Keep(names, now, days, weeks, newest)` for D4.
- Files: `internal/masterbackup/{files,bundle,manifest,retention}.go`.
- Tests: deterministic bytes for the same snapshot; a round trip through age; a flipped byte fails `Open`; a tar with `../` or a symlink is refused; `Keep` for 30 days + 52 weeks + newest three, a year-old single bundle kept, unparseable names left alone.
- Done when: the package builds for linux and darwin (the operator opens bundles too) and the tests pass.

### 2. `master-backup-v1`: the recipient, the instance id and the units
- New step 15 `master-backup-v1` in the catalogue and `DatabaseStepIDs()` (after `pgbackrest-v1`): creates `/var/lib/vaire/master-backup` (root 0700), writes `/etc/vaire/runtime/recovery.recipient` from `backup.age_recipient` when missing (input_required when both are missing), makes `/var/lib/vaire/instance` when missing.
- `addRecoveryKit` splits: the recipient travels whenever the kit has an identity; the bucket keys and cipher only with a bucket. `server init` and `apply` send it.
- `timers-v1` renders `vaire-master-backup.service`, `.path` (`PathExists=`) and `.timer` (daily in the window), and with a bucket `vaire-fence.service` (boot ordering from D5) and `vaire-fence.timer` (every 5 min); `ownedService` and the stale sweep learn them. `jobNames` gains `master-backup` and `fence-check` (both quiet jobs, like `metrics-refresh`).
- Files: `internal/steps/{masterbackup,timers,catalog,secrets}.go`, `internal/render/templates/vaire-master-backup.{service,path,timer}.tmpl`, `vaire-fence.{service,timer}.tmpl`, `internal/cli/backup.go`, `internal/node/jobs.go`.
- Tests: unit renders (path unit, boot ordering, jitter); `--check-steps` sees the ID; the recipient travels without a bucket.
- To verify on the VPS: `systemctl list-units 'vaire-master-backup*' 'vaire-fence*'`; the path unit triggering on `touch` of the dirty file; `vaire-fence.service` ordered before `pgbouncer.service` at boot (`systemd-analyze critical-chain`).
- Done when: `apply` on the Phase 4 server writes the recipient and instance id and enables the units, and a second `apply` skips the step.

### 3. MarkDirty and the local job
- `masterbackup.MarkDirty(reason)` and its callers from D3; `backup.UpdateRecords` compares the durable sections before and after.
- `node run master-backup`: debounce, snapshot, skip on an unchanged hash, encrypt to the recipient, write `<name>.tar.age` and `<name>.json` locally, keep `local_keep`, update `last.json` (name, content hash, created at, uploaded, last failure).
- Files: `internal/masterbackup/{dirty,job}.go`, `internal/state/state.go`, `internal/backup/state.go`, `internal/node/{bootstrap,certs,protecttotp,jobs}.go`.
- Tests: durable vs fact changes of `records.json`; `local_keep` pruning; the debounce decision from mtimes.
- To verify on the VPS: `vaire config set clusters.app.settings.work_mem 8MB` makes one local bundle within ~10 s; three quick `config set`s make one or two; `vaire backup now --db app` makes none.
- Done when: every class-A writer marks dirty and the job keeps the local set.

### 4. Upload, sidecar, retention and the first epoch
- With `[backup]`: after the local write, check the fence (unit 5), `PutObject` the bundle, then the sidecar; write `epoch.json` (epoch 1, own instance) if absent; prune once a day (D4). The daily run also `HEAD`s the newest sidecar and re-uploads when it isn't ours (D3).
- `backup setup` starts the job at the end (first upload and epoch 1). `backup.Target` gains the `master/` keys and a `put`/`head`/`delete` helper.
- `backup.Probe` stops refusing the server's own stanzas: a prefix is ours when `master/epoch.json` names this server, or, with no epoch object, when every stanza under it is in `cluster.toml` or the dropped stanzas. (Today it refuses any stanza that isn't the server's name, which breaks a second `backup setup` and the restore.)
- Files: `internal/masterbackup/{upload,epoch}.go`, `internal/backup/s3.go`, `internal/cli/backup.go`.
- Tests: the sidecar written only after the bundle (a fake that fails the second put leaves no sidecar); the ownership rule of the probe.
- To verify on the VPS: R2 accepting the puts and deletes under `master/`; the sidecar's sha256 matching the object.
- Done when: a change on the master shows up in `<prefix>/master/` within seconds, with its sidecar, and `epoch.json` exists.

### 5. The fence
- `masterbackup.CheckFence(ctx, target) (State: ok|fenced|unknown)` from D5; called at the start of `backup-*`, `db-dump`, `stanza-expiry`, `master-backup`, `db drop`'s final dump and `db recover`.
- `node run fence-check` (boot and every 5 min) and `Fence()`: write `/etc/vaire/fenced`, stop and disable the timers and poolers, refuse in the dispatcher (`node_fenced` for every method with a lock other than shared, or with `runLog`).
- `vaire node archive-push <path> --stanza <s>` (hidden, node-only, no RPC, no lock): refuse when fenced (exit 1, a line on stderr for the PostgreSQL log), otherwise `exec` pgBackRest. `postgres-config-v1`'s `40-vaire-backup.conf` uses it.
- Files: `internal/masterbackup/fence.go`, `internal/node/{dispatch,jobs,archivepush}.go`, `internal/render` (`40-vaire-backup.conf`), `internal/cli/tree.go`.
- Tests: the fence decision (no object, same instance, other instance, higher epoch, read error); the dispatcher refusal table; `archive_command` render with the stanza quoted.
- To verify on the VPS: archiving still works through the wrapper (`pg_stat_archiver` and `pgbackrest check` per database); a hand-edited `epoch.json` naming another instance fences the node within 5 minutes (timers gone, poolers stopped, `archived_count` frozen, `failed_count` rising, WAL kept locally); restoring the file and `rm /etc/vaire/fenced` + `apply` heals it (test only, not a feature).
- Done when: a fenced node writes nothing to the bucket and refuses `apply`.

---

## B. Commands and observability

### 6. `server backup now|list|export`
- RPC `master-backup.now` (no global lock; runs the job inline, returns the bundle and whether it uploaded), `master-backup.list` (local bundles, `last.json`, epoch, fenced), `master-backup.get {id}` (the encrypted bytes, base64; records `last_exported_at`).
- CLI: `vaire server backup now`, `list` (bucket via the kit and node, merged by id; `--local`/`--bucket`), `export [--bundle] [--out]`. The `server` group gets a `backup` child.
- Files: `internal/node/masterbackup.go`, `internal/cli/{serverbackup,tree}.go`.
- Tests: merging local and bucket listings; export refuses an existing file and writes 0600.
- Done when: `vaire server backup export` writes a file that `server backup verify --bundle-file` (unit 7) opens.

### 7. `server backup verify`
- Operator-side: fetch (bucket, or `--bundle-file`), sidecar sha256, decrypt with the kit's identity, `Open`; then, when the node answers, `master-backup.list`'s content hash vs the bundle's: `current` or `behind (N changes not yet bundled)`.
- Errors: `bundle_corrupt`, `bundle_not_found`, `secrets_error` (no identity in the kit).
- Files: `internal/cli/serverbackup.go`.
- Tests: the outcomes on fixtures (good, flipped byte, missing sidecar, wrong server).
- Done when: verify of the newest bundle reports `current` right after a change has settled.

### 8. Doctor, metrics and status
- Checks (category backups): `master.backup` (warn 1 h of unbundled or unuploaded changes, fail 24 h; `skip` during init), `master.offsite` (D13), `master.fenced` (fail), `master.verified` (warn when the daily check hasn't confirmed the bucket's newest bundle in 48 h). `ClusterFacts.AwaitingRecovery` is filled from `records.json` (the field exists since Phase 4).
- `vaire-master-backup.prom`: `vaire_master_backup_last_success_timestamp_seconds`, `vaire_master_backup_last_failure_timestamp_seconds`, `vaire_master_backup_last_verified_timestamp_seconds`, `vaire_master_backup_size_bytes`, `vaire_master_backup_dirty`, `vaire_master_backup_local_bundles`, `vaire_master_epoch`, `vaire_master_fenced`, `vaire_database_awaiting_recovery{db}`.
- `status`: a "Master backup" row on the server card (last bundle, uploaded or local only, epoch) and `awaiting_recovery` as a database state.
- Files: `internal/doctor/{checks,master}.go`, `internal/node/{doctor,status,metrics}.go`, `internal/promfile`.
- Tests: the thresholds; awaiting databases skipped by every per-database check.
- Done when: `vaire doctor` on the Phase 4 server shows the four checks passing.

---

## C. Rebuild and recovery

### 9. Awaiting recovery
- `records.json` `awaiting_recovery: {db: {since, stanza}}`; `node.stepsView(cfg)` for `plan`, `apply` and the bootstrap phases; `db list`, `status` show `awaiting_recovery`; the refusals of D9 through one helper beside `checkRestorable`.
- Files: `internal/backup/state.go`, `internal/node/{apply,clusters,db,restore,backup,maint}.go`.
- Tests: the steps view drops exactly the awaiting databases from templates, pooler and timer renders; the refusal list.
- Done when: a hand-marked database (test only) is skipped by `apply` and refused by `restore`.

### 10. Records pruning and the restore placement on the node
- `cluster.bootstrap phase=restore` (token `<server>/replace`; params: `public_address`, `instance_id`, `epoch`; the request secret `master.bundle`, the tar base64, registered for redaction and never in the run log). Validates the tar (unit 1), refuses a host that isn't fresh, writes each file with owner and mode, rewrites `cluster.toml`'s master `public_address`, prunes `records.json` (D9), resets `protect.json`'s failure count and lockout, writes the instance id and the local epoch, checks each host key with `ssh-keygen -l`, reloads sshd. Returns the pruned items and the new host keys.
- Files: `internal/node/{bootstrap,restoreplace}.go`, `internal/masterbackup/prune.go`.
- Tests: the pruning table of D9; `protect.json` reset; the `public_address` rewrite keeps the file valid.
- To verify on the VPS: the first-contact SSH session surviving the host-key swap and `reload ssh`; a new connection presenting the restored key.
- Done when: placement on a fresh VPS leaves `cluster.toml`, `records.json` and the keys as the bundle had them.

### 11. `server init --restore`
- The flow of D7 in `internal/cli/serverinit_restore.go`, reusing `firstContact`, `bootstrapHost`, the tunnel and lockdown, `dnsStep` and `callDatabasePhase`; its own phase names in the pending progress; `--resume`.
- Fetch and fence in `internal/masterbackup` (operator side): list, pick, download, verify, open; epoch read-write-read.
- The old-master probe (5 s tunnel attempt with the context).
- Result: phase timings, awaiting databases, next steps.
- Files: `internal/cli/{serverinit,serverinit_restore,tree}.go`, `internal/masterbackup/fetch.go`, `internal/contexts/pending.go`.
- Error codes from D14.
- Tests: bundle choice (newest complete of the current epoch, `--bundle` by id and by time, incomplete skipped, older epoch ignored); the refusals (`bundle_server_mismatch`, `operator_not_in_bundle`, `update_required`).
- To verify on the VPS: the whole of it, in the checklist.
- Done when: a restore onto a fresh VPS ends with the operator's context working, without edits on this machine.

### 12. The context's endpoint
- `server init --restore` rewrites the master's `public_address` and pins (`contexts.UpdateNode`).
- The tunnel: on a handshake timeout in `acme` mode, resolve the master hostname through the public resolvers, retry at a different address, update `context.toml` and warn `context_endpoint_updated`. The tunnel daemon does the same.
- `vaire context set-address <ip[:port]> [--name]`.
- Files: `internal/cli/{session,tunneld,login,tree}.go`, `internal/contexts/contexts.go`.
- Tests: the retry decision (same address, new address, resolver failure); `set-address` parsing (IPv4, IPv6 in brackets, a port).
- To verify on the VPS: a second machine logged in before the rebuild reaches the new master after DNS has moved, with one warning.
- Done when: a context with a stale address heals itself in `acme` mode and by `set-address` in `self-signed` mode.

### 13. `db recover`
- RPC `db.recover {db, time, no_promote, dump, no_full}` starts the job `recover` (rpc-only): fence check, the split-brain check, wait for the heavy-I/O lock, `kernel-tuning-v1`, materialise `<db>__restoring` (latest as a standby, or `--time` paused), the split-brain check again, the promote path with the "no live cluster" branch, the database's step instances and the poolers, clear `awaiting_recovery`.
- `--all` in the CLI: the order of D8, one job after another (the `eachDatabase` pattern), then `timers-v1`, `metrics-v1` and the fulls (unless `--no-full`), each recorded in `job list`. Summary: per database, recovered to (time and timeline), duration; the total.
- Refusals: not awaiting recovery (use `restore`), `--time` on a protected database, `no_backup_in_stanza`, `stanza_still_written`, no bucket (`no_offsite_backup`).
- Files: `internal/node/{recover,restore,jobs}.go`, `internal/backup/materialise.go` (the no-live branch), `internal/engine/lock.go` (`AcquireHeavyIOWait`), `internal/cli/{recover,tree}.go`.
- Tests: the order; the token rule; the newest-segment comparison against the epoch time.
- To verify on the VPS: promote with no live cluster (rename, `Adopt` onto the database's port); the new timeline's history file and segments archived to the same stanza; `app_app` connecting with its old password; a database that went through `restore undo` recovering on its recorded timeline; `--no-promote` then `restore promote`.
- Done when: `db recover --all` brings back every database at its latest point and the poolers route to them.

### 14. Remove restic and the VPS backup
- Delete `vps backup now|list`, `vps restore`, `--vps-backup-schedule`, `--restore-time`, `laterPhaseFlags`, the `vps-backup` job and `laterJobs`; `BackupVPS` and its validation; `ParseCluster` drops `[backup.vps]` (D15); `Restic` leaves `BackupCreds` and the secrets document (`restic` no longer written; ignored on read).
- Files: `internal/cli/{tree,serverinit}.go`, `internal/node/jobs.go`, `internal/config/cluster.go`, `internal/contexts/credentials.go`, `internal/secretsdoc/document.go`.
- Done when: `grep -ri restic internal` is empty apart from the read-tolerance comment, and `vaire commands --json` has no `vps`.

---

## D. Docs and the exit test

### 15. Agent guide and schema
- `vaire guide agents`: the DR workflow (power off, `server init --restore`, `db recover --all`), the tokens `<server>/replace` and `<db>/recover`, `node_fenced`, `bundle_corrupt`, never printing a decrypted bundle; `schema/vaire-v1.json` for the new results.
- Files: `internal/guide/agents.md`, `schema/vaire-v1.json`.
- Done when: `commands --check-meta` passes with every new command's metadata.

### 16. Docs
- MASTER: Phase 5's row and modules, the decisions table (master backup, the fence, `db recover`), technology (age to the recovery identity, no restic), files on a node (the master-backup directory, `instance`, `fenced`, `recovery.recipient`), the single-point-of-failure risk with the measured RTO, the split-brain risk rewritten for the `archive-push` fence.
- IMPLEMENTATION: the master backup section becomes the spec; step 15; the command table; error codes; the node files table; timers; the secrets table (the recipient without a bucket).
- README: "Recovering a lost server" rewritten; the floating-IP recommendation.
- MANUAL-TESTING: the checklist below.
- Done when: no doc says restic or "until Phase 5".

### 17. Release notes for the phase
- Start the changelog the alpha needs (see the last section) with a Phase 5 entry listing what changes for an operator: the new step, the `archive_command` change (a reload of every cluster on the next `apply`), the removed commands and flags.
- Done when: the entry exists.

### 18. Manual test (the exit test)
- Run the checklist below. Record the date, vaire version and VPSes next to Phase 5 in MASTER, and the measured RTO in MASTER's risks.

---

## Manual test checklist (to add to MANUAL-TESTING.md)

## Phase 5: Master backup and disaster recovery

Run on real VPSes against Cloudflare R2, after the Phase 4 test, in order. Keep a stopwatch: the RTO table at the end is part of the result. Useful on the node: `ls -l /var/lib/vaire/master-backup/`, `cat /var/lib/vaire/master-backup/last.json`, `systemctl list-units 'vaire-master-backup*' 'vaire-fence*'`, `cat /var/lib/vaire/records.json`; from the laptop: `vaire server backup list`.

**Part A: with a bucket**

- [ ] **Rebuild `mbvlabs` from scratch**
    - Destroy the old server, `vaire server init` on a fresh VPS with R2. Expected: `master-backup-v1` applies, `<prefix>/master/` has a bundle and its sidecar, and `epoch.json` names epoch 1 and the node's instance id (`cat /var/lib/vaire/instance`).
    - `vaire server backup list` shows it as complete and uploaded; `vaire server backup verify` says `current`.
- [ ] **Make changes**
    - `vaire db create app --size small`, `vaire db create other --size small`, a write loop on `app`, `vaire protect init` and `vaire db protect app`.
    - `vaire restore app --time <t> --confirm app/restore`, promote, then `restore undo` (records a timeline switch), and `restore cleanup`.
    - `vaire db create scratch` then `vaire db drop scratch --confirm scratch` (a dropped stanza).
    - `vaire config set clusters.other.settings.work_mem 8MB` and `apply`.
    - Expected: a new bundle within ~10 s of each change (`server backup list`); three `config set`s in a row make one or two bundles, not three; `vaire backup now --db app` makes none; `vaire server backup verify` says `current`.
    - Also check: `vaire-master-backup.prom` lines; `vaire doctor` shows `master.backup`, `master.verified` passing; `vaire status` has the Master backup row.
- [ ] **The archive wrapper**
    - `SHOW archive_command` in `app` names `vaire node archive-push`; `pg_stat_archiver.archived_count` rises under the write loop; `pgbackrest --stanza=<app's stanza> check` passes.
- [ ] **Destroy the VPS**
    - Take a provider snapshot of the VPS first (it plays the old master that comes back, below). Note the time of the last write in `app` (`select max(created_at)`), then destroy the VPS in the provider's console. Start the stopwatch.
- [ ] **`server init --restore` on a new VPS**
    - Create a new VPS (a new IP, unless you test the floating-IP path). `vaire server init --restore --host root@<new-ip>`. Expected: it lists the bundle (created at, epoch 1, the databases), asks for `mbvlabs/replace`, writes epoch 2, does first contact and the lockdown, asks to move `db1.<domain>` to the new IP and waits for it, and ends with a new handoff (the address changed), the phase timings, and `app`, `other` awaiting recovery.
    - Also check: the old-master probe said it couldn't reach the old master; `epoch.json` names epoch 2 and the new instance; `<prefix>/master/` has a new bundle with `-e2-`.
    - The context works without edits: `vaire status`, `vaire ssh`, `vaire doctor` from the same laptop. `ssh-keygen -lf` of the new server's host key equals the old pin; `wg show` on the node has the same public key as before.
    - On a second machine that logged in before the loss: `vaire status` warns `context_endpoint_updated` once and works.
    - Records intact: `records.json` has the timeline switch for `app`, the dropped stanza of `scratch` with its expiry, the drill and backup history; no restore leftovers (reported as `restore_leftovers_lost` if there were any).
    - TLS: `openssl s_client -connect db1.<domain>:6432 -starttls postgres` shows the old certificate's serial; no ACME order was placed (`journalctl -u vaire-certs`).
    - Metrics: the scraper's existing credential still gets 200 on `/metrics/node`.
    - `vaire doctor`: databases awaiting recovery are `skip`, everything else passes; `secrets.confirmed` passes.
- [ ] **The databases come back: `db recover --all`**
    - First `vaire db recover app --time <t> --confirm app/recover`: refused with `database_protected`.
    - Then `vaire db recover --all`. Expected: `app` first (protected), then `other`; each is restored, promoted on a new timeline and opened; the summary has the recovered-to time per database and the durations; then one full each.
    - `app`'s newest rows match the last write noted before the destroy (within `archive_timeout`, 60 s); `app_app` connects with its old password through 6432 and 443; `other` keeps `work_mem = 8MB`.
    - Also check: `db list` shows both `running`; timers back (`systemctl list-timers 'vaire-*'`); `targets.json` lists both; `vaire protect status` shows `app` protected and a current code is accepted; `select timeline_id from pg_control_checkpoint()` in `app` is one above its timeline before the loss.
    - Stop the stopwatch at the second database open. Write the RTO table below.
- [ ] **The old master comes back and is fenced**
    - Create a VPS from the snapshot taken before the destroy and boot it. Expected: at boot `vaire-fence.service` writes `/etc/vaire/fenced` before the poolers start; its timers and poolers are stopped and disabled; `archive-push` refuses (`pg_stat_archiver.failed_count` rises and nothing new from it appears in the stanzas); over break-glass, `sudo vaire node rpc` of a mutating method gets `node_fenced`; `vaire doctor` on it (break-glass) fails `master.fenced`. Destroy it afterwards.
    - The split-brain check: mark a recovered test database awaiting recovery by hand (test only), let the snapshot VPS push one segment to its stanza with the fence file removed, then `vaire db recover <db>`: refused with `stanza_still_written`. Skip this item if it can't be staged cleanly; unit 13's tests cover the comparison.
- [ ] **Failure modes**
    - A corrupt bundle: upload a copy of the newest bundle with one byte flipped under a newer name and its sidecar; `server init --restore --dry-run` (or `server backup verify`) refuses with `bundle_corrupt` and names the previous bundle. Remove it.
    - An incomplete upload: delete the newest sidecar; `server backup list` marks the bundle incomplete; the daily run (or `server backup now` after a change) uploads again.
    - Bucket unreachable: point the context's endpoint at a wrong host (edit `credentials.json` on a copy of the context) and run `server init --restore --dry-run`: `endpoint_unreachable` before anything happens.

**Part B: without a bucket**

- [ ] **A server without a bucket**
    - `vaire server init` on a fresh VPS, skipping backups; `vaire db create app`; a few changes. Expected: bundles in `/var/lib/vaire/master-backup/` only (`local_keep` 10), `vaire doctor` warns `master.offsite` with the export fix.
    - `vaire server backup export --out mbvlabs-master.tar.age`: a 0600 file; `master.offsite` passes until the next change.
    - `vaire server backup verify --bundle-file mbvlabs-master.tar.age` says it's valid.
- [ ] **Rebuild from the export**
    - Destroy the VPS. `vaire server init --restore --host root@<new-ip> --bundle-file mbvlabs-master.tar.age`. Expected: no fence (it says so and repeats the power-off rule), then the same rebuild as Part A; `app` comes back `awaiting_recovery`.
    - `vaire db recover app` refuses with `no_offsite_backup` and the drop-and-create hint. `vaire db drop app --confirm app` and `vaire db create app` bring an empty `app` back (the data is lost: that's what a bucket is for). Expected otherwise as Part A: same context, same host keys and WireGuard key, records intact.

**RTO**

| Measure | Time |
| --- | --- |
| Destroy → `server init --restore` started (new VPS ready) | |
| `server init --restore` (first contact → handoff), of which DNS wait | |
| `db recover` per database (size, time) | |
| Destroy → last database open | |
| Fulls after recovery | |

---

## What later phases need from Phase 5

- **Phase 6 (followers):** a follower's entry in `cluster.toml` comes back `awaiting_recovery` like any database; `db recover` of a follower re-follows its source after the source is recovered (order: sources first).
- **Phase 7 (extensions):** `db recover` runs the database's extension steps; a preload extension's package must be installed before the cluster starts.
- **Phase 8 (upgrades):** `node upgrade` starts with `master-backup.now` (IMPLEMENTATION's upgrade section already says so).
- **Phase 10 (workers):** `nodes.toml` and the fleet key join the bundle; `server takeover` reuses the fetch, the epoch bump and the placement.

---

## Risks

- **The fence has a window.** An old master booted with the bucket unreachable, or between two checks, can push WAL for up to 5 minutes before it fences itself. pgBackRest's checksum refusal and `db recover`'s newest-segment check catch most of it; the "power off the old VPS" rule and the 5 s probe catch the rest. Nothing fences a master the operator never destroys and whose bucket access is cut, but that master can't write to the bucket either.
- **The archive wrapper is on every cluster's WAL path.** A bug in `vaire node archive-push` stops archiving everywhere. It's a stat and an `exec`, doctor's `backup.archiving` catches failures within its interval, and the manual test checks `pg_stat_archiver` on every cluster.
- **RTO depends on R2's download speed and on GitHub.** Database restore time is bucket throughput; the exporters come from GitHub during a rebuild (a failure is only `metrics_deferred`). The measured numbers go into MASTER.
- **A new IP means a DNS change.** Clients can't connect until `db1.<domain>` moves and caches expire; a short TTL on that record (set before a disaster) or a floating IP avoids it. The README says so.
- **The bundle and the identity.** Anyone with the recovery identity can read every bundle, which holds the host keys, the WireGuard key, the TLS key and the TOTP secret. The identity already opens every dump and is in the recovery kit; that's what the cold storage advice in MASTER's risks is for. Losing the identity loses the ability to rebuild: the bucket alone is then only database backups.
- **Records pruning loses restore leftovers.** A `<db>__pre_restore_*` kept for safety is gone with the disk; the rebuild reports it but can't bring it back. Its data is still in the stanza at an earlier time.
- **Serial recovery** makes RTO the sum of the databases' restore times. Fine for a handful; a server with many large databases may want parallel recovery later (`--jobs`), at the cost of disk contention.
- **A lost trigger.** If a writer forgets `MarkDirty`, the daily safety net still bundles within a day, and `server backup verify` reports `behind`. The trigger lives in the writers, not in the commands, to make that unlikely.
- **The no-bucket path is weak by design.** A local bundle dies with the VPS; only an export survives, and the data doesn't. Doctor nags; the README says to configure a bucket for production.

---

## v1 alpha readiness

What a first alpha release needs besides Phase 5. Gaps only, not designs.

**Releases and channels**

- **No stable release exists yet.** Only edge has been published; `git tag` is empty. The README's install line (`curl -fsSL https://vaire.mbvlabs.com/install.sh | bash`) fetches the stable root, which has nothing until the first `v*` tag.
- **The stable channel accepts only `X.Y.Z`.** `build-release.sh`, `release.matchVersion` and the signing identity regexp refuse a pre-release like `0.1.0-alpha.1`, and `upgrade.cmpSemver` falls back to string comparison for one. Decide between tagging `v0.1.0` on stable and calling 0.x the alpha (no tooling change), or adding pre-release support (four places plus `install.sh`), or a separate `alpha` channel (a third channel root, workflow, identity and `vaire upgrade --channel` value).
- **Compatibility: decided, nothing is frozen during the alpha.** `cluster.toml` schema 1, `vaire-secrets/v1`, `vaire/v1` JSON, `vaire-master-bundle/v1` and the step IDs may change between alpha releases, and a later alpha may require a server rebuild. Recorded in MASTER beside the v1 line; the README's alpha caveats should say it too.
- **Release notes.** There's no `CHANGELOG.md`, and `publish-stable.yml` uploads to R2 only: no GitHub release, no notes. `vaire upgrade` shows no notes either.
- **`LICENSE`: done.** MIT, at the repository root and linked from the README.

**Commands that v1 promises but don't work**

- `vaire upgrade` ends with "run vaire node upgrade", and `node upgrade` returns `not implemented yet` (Phase 8). Today nodes are upgraded through `apply --auto-upgrade-nodes`. Either the warning names that, or a minimal `node upgrade` lands before the alpha.
- `vaire wg export` is `not implemented yet` but the README's tools table offers `vaire wg export --install`.
- `vaire pg update` is `not implemented yet` (Phase 8); `unattended-upgrades` excludes PostgreSQL, so an alpha user has no supported path to a PostgreSQL minor update. Document a manual path or bring it forward.
- `vaire ext list|add|remove` (Phase 7) and `maint repack` are stubs; fine for the alpha if the README says so.
- MASTER's secrets section lists `vaire operator add|revoke` and `secrets export --rotate-operator`, which aren't in the tree (Phase 9). The README shouldn't promise them.

**Tests not yet run**

- Phase 4's manual test (it stays 🟡 until it passes), and Phase 5's.
- From earlier phases (MANUAL-TESTING): the lockdown self-revert, `server init --local`, and the edge install and `vaire upgrade` on Linux and macOS.

**Docs**

- The README's install section for a stable tag (and the edge one-liner for testers), the supported OS list, the "alpha" caveats (single master, soft isolation, no HA, a bucket recommended), and the DR runbook from this phase.
