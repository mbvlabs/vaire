# RFC: upgrading eru between major versions

Status: design (draft), 2026-10-16. Not implemented. This covers eru's own 1.x → 2.x upgrade, **not** PostgreSQL major upgrades (those are a separate, later problem). It answers the questions in the v1 release plan about what a major version promises and what has to land in 1.x before `1.0.0` final.

## 1. Inventory of persisted and wire formats

Every format eru writes or sends, where it lives, and whether it carries a version today. The `Versioned?` column is the point of the RFC: a format without a marker cannot be migrated safely, so it must gain one in 1.x.

| Format | Marker today | Where it lives | Who reads/writes |
| --- | --- | --- | --- |
| `cluster.toml` | `schema` int (currently 1) | node `/etc/eru/cluster.toml` | `config.Load/Save`, all steps |
| Secrets document | `"schema": "eru-secrets/v1"` | operator only, as an `age` file | `secretsdoc` |
| Operator context `context.toml` | none | `~/.config/eru/contexts/<name>/context.toml` | `contexts` |
| Operator `credentials.json` | none | same directory | `contexts` |
| Operator `wg.key`, `id_ed25519`, `known_hosts` | n/a (opaque) | same directory | `transport`, `contexts` |
| Operator `config.toml` (current context) | none | `~/.config/eru/config.toml` | `contexts` |
| Pending init state | none | `~/.config/eru/pending/<name>/{secrets.json,progress.json,bundle.tar.age}` | `contexts/pending` |
| Step journal | none | node `/var/lib/eru/journal.json` | `engine` |
| Node state | none | node `/var/lib/eru/state.json` (jobs, lockdown flags) | `engine` |
| Records | none | node `/var/lib/eru/records.json` (restores, dumps, backups, timelines, awaiting recovery) | `backup` |
| Maintenance/doctor caches | none | node `/var/lib/eru/maint.json`, `doctor.json` | `node` |
| Step IDs | `-vN` suffix, e.g. `postgres-roles-v1`, `pgbackrest-v1`, `master-backup-v1`, `kernel-tuning-v1` | wire (plan/apply), journal | `steps`, `engine` |
| RPC envelope | `"schema": "eru-rpc/v1"` | wire (node RPC over SSH stdout) | `rpc`, `node`, `remote` |
| Version handshake | exact `Version`+`Commit`+`Channel` match, no protocol version | wire (every RPC) | `node.checkVersion`, `remote` |
| CLI JSON output | `"schema": "eru/v1"`, events `"eru-event/v1"` | wire (stdout to operators, agents, scripts) | `cli/output` |
| Release manifest | `"schemaVersion": 1` | bucket `/releases/<v>/`, root, `/edge/`, `/<major>.x/` | `release`, `install.sh` |
| Tunnel socket protocol | `SharedProto = 1` (also in the fingerprint) | operator unix socket `contexts/<name>/tunnel.sock` | `transport` |
| Master bundle | `eru-master-bundle/v1`; sidecar `eru-master-sidecar/v1`; epoch `eru-master-epoch/v1` | node `/var/lib/eru/master-backup/` and the bucket | `masterbackup`, `node` |
| Dumps | `pg_dump` custom format, `age`-encrypted; identified by id in `records.json` | bucket | `backup` |
| Run log | none (NDJSON) | node `/var/log/eru/runs/`, `/var/log/eru/<job>.log` | `engine`, `node` |
| Prometheus text / `targets.json` | none | node `/var/lib/eru/*.prom`, exporters | `promfile`, `exporters` |
| TOTP protect state | none (JSON, root 0600) | node `/etc/eru/protect.json` | `node` |

Two of these are the load-bearing ones for a cross-major upgrade: **`cluster.toml`** (the desired state the node and the operator both hold) and **`records.json`** (what the node won't forget: open restores, dropped stanzas, awaiting recovery). The bundles in the bucket are the third.

## 2. What a major version promises

### Within a line (1.0 → 1.7)

- **Formats are additive.** A new field is optional and a reader tolerates its absence; an older reader must not choke on a file a newer minor wrote, except where a migration deliberately rewrites a marker.
- **Migrations are automatic, ordered and idempotent.** `eru upgrade` / `eru apply` brings every format up to the running build's version with no operator step, and re-running is a no-op.
- **A minor upgrade never needs a rebuild or restore.** No step requires re-running `server init`; backups and records are read in place.
- **CLI and node stay in lockstep.** The handshake demands an exact build match, so a line is upgraded as a whole. `apply --auto-upgrade-nodes` pushes the matching node binary during the handshake.
- **Downgrade within a line** is `eru upgrade --version <older>` plus the automatic re-read of formats (migrations are forward-only, so a binary that predates a migration refuses with `update_required` rather than reading a newer file).

### Across lines (1.x → 2.x)

- **A major may break formats.** It ships migrations that read the *previous* major's formats and write the new ones; unknown future versions are refused.
- **There is no cross-version wire.** A 1.x operator cannot call a 2.x node and vice versa: the handshake fails with `node_version_mismatch` (`update_required` when the operator is the older one). This is the constraint that shapes the whole flow (§3).
- **Only forward.** A downgrade across a major is not supported in place; the documented path is restore from the pre-migration backup or `server init --restore` from a bundle.
- **Old bundles stay readable for one major.** The new CLI reads the previous major's `eru-master-bundle/v1`. Whether a 3.x must still read a 1.x bundle is an open question (§7).

### Alpha and beta

No promise. `eru-secrets/v1`, `eru-master-bundle/v1`, the `eru/v1` JSON and the step IDs may change between `1.0.0-alpha.N` builds, and a later alpha may require a rebuild and restore. The promise above starts at `1.0.0` final (`docs/PLAN-v1-release.md`).

## 3. The upgrade flow end to end

### Preflight

Before anything is written, `eru upgrade` (and the cross-major `apply` it drives):

1. resolves the target manifest for the line and verifies its signature and that the version is on the running line (or that `--channel` explicitly moved lines);
2. refuses if any job is running, or offers to wait (`job.list`);
3. checks free space for the pre-migration backup of the formats it will rewrite;
4. plans the migrations: `eru plan` shows each format's `from → to` and the step instances that will re-apply, so the operator sees the point of no return before it is crossed;
5. refuses with `update_required` when the node's build already wrote a newer format than the operator understands (the "old CLI meets new node" case), pointing at the upgrade command.

### Ordering

The lockstep handshake forces the order:

1. **Operator CLI first.** Download and verify the new local binary (kept as `eru.prev`). It can no longer talk to the old node until the node is upgraded — which is exactly what the handshake's auto-upgrade does.
2. **Node binary**, pushed during the first handshake (`apply --auto-upgrade-nodes`), then the node's migrations run under the node lock in `eru apply`.
3. **Operator context files** migrate on this machine during `eru upgrade` / the next `eru login`.
4. **Re-apply steps**, because a format change may change what a step's `Inputs` hash to; `apply` re-runs the affected instances (for example a `postgres-roles-v1` or `pgbackrest-v1` whose inputs changed).
5. **Doctor** must pass before the upgrade reports success.

### The point of no return

The first format that writes a new marker — in practice `cluster.toml`'s `schema` and `records.json` — ends compatibility with an old binary. Everything before it (manifest verification, the local binary swap, the pre-migration backup) is reversible; after it, only the backup or a restore brings 1.x back.

### Rollback

- **Within a line:** `eru upgrade --version <previous>` restores the binary; formats are read as they are, because a minor only adds fields.
- **Across a major:** restore the pre-migration copies. On the node the migration writes a tarball under `/var/lib/eru/migrate/<from>/` before it rewrites anything; on the operator the same under `~/.config/eru/contexts/<name>/migrate/<from>/`. Rolling back means reinstalling the previous major's CLI and unpacking those copies, or, if the node is already 2.x, `server init --restore` from the last 1.x master bundle. A rollback is a documented, bounded operation, not a normal path.

### A second operator machine still on 1.x

It cannot run anything against the upgraded node: the handshake fails. The upgrade must therefore reach every operator machine in the same window. It cannot be reached over the wire from the first machine (that is the point of the no-cross-version rule); it upgrades from the release channel itself. Until it does, its context files stay 1.x and it holds no lease on the node. The release notes must say so: *upgrade every operator machine, or expect it to be unable to reach the server.*

### Old bundles in the bucket

- The root and each `/<major>.x/` channel keep publishing during the window, so a 1.x machine can still install a 1.x patch and a 2.x machine a 2.x patch.
- `server init --restore` on a 2.x build reads the newest 1.x master bundle (the previous major's reader, §2). A bundle older than that is refused with a clear `update_required`, not a parse error.
- The `epoch` fence and dropped stanzas are format-stable across the major: they are small and version-tagged, and the new reader understands the old tags.

## 4. Migration mechanics

### One ordered, idempotent chain per format

Each format gets a version (the markers missing in §1 are added in 1.x). A migration is a small function:

```
migrate/<format>/<from>_<to>.go   func Up(raw []byte) ([]byte, error)
```

- The chain is **ordered** (1→2, 2→3, …); the runner applies each step whose `<from>` is above the file's version, never skipping.
- Each step is **idempotent on the version marker**: after it runs, the file says `<to>`, so re-running the chain is a no-op.
- A format whose data is structured (TOML, JSON) migrates the decoded struct and re-encodes; a format with no natural version field gains one (`"schema": <int>`).
- A single worked example (even a no-op `v1 → v1`) must ship in 1.x to prove the plumbing, with a golden-file test.

### Backups and the record of what ran

- Before the first write to a format, the runner copies the current file to `/var/lib/eru/migrate/<from>/<file>` (node) or `contexts/<name>/migrate/<from>/<file>` (operator), 0600, and records the tarball's digest.
- A node-side `migrations.json` records, per format, the last version written and when. It is itself version-tagged and is part of the master bundle.
- Formats that carry their own marker (`cluster.toml` `schema`, `eru-secrets/v1`, `eru-master-bundle/v1`) do not need a separate record; the file is the record. `migrations.json` covers the ones that don't (journal, state, records, operator contexts) until they gain markers.

### Lockstep during the window

- The node runs migrations under the node lock, so two operators cannot race.
- An RPC whose request format the node no longer accepts fails the handshake before the handler runs; an RPC whose *reply* format the operator no longer understands is impossible, because the handshake pins the builds to the same release.
- A command that needs a migrated format from a not-yet-migrated peer is refused with `update_required` and a next step, never with a parse error.
- Operator context files migrate locally; the node does not depend on them.

## 5. Testing without docker

`AGENTS.md` forbids local harnesses; the strategy is golden files plus one manual run on the VPS.

- **Golden files of every old format**, checked in under `internal/.../testdata/migrate/<from>/`: a `cluster.toml` at schema 1, a `records.json` with an open restore and an awaiting-recovery entry, a `journal.json`, a `state.json`, an operator `context.toml` and `credentials.json`, an `eru-secrets/v1` document, and an `eru-master-bundle/v1` bundle. Unit tests run each migration over the golden file and assert the result, then run it again and assert it is unchanged (idempotence), then decode the result with the current reader.
- A `eru node migrate --check` (dry) reads every golden file through the real loader and reports what would change, so the plan is testable without mutating anything.
- **Manual, on the test VPS:** install the last `1.x` tag, create a database, take a backup and open a restore, then install the `2.x` build, run `eru upgrade`, `eru apply --auto-upgrade-nodes` and `eru doctor`; verify the database, the restore and `server backup list` still work. Then `server init --restore` from the 1.x master bundle onto a fresh VPS. Log both in `docs/MANUAL-TESTING.md`.
- No docker, incus or scratch VMs.

## 6. What must land in 1.x before `1.0.0` final

1. **A version marker in every persisted format that lacks one:** `context.toml`, `credentials.json`, `config.toml`, the pending init state, `journal.json`, `state.json`, `records.json`, `maint.json`, `doctor.json`, and the TOTP protect state. `cluster.toml` already has `schema`; the RPC, output, secrets, bundle and tunnel formats are already tagged.
2. **The migration skeleton:** the per-format registry, the ordered idempotent chain, `migrations.json`, the pre-migration backup, and the `update_required` refusal on both sides.
3. **One worked, tested migration** (a no-op `v1 → v1` or a real field addition) with a golden-file test and a second run proving idempotence.
4. **The previous-major bundle reader**, starting with `eru-master-bundle/v1`, so a 2.x build can `server init --restore` a 1.x bundle.
5. **The promise written down** (this RFC, condensed into `IMPLEMENTATION.md`) and the lockstep rule stated in `eru upgrade`'s help and `internal/guide/agents.md`.
6. **The release model hooks:** a `/<major>.x/` channel and a binary that follows its line (done), so a 1.x machine can stay on 1.x through the window.

The releases that add these are `1.0.0-alpha.N`, so the skeleton can be exercised well before the final.

## 7. Open questions

- **A protocol version in the handshake.** Should a 1.x operator be able to run a documented, read-only subset against a 2.x node (for example `status` and `doctor`)? Today the handshake is an exact build match, which makes the second-machine window (§3) total. A `proto` field could allow a read-only bridge.
- **How long to keep the previous major's readers.** One major back, or every major forever? Storage is cheap; test surface is not.
- **Lazy vs eager operator-context migration.** Migrate `context.toml`/`credentials.json` when loaded (a stray command silently rewrites them) or only at `eru upgrade` (a stray older binary then refuses clearly)? The node's files must be eager; the operator's could be lazy behind a `update_required`.
- **In-place cross-major rollback.** Is the pre-migration backup enough, or do we need a real down-migration for the small formats (`cluster.toml`, contexts) to make a rollback a one-liner?
- **Mixed fleets.** If a server ever runs more than one node (Phase 10), an upgrade window has nodes on two builds. Does the node lock plus a per-node `update_required` cover it, or does the fleet need a coordinated pause?
- **Alpha churn.** Do the alpha releases get any migration at all, or is "rebuild and restore" the documented cost until `1.0.0` final? (Current plan: rebuild and restore.)
- **Step IDs and re-apply.** A format change that alters a step's `Inputs` re-runs the step. Which steps are safe to re-run on a live server without a maintenance window, and which must be deferred to one?
