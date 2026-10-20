# RFC: one PostgreSQL cluster per database

> Status: **approved, revision 4** · 2026-10-13 · touches MASTER's decisions table, the Phase 3 scope and every later phase.
> The questions from revisions 1 and 3 are answered and recorded as **Decided** lines. Revision 2 added the comparison with managed providers (section 3), resource limits per cluster, restore as a fork plus a cut-over, and the session-mode PgBouncer. Revision 3 added the master node, sizes per database, oversubscription warnings and dedicated nodes. **Revision 4:** one hostname per server instead of one per database, with routing by database name and a database move that changes the URL's host (8.4, 7.6); several databases in one cluster considered and declined (5.1); how `sql`, `psql`, `console` and `tunnel` reach a database on a worker (6.3); followers and their commands (10.4, 11.3); what HA does for a compromised master (14.1); no bucket required at `server init`, with warnings and an exported bundle instead (6.5); and the master backup back in Phase 7 (Phase 5 since 2026-10-14). Every question is answered; the URL during a move is deferred to Phase 10.

## 1. Summary

Today a server runs one PostgreSQL 18 cluster (`18/main`) that holds every database. This RFC turns that around: **each database gets its own PostgreSQL cluster** (its own `pg_createcluster 18 <db>`, data directory, socket, WAL stream, pgBackRest stanza, configuration, restarts and systemd resource limits), and a server can host several of them.

This isn't the surface managed providers sell (they put one cluster on each VM), but it's the unit they build on. Their unit of isolation, backup, PITR, fork, resize, failover and billing is the instance (DigitalOcean's "database cluster", RDS's "DB instance"), not the database inside it. Kubernetes operators that pack many PostgreSQL clusters onto shared machines settle on one database per cluster, with cgroup limits for each. vaire does the same thing on VPSs the user owns: a database is a small dedicated instance, several instances share a server, and a later phase can give one its own server.

The decision is made: option C, now, inside Phase 3. The main gains are native physical restore and PITR per database, plus a per-database boundary for configuration, extensions, restarts, upgrades, resources and failures. The costs are density (roughly 50–100 MiB of fixed overhead per idle database on top of its `shared_buffers`), a memory budget vaire has to split explicitly, a second PgBouncer, and more moving parts for HA in Phase 11. The user-facing model barely changes: people still name a database, and its cluster sits underneath.

Every server has **one hostname** (`db1.<domain>`, `db2.<domain>`, …), and the database name in the URL's path picks the database, which is how PostgreSQL URLs already work. DNS grows with servers, not databases. Moving a database to another server changes its URL's host.

Around that sits a **master node**: the first server `server init` configures. It holds the desired state for everything vaire manages, operators reach every node through it, and it drives any node added later, including a node that holds a single database. Its own state is small, so it's backed up on every change as an encrypted bundle, enough to rebuild the master on a fresh VPS. Database recovery is separate, from pgBackRest. The master is a single point of failure for *changes* until HA. Each database has a **size** (memory, CPU weight, connections), defaulting to something sensible and adjustable up or down. vaire warns when a server is oversubscribed but leaves the choice to the user.

## 2. Where we are

- Phases 1 and 2 are done. Phase 3 (pgBackRest, per-database restore, fork, drill, long-retention dumps) is merged (`c5fda49`, `344566b`) and waiting for its manual VPS test.
- One server = one context = one `cluster.toml` = one PostgreSQL cluster `18/main` on 5432, behind PgBouncer on 6432 and nginx's ALPN routing on 443. Each database has `<db>_owner/_migrate/_app/_ro`. The cluster-wide roles are `vaire_admin`, `vaire_monitor`, `pgbouncer_auth` and `vaire_direct_only`.
- Backups and PITR are cluster-wide: one WAL stream, one stanza named after the cluster. "Restore one database" means a `pgbackrest restore --db-include=<db>` into a scratch instance on port 5499, then `pg_dump | pg_restore` into `<db>__restoring` on the live cluster, then a rename swap on promote. Fork uses the same pipeline.
- The first VPS test already turned up problems in exactly that area: the restore window's start second, count-based retention that shrank the window, restarts that went through without the confirm token, a failed restore blocking the next one, and the `pg_stat_statements` owner warning that marks every restore. All of them are fixed or understood, but they show how indirect the path is.

### What is shared today, and what that costs

| Shared across every database | Consequence |
| --- | --- |
| WAL stream, stanza, retention | PITR exists only for the whole cluster. A per-database restore is a logical copy of a physical restore: slow (dump and reload, then rebuilding every index), about 2× the database's size in free space, and lossy at the edges (extension ownership, large objects, grants outside `public`) |
| `postgresql.conf`, tuning | One profile, one `work_mem`, one `max_connections` for every workload |
| `shared_preload_libraries` | The union over all databases. Adding `timescaledb` for one database restarts every database and loads it into all of them |
| Restarts | Any restart-only change is downtime for everyone |
| Major and minor upgrades | All databases move together |
| Failure | One runaway query, a full WAL volume, an OOM kill or a corrupt catalog takes every database down with it |
| Replication (Phase 6 followers, Phase 10 replicas, Phase 11 HA) | Physical replication is all or nothing; followers had to be logical, with their DDL, primary-key and sequence caveats |

## 3. How managed providers do it

### 3.1 DigitalOcean

DigitalOcean's managed PostgreSQL looks like vaire today: you buy a **database cluster** and create several databases inside it. Underneath:

- A cluster is **one PostgreSQL instance on its own droplet-sized node**, plus optional standby nodes (three nodes at most) and read-only nodes. Every database in it shares that instance's memory, connections, configuration and extensions.
- **Every lifecycle operation works on the whole cluster.** PITR covers the last 7 days, and both a restore and a fork **create a new cluster** with all of the source's databases and users. Nothing restores one database, in place or otherwise. For one database, the docs point to `pg_dump`. After a restore, standby and read-only nodes have to be added again.
- **Connections:** each cluster has its own hostname, with a direct port (25060) and a PgBouncer port (25061). You create pools per database and user, each with a mode (transaction, session or statement) and a size. Backend connections scale with RAM, about 25 per GiB, from 22 on the 1 GiB plan up to 997, with 3 reserved for maintenance and at most 21 pools.
- No superuser, and a fixed list of extensions.

The isolation DigitalOcean sells is **one cluster per VM**. Several databases in one cluster are a convenience for things that live and die together. Customers who need a database restored, resized or upgraded on its own buy another cluster, on another VM. vaire today copies the "many databases in one cluster" surface and then promises something DigitalOcean doesn't: restoring one of them. That promise is where the indirect pipeline and its bugs come from.

### 3.2 The rest of the market

| Provider | Unit | Databases per unit | Restore and PITR | Placement |
| --- | --- | --- | --- | --- |
| DigitalOcean | Database cluster | Many | Whole cluster, always into a **new** cluster | One VM per node |
| AWS RDS, Google Cloud SQL, Azure Flexible Server | DB instance / instance / server | Many | Whole instance, always into a **new** instance, then you switch the connection string | One VM per instance |
| Crunchy Bridge, Aiven | Cluster / service | Many | Whole cluster, into a new one (fork) | One VM per node |
| Heroku Postgres | "Database" add-on | One in practice (one URL, one database) | Fork and rollback make a **new** database at a point in time (Standard tier and up) | Dedicated instance from Standard up |
| Supabase | Project | One main database (`postgres`) | Whole project | One VM per project |
| Neon | Project → branch | Several per branch | Branch reset or a new branch (copy-on-write storage) | Shared storage, separate compute per branch |
| CloudNativePG (Kubernetes) | Cluster | **One, by design.** The maintainers refuse multi-database clusters | Whole cluster, into a new cluster | Many clusters per node, each pod with CPU and memory requests and limits |

The pattern:

1. **Isolation, backup, PITR, resizing, failover and upgrades all happen per instance.** Databases inside one instance share all of them. No major provider restores one database out of a shared instance.
2. **A restore makes a new instance**, which you inspect and then cut over to. The cut-over is a connection-string or DNS change.
3. **Each instance has its own hostname.** For a provider that's one DNS record per VM they already run. vaire deliberately doesn't copy this (8.4).
4. **Pooling is per database**, with a mode and a size, next to a direct endpoint for sessions that need one.
5. When instances share hardware (CloudNativePG, Neon's compute, the cheap tiers of the hosted services), **each one gets resource limits from the scheduler.** Otherwise a database's "size" is only a tuning hint.

### 3.3 Do providers run "many clusters per server, one database each"?

Partly, and it depends on which layer you look at.

- **What a customer sees: no.** At DigitalOcean, RDS, Cloud SQL, Azure, Crunchy Bridge and Aiven, one instance is one PostgreSQL cluster on its own VM, and that cluster may hold many databases. None of them puts two of a customer's clusters on one VM, and only CloudNativePG *forces* one database per cluster. Heroku and Supabase come close in practice (one URL, one main database), but by convention.
- **What the provider runs: yes.** Underneath, every one of them packs many instances onto one physical host. Those instances are separated by a hypervisor (each VM gets its own kernel, memory and disk quota) or by containers with cgroup limits (CloudNativePG's pods, Neon's computes). "Many PostgreSQL clusters on one machine" is the normal way to build a managed service. The customer never sees the machine.
- **Where vaire sits:** the user's VPS plays the role of the provider's physical host, and vaire plays the provider. vaire can't start a VM per database inside a VPS, so its boundary is the one container platforms use: a cluster per database with cgroup limits. That's the CloudNativePG pattern, not the DigitalOcean one. DigitalOcean's surface on a single VPS ("one cluster, many databases") is what vaire has today. What makes it work at DigitalOcean is the cheap VM per cluster behind it, and a VPS doesn't have that.

So the claim isn't "providers do exactly this". It's this: providers make the PostgreSQL cluster the unit of isolation, backup and restore, and they put many of those units on shared hardware with a hard boundary between them. Option C gives vaire the same unit, with the strongest boundary a single VPS offers. The part providers offer that vaire can't on one VPS (a VM between databases) comes back as placement in Phase 10: a database on its own server.

### 3.4 What that means for vaire

vaire can't put each database in its own VM on a VPS the user already owns. The closest professional pattern for that setting is CloudNativePG's: **one database per cluster, many clusters per machine, each with resource limits**, and a cluster's placement as something that can change. Option C is that pattern. Compared with revision 1, it means:

- **Resource limits move into Phase 3** (section 7.2). Without them, `memory = "1GiB"` describes tuning, not a guarantee, and one database's runaway query can still starve the rest of the server.
- **One hostname per server, not per database** (section 8.4). Providers give each instance a hostname because each instance is a VM they already put in DNS. For vaire that would be a DNS record per database in the user's DNS provider. Instead, a server has one name and the database name routes, as PgBouncer already does. The price is that moving a database to another server changes its URL's host.
- **Restore is a fork plus a cut-over** (section 10). vaire keeps an in-place swap because a VPS user can't hand out a new connection string as easily as a cloud console can, but the primitive is the provider's: build a new cluster at a point in time, inspect it, then either make it the database (`restore promote`) or keep it as a new one (`db fork --at`). One code path.
- **Pooling matches DigitalOcean's split:** a transaction pooler for applications and a session pooler for migrations and tools (section 8). Pool sizes become per-database settings.
- **Several databases in one cluster aren't offered** (5.1). Things that belong together share one database with several schemas.
- **"Its own server" is the dedicated plan.** A large production database placed on its own node in Phase 10 is option B for that one database, the same thing as a provider's bigger instance.

Where vaire stays below a managed provider, and should say so in the README: no VM boundary between clusters on one server (cgroups instead), I/O fairness that depends on the disk scheduler (section 7.2), a URL that changes when a database moves, and HA only from Phase 11.

## 4. Terms

The word "cluster" is overloaded in vaire today: it names the server, the context, the desired-state file and the PostgreSQL cluster. This RFC uses:

- **Server** (or **node**): one VPS. It has WireGuard, sshd, ufw, nginx, one hostname and its certificate, both PgBouncers, the vaire node binary and the journal. The master also holds the desired state. A context points at the master.
- **Cluster**: a PostgreSQL cluster in the PostgreSQL and `postgresql-common` sense: `pg_createcluster 18 <name>`, one data directory, one socket, one postmaster, one WAL stream. It's an internal building block, the equivalent of a provider's instance, not something the user manages directly.
- **Database**: the application database. **Each cluster holds exactly one, with the same name.** So `app` is the database, the cluster `18/app`, the stanza `app-<id>` and the unit `postgresql@18-app`, and it's reached at `db1.<domain>/app`.

Because a database and its cluster are 1:1 and share a name, every existing command that takes `<db>` still says what it means. No new noun is needed for day-to-day work.

## 5. The model

| | A. Many clusters on one server | B. One server per database | C. A then B: clusters with a placement |
| --- | --- | --- | --- |
| Shape | `18/app`, `18/billing`, … on one VPS, each with its own data directory | Today's code with at most one database per server; every database is its own VPS, context and handoff | A cluster is the unit, and it has a `node` it runs on. Phase 3 puts every cluster on the one server; Phase 10 lets a cluster live on another node |
| Isolation | WAL, config, restarts, extensions, crashes, and memory and CPU through cgroups. Disk I/O and the page cache only partly | Total | As A, and as B for clusters placed on their own node |
| Density | Good: about 50–100 MiB fixed per cluster plus its buffers | Poor: a VPS (and a WireGuard endpoint, a certificate, a secrets handoff) per database | As A by default |
| Provider analogue | CloudNativePG's clusters on shared nodes | A provider instance per database | Both, per database |

**Decided: C.** In Phase 3, every cluster lives on the master node (section 6) (`node` defaults to it and nothing else validates). The design keeps a cluster portable: its own stanza, its own data directory, and clients that reach it by database name through PgBouncer.

### 5.1 Several databases in one cluster (considered in revision 4)

There's no technical reason against it: if every cluster-level operation (restore, PITR, size, move, follower) acted on the whole cluster, as at DigitalOcean, it wouldn't need a second restore path. It would still be a second shape to document, test and explain in every cluster-level command's output.

**Decided: no. One database per cluster.** Things that belong together share one database with several schemas. Small databases pay the per-cluster overhead.

## 6. The master node

**Assumption: one operator.** Everything in this section is designed for a single person running vaire, possibly with agents acting as that person. Teams (several operators, scoped access, concurrent edits, auditing) come later and build on it. A proper team setup needs auditing and access control that aren't worth building now. Where a choice would differ for teams, it's noted, not designed.

### 6.1 Master and workers

- **The master node** is the first server `server init` configures. It stays the master for the life of the setup, unless it's rebuilt (6.6) or, later, handed over (6.7). It holds:
  - the **desired state for everything**: `cluster.toml` with the server settings, every node, and every cluster with its placement and size;
  - the **registries**: the operator (`operators.toml`) and, from Phase 10, the nodes (`nodes.toml`: name, hostname, WireGuard key and address, public address);
  - the **durable records** that exist nowhere else: timeline switches after `restore undo`, dropped stanzas and when they expire, the drill and verify history, the last restore per database. They are split out of `state.json` into `/var/lib/vaire/records.json`, so the ephemeral parts (jobs, lockdown flags) stay out of backups;
  - the **WireGuard hub** (`10.42.0.1`), the only peer the operator's CLI has;
  - its own hostname, certificate and the ACME account;
  - and **clusters**, like any node. In Phase 3 it holds all of them.
- **A worker node** (Phase 10) is added from the master: `vaire node add db2 --host root@<ip>`. It holds only what the master pushes to it: `node.toml`, its slice of the desired state rendered into config, its own journal and job state. It runs its clusters, its own PgBouncers, nginx, its hostname's certificate, and **its own timers** (backups, verify, drills, dumps). The data plane on a worker doesn't need the master.
- **Rules:**
  - Only the master accepts changes to desired state. A worker refuses `config.set`, `db create` and the rest unless the request comes from the master.
  - Every change flows operator → master → workers. `apply` on the master plans the whole fleet, applies the master's own steps first, then each worker's.
  - Workers don't control each other. Replication between them (Phase 6 followers, Phase 10 replicas) is data plane over the mesh.
  - **Vocabulary:** "master" names only this control role. PostgreSQL replication keeps "primary" and "standby", so `vaire` never says "master" about a database.

### 6.2 How the operator reaches workers

**Decided: through the master.** Hub and spoke, one peer:

- The operator has one WireGuard peer, the master, with `AllowedIPs` covering the node addresses. The master forwards `wg0 → wg0` only (`net.ipv4.ip_forward` plus a ufw route rule limited to `wg0`; it's still not a gateway to anything else). Workers accept WireGuard only from the master and from each other, and route the operator's addresses back through the master.
- vaire commands for a worker go to the master, which runs them on the worker with a **fleet SSH key**: generated at `server init`, private half only on the master (and in its backup), authorised on every worker's admin user, and restricted to `vaire node rpc`.
- Database connections from the operator's tools are plain packets through the hub (6.3).
- Applications never go through the master. They connect to the hostname of the node that holds their database.

What it costs: with the master down, the operator can't reach workers through vaire. Break-glass is the same as today: the provider console and `vaire node unlock-ssh` on the worker. For teams, direct peers on every node may make sense later, and they're compatible with this.

### 6.3 How `sql`, `psql`, `console` and `tunnel` reach a database (new in revision 4)

**Today:** the CLI brings up a userspace WireGuard tunnel inside its own process (no `wg-quick` on the operator's machine). The server is its only peer, and only the node /24 routes through it. `vaire sql` dials `10.42.0.1:5432` through the tunnel, verifies TLS against the server's hostname, and logs in as `vaire_admin`, read-only unless `--write`. `psql` and `console` open a local listener on `127.0.0.1` that forwards through the same tunnel and point the tool at it. `tunnel` does the same and stays up.

**With workers, only the address inside the tunnel changes:**

1. **Where is `app`?** The CLI looks up the database's node: its WireGuard address and hostname. The context caches this placement map and refreshes it from the master whenever a command talks to the master, and again when a node answers "database does not exist" for a database the cache put there.
2. **The route.** The tunnel's only peer is still the master, and its allowed range is still the node /24, which already includes every worker's address. A packet for `10.42.0.2` goes to the master, which forwards it out of `wg0` to the worker (6.2). Replies come back the same way. There's no second peer, no SSH hop and no extra port on the operator's machine.
3. **The endpoint** is the worker's session pooler on `10.42.0.2:5432`, the same port as on the master. TLS is verified against the worker's hostname (`db2.<domain>`), which its certificate carries.
4. **The login** is `vaire_admin` with the setup-wide service password (section 12), read-only unless `--write`. `--db app` is the database name the pooler routes by.

So `vaire sql --db app` works the same wherever `app` lives. `vaire tunnel --db app --target postgres` forwards to the right node and prints `postgres://…@127.0.0.1:6544/app`. Without `--db`, it targets the master, as today. `--target pgbouncer` reaches that node's transaction pooler on 6432 the same way.

**Rejected:** the master's session pooler forwarding to the workers' poolers. That would need pooler-to-pooler authentication over the mesh, and every operator query would depend on two poolers. Forwarding at the WireGuard layer is simpler, and the master only ever sees encrypted packets.

When the master is down, these commands can't reach workers. Applications aren't affected (6.7).

### 6.4 What the master backup holds

A full VPS backup isn't the goal. The goal is **everything that's needed to make a fresh VPS into this master again** and that can't be regenerated. Everything else either comes from the desired state, comes from the operator's secrets document, or is data.

| Class | Examples | In the master backup? |
| --- | --- | --- |
| **Born on the master, not regenerable** | `cluster.toml`, `operators.toml`, `nodes.toml`, `node.toml`, `records.json`; the WireGuard private key (`/etc/wireguard/vaire.key`); the SSH host keys; the fleet SSH key; the certificate and key in `/etc/vaire/tls`; the ACME account in `/etc/vaire/acme` | **Yes** |
| **Supplied by the operator** | Bucket keys, pgBackRest cipher pass, the dump age recipient, the `pgbouncer_auth` and `vaire_monitor` passwords, the metrics credential | No. They're re-sent from the operator's credentials or secrets document during the rebuild, by the same top-up `apply` uses today |
| **Rendered by steps** | `wg0.conf`, sshd drop-ins, ufw rules, nginx and PgBouncer config, `pgbackrest.conf`, systemd units and timers, sysctl, `userlist.txt` | No. Regenerated from the classes above |
| **Data** | `<data_root>`, WAL, temporary clusters, stanzas, dumps | No. pgBackRest covers it (section 9) |
| **Not needed** | The journal (a fresh host runs every step anyway), job state, logs, `/home`, `/root`, packages | No |

**Decided: the bundle holds only what's born on the master.** The bucket never holds the keys to itself.

Keeping the WireGuard key and the host keys is what makes a rebuild invisible to the operator: their peer config and pinned `known_hosts` still match. Keeping the certificate and the ACME account avoids a burst of issuance. Keeping `records.json` keeps PITR correct after an undo.

### 6.5 The bundle

- **Format:** a tar of the class-A files with a `manifest.json` (vaire version, schema, server, node, desired-state revision, timestamp, path, owner, mode and sha256 per file). It's encrypted with age to the **recovery kit's recipient**, the one the dumps use. The identity lives only in the recovery kit and the operator's credentials, so neither a compromised node nor a leaked bucket key can read old bundles. Encryption and upload are in-process (Go `age` and `minio-go`), with nothing on disk in the clear and no secret in argv. Today `backup setup` creates the recovery identity. Because a bucket is now optional, `server init` creates it instead.
- **Where:** `<prefix>/master/<UTC ts>-r<revision>.tar.age`, next to an **unencrypted sidecar**, `….manifest.json`, that holds the file list and hashes but no content.
- **When:** after every change to a class-A file. That covers `apply` and `config set`, `db create/drop/fork/resize`, `restore promote/undo`, `node add/remove`, certificate renewal, and operator key changes. The writing command marks the state dirty and starts `vaire-master-backup.service`. systemd won't start a second copy while one runs, and the job loops until the state is clean, so a burst of changes makes one or two bundles. A **daily timer** is the safety net: it uploads only when the content hash differs from the last bundle's.
- **Retention:** every bundle from the last 30 days, then the newest of each week for a year. Bundles are kilobytes.
- **Checks:** the node compares the latest sidecar with its live files every day. A missing or stale bundle is a `doctor` failure and an alert (Phase 9). `vaire server backup verify` runs on the operator's machine, which has the identity: it downloads, decrypts and validates the latest bundle.
- **Commands:** `vaire server backup now|list|verify|export`.

**Without a bucket. Decided: `server init` doesn't require one.**

- The job still builds a bundle on every change and keeps the last ten in `/var/lib/vaire/master-backup/`. That's useful after a bad change and useless when the VPS is lost.
- Until a bucket exists, `server init` ends with a warning, and `doctor`, `status` and every `apply` warn `no_offsite_backup` with the fix (`vaire backup setup`).
- `vaire server backup export [--out <file>]` copies the latest bundle, still encrypted, to the operator's machine, and `server init --restore --bundle-file <file>` rebuilds from it.
- `backup setup` uploads a bundle as soon as the bucket works.

Losing both the bucket and the recovery kit means the master can only be re-created, not rebuilt. That's what explicit storage is for.

### 6.6 Rebuilding the master

`vaire server init --restore --host root@<new-ip> [--bundle <ts> | --bundle-file <file>]`, run by the operator with their context (or the secrets document):

1. **Fetch and check:** list `<prefix>/master/`, download the latest bundle (or the one given), decrypt it on the operator's machine, and validate the manifest: the same server, and a schema this vaire can read.
2. **Fence the old master:** write `<prefix>/master/epoch` with a new number and require the confirm token `<server>/replace`. Every vaire job that writes to the bucket (backups, dumps, bundles) checks the epoch first and stops if it's stale, so an old master that comes back can't keep writing. This doesn't fence `archive-push`, which pgBackRest runs per WAL segment. That's covered by step 5 and by the rule "power off or destroy the old VPS first", which the command states. With `--bundle-file` and no bucket there's nothing to fence, and the rule is all there is.
3. **First contact and preflight**, as `server init` does today (Debian 13 or Ubuntu 26.04).
4. **Place the class-A files** through the node RPC on stdin, before any step runs: host keys, WireGuard key, fleet key, TLS, ACME, desired state, registries, records.
5. **Run every server-level step** from the restored `cluster.toml`, with the operator's top-up supplying the secrets. Commit-confirm switches SSH onto WireGuard as usual. The keys match, so the operator's existing context works. Every cluster is recorded as **`awaiting_recovery`**: no `pg_createcluster`, no pooler entry, no timers. An empty cluster archiving into an existing stanza would be wrong, so nothing creates one implicitly.
6. **Databases, separately:** `vaire db recover <db>` or `--all` materialises each cluster from its stanza (section 10) and opens it. This is the database recovery that the master backup deliberately leaves out.
7. **Addresses:** if the IP changed, the master's DNS record (`db1.<domain>`) has to move. Contexts store the master's hostname, not its IP, so for the operator the DNS change is all there is. Workers learn the new endpoint from the master on the next `apply`. A worker that can't reach the master keeps serving.

**Targets for a single server:** state RPO is about zero (bundles follow every change), and database RPO is `archive_timeout` (60 s). RTO is a new VPS, about ten minutes of steps, plus each database's restore time. The Phase 5 manual test measures it and writes it into MASTER's risks.

### 6.7 The single point of failure

| While the master is down | Phase 3 (one server) | Phase 10 (master + workers) |
| --- | --- | --- |
| Databases on the master | Down | Down |
| Databases on workers | — | **Keep serving**: own poolers, own hostname and certificate |
| Backups, WAL archiving, drills | Stop | Workers' timers keep running |
| Changes (`apply`, `db create`, resize, restores) | Impossible | Impossible fleet-wide |
| Operator access to workers (`sql`, `ssh`, …) | — | Only through break-glass (6.2) |
| Certificate renewal | Stops | Workers renew their own |

The way out, in steps:

1. **Phase 5:** fast rebuild (6.6) with bundles that are never more than seconds old.
2. **Phase 10, `vaire server takeover --node <worker>`:** turn an existing worker into the master from the latest bundle. Bump the epoch, move the hub address, keep the fleet key, and point the operator's context at the new master's hostname (the command runs on the operator's machine). That cuts the RTO to minutes for the control plane and leaves only the old master's databases to recover. It's cheap because the bundle already is the full control state.
3. **Phase 11 (HA):** the desired state and records move to a replicated store (the etcd Patroni already needs), or a standby master follows the bundles continuously. Either way the master stops being a single point. Each master gets its own keys (14.1).

### 6.8 Contexts, WireGuard and naming

- **WireGuard and SSH** stay per server: `wg0`, operator peers, commit-confirm lockdown and break-glass. The master is `10.42.0.1`, and workers get addresses from the master.
- **Contexts point at the master** by its hostname. `context.toml` keeps `domain`, `tls_mode` and the operator. Its `cluster` field becomes `server`, and its `nodes` list becomes a cache: each node's name, hostname and WireGuard address, plus the database placement map (6.3). `vaire context show` lists nodes and databases from the master.
- **`/etc/vaire/node.toml`** gets `server`, `hostname` and `role = "master" | "worker"`.
- **HA (Phase 11)**: Patroni manages one PostgreSQL cluster per scope. N clusters means N Patroni processes per node, sharing one etcd, each failing over on its own. That's more processes and more routing, but it's the only way to fail over one database without the others, which is what providers offer per instance. See sections 15.2 and 16.

## 7. Resources on one server

### 7.1 The cost of one more cluster

An idle PostgreSQL 18 cluster runs a postmaster plus about ten auxiliary processes (checkpointer, background writer, WAL writer, autovacuum launcher, archiver, logical replication launcher, three or more I/O workers), and a pgBackRest `archive-push` process whenever it archives. Beyond its `shared_buffers`, expect **about 50–100 MiB resident** for an idle cluster with small settings, plus its shared memory for lock tables and connection slots. That's an estimate to measure in the manual test, not a promise.

Other per-cluster costs:

- **Disk:** `min_wal_size`, `max_wal_size` and the archive spool are per cluster, so they're split, not repeated (7.7).
- **Bucket:** one stanza each. `archive_timeout = 60s` makes an active cluster push a segment a minute; an idle one pushes nothing. Requests and storage grow with the number of active clusters. Backups get staggered schedules (9.3).
- **Restarts and reloads:** per cluster, which is the point. A server-wide change (a certificate, a minor PostgreSQL update) touches each cluster in turn.

### 7.2 Resource limits per cluster

**Decided: Phase 3,** with `MemoryHigh`, `CPUWeight` and `IOWeight` by default, and `MemoryMax` and `CPUQuota` opt-in.

Each `postgresql@18-<cluster>.service` gets a systemd drop-in, written by `postgres-cluster-v1@<cluster>` from the cluster's config. systemd puts each instance of the template unit in its own cgroup, so the limits cover the postmaster and every backend, and **shared memory is charged to the cluster that touches it**. With one cluster per cgroup, `shared_buffers` lands in the right place.

| Setting | Value | Why |
| --- | --- | --- |
| `MemoryHigh=` | `memory` + a 25% allowance for backends | Above it the kernel reclaims and throttles **that cluster** first, instead of the whole server reaching the global OOM killer. This is the "size" a provider's instance has |
| `MemoryMax=` | Unset by default; `memory_max` in config to opt in | A hard cap kills a backend inside the cgroup. PostgreSQL then restarts every backend of that cluster and runs crash recovery. That's acceptable for one database, but it surprises people, so it's off unless asked for |
| `CPUWeight=` | From the size (7.4), `cpu_weight` to override | Fair sharing under contention; no cap when the CPU is idle |
| `CPUQuota=` | Unset; `cpu_quota` (for example `"150%"`) to opt in | A hard ceiling for a noisy staging database |
| `IOWeight=` | 100, `io_weight` to override | Only effective when the block device uses BFQ or the kernel's `io.cost` model. On a VPS virtio disk with `none` or `mq-deadline` it does nothing. **To verify** on Debian 13 and Ubuntu 26.04. vaire reports it as "not enforced" in `tune show` when that's the case, rather than pretending |
| `OOMScoreAdjust=` | -900 for the postmaster (as today), per cluster | The global OOM killer still prefers vaire jobs and backends over postmasters |

The PgBouncers, nginx and vaire's own jobs run in `system.slice` as today. The server reserve (7.3) is what keeps them alive. `vaire tune show` prints each cluster's limits and its current `memory.current`, and doctor (Phase 4) warns when a cluster stays above `MemoryHigh`.

### 7.3 A memory budget instead of "25% of RAM"

Today `internal/tune` sizes everything from total RAM. With several clusters, vaire has to split memory explicitly, or the second cluster tunes itself as if it owned the box.

- **Server reserve.** The OS, both PgBouncers, nginx, vaire jobs and the page cache keep `max(512 MiB, 25% of RAM)`. The rest is the **cluster budget**.
- **Each cluster has `memory`** (for example `"1GiB"`) in its config. `internal/tune` treats it as that cluster's `RAMMiB`: `shared_buffers`, `work_mem`, `maintenance_work_mem` and `effective_cache_size` come from it with the current formulas, and `MemoryHigh` from 7.2 enforces it. vCPU stays the server's, but `max_worker_processes` and `max_parallel_workers` are capped at `vCPU / clusters` (minimum 2), so N clusters can't each plan for the whole CPU.
- **Sizes and defaults** are in 7.4. vaire **never rebalances existing clusters** on its own: shrinking one means a restart, and that's the user's call.
- **`max_connections`** per cluster comes from its size (7.4), with a floor that fits both pools plus the reserved slots (8.1).
- **Huge pages.** `vm.nr_hugepages` becomes the **sum** of each cluster's estimate (`ceil(shared_buffers × 1.05 / 2) + 64`, with the stopped-server measurement where a restart allows it). `huge_pages = try` only for clusters with `memory ≥ 2 GiB`. Smaller ones use `off` so that a fragmented pool can't leave one cluster's start-up at the mercy of another's.
- **`vm.overcommit_memory = 2`** stays. Its `CommitLimit` decides what can start at all (7.5).
- **`vaire tune show`** gets a server view: RAM, reserve, each cluster's size and limits, what's allocated, and what's free or overcommitted.

### 7.4 Sizes: compute per database

Every cluster has a **size**, the way a provider's instance has a plan. A size sets the memory (which drives tuning and `MemoryHigh`), the CPU weight and the connection limit together, so "less for staging, more for production" is one word:

| Size | `memory` | `cpu_weight` | `max_connections` | Typical use |
| --- | --- | --- | --- | --- |
| `nano` | 256 MiB | 25 | 20 | Preview environments, a tiny side project |
| `small` | 512 MiB | 50 | 30 | Staging |
| `medium` | 1 GiB | 100 | 50 | Small production |
| `large` | 2 GiB | 200 | 100 | Production |
| `xlarge` | 4 GiB | 400 | 200 | A busy database, usually on its own node (7.6) |

- **Explicit values win.** `size = "small"` plus `memory = "768MiB"` is a small with more memory. `size = "custom"` means only the explicit keys count. `cpu_quota`, `memory_max` and `io_weight` (7.2) stay separate opt-ins.
- **A decent default.** `[postgres] default_size` (default `medium`) applies when `db create` gets no `--size`. If it doesn't fit in the free budget, `db create` is refused like any other size that doesn't fit, and the error names the largest size that does, with the command (7.5). vaire never picks a smaller size by itself.
- **Resizing up or down:** `vaire db resize <db> --size large` (or `config set clusters.app.size large`). The cgroup limits and reload-only settings change live. `shared_buffers`, `max_connections` and `huge_pages` need a restart, so the plan shows "restart needed" and asks for `<cluster>/restart`. With `--defer-restart`, the rest applies now and the restart waits for the next maintenance window. Growing past the budget is refused unless `--allow-overcommit` (7.5). Shrinking below what the cluster uses right now is a warning, not a refusal.
- **Per-node defaults later:** a worker added as a dedicated node (7.6) gives its one database the whole node's budget by default.

### 7.5 Oversubscription: refused unless allowed

The user decides how full a server gets, but overcommitting is an explicit choice. vaire refuses a new or larger size that would overcommit the server unless the user passes `--allow-overcommit`, makes the consequences visible, and always refuses what can't work.

| Situation | What vaire does |
| --- | --- |
| **Fits:** the sum of sizes ≤ the cluster budget | Nothing |
| **Overcommitted:** the sum > budget, but every cluster's shared memory still fits within `CommitLimit` and the huge-page pool | **Refused** in `db create` (with or without `--size`), `db fork` and a growing `db resize` when the new or larger size takes the sum past the budget: `precondition_failed`, reason `memory_overcommitted`, with the numbers ("big2 at medium (1 GiB) would put 6 GiB of database sizes on a 5.7 GiB budget") and the largest size that still fits as a complete command (`vaire db create big2 --size small`), or, when none fits, that none does and to resize another database down or use a bigger server. Nothing steps down by itself. With `--allow-overcommit` it goes ahead with the **warning** `memory_overcommitted`, which `plan`, `status` and `doctor` repeat with the options: shrink these, move one to a node, or a bigger VPS. Under load, the result is throttling (`MemoryHigh`) and a smaller page cache, not crashes. Shrinking is never refused |
| **Crowded CPU:** clusters > 2 × vCPU, or the sum of `cpu_quota` > 100% × vCPU | Warning `cpu_crowded`. Weights are relative, so CPU is never "over" by itself |
| **Disk:** data sizes plus each `max_wal_size` plus the archive spool > 80% of the disk | Warning `disk_crowded`, and a doctor failure at 90% |
| **Can't start:** the cluster's shared memory doesn't fit in what's left of `CommitLimit`, or huge pages are required and the pool can't grow | **Refused**, `would_not_start`, with the arithmetic. PostgreSQL would fail at start-up anyway, so this isn't policy. The hint is the smallest size that would start |

The only override is `--allow-overcommit`, for the memory budget; the other warnings never block. Every warning has a stable code for agents, and `vaire tune show` always prints the server's sheet:

```
server db1   8 GiB RAM · 4 vCPU · reserve 2 GiB · budget 6 GiB
  app            large    2 GiB   cpu 200   running
  app_staging    small  512 MiB   cpu  50   running
  billing        medium   1 GiB   cpu 100   running
  allocated 3.5 GiB of 6 GiB · free 2.5 GiB
```

Rough guidance for the README: a 2 GiB VPS takes 2 `small` and 1 `nano`; a 4 GiB VPS takes 1 `medium` and 3 `small`; an 8 GiB VPS takes 1 `large`, 2 `medium` and 2 `small`. That's the same order as DigitalOcean's smallest plan (1 GiB per cluster), on hardware the user already pays for. The manual test calibrates these from the measured per-cluster overhead.

### 7.6 A node for one database (Phase 10)

When a database outgrows its share, or production should be kept away from staging, the user adds a node for it. It's still driven from the master:

- `vaire node add db2 --host root@<ip> --dedicated app`: the master bootstraps the node (hardening, WireGuard to the master, the fleet key, the base steps, its hostname `db2.<domain>` and certificate) and records it in `nodes.toml`. The operator adds one DNS record for `db2.<domain>`. `--dedicated` reserves the node for the named databases: vaire won't place anything else there unless asked, and `app` defaults to the node's whole budget.
- `vaire db create billing --node db2 --size xlarge` creates a database there directly, at `db2.<domain>/billing`.
- `vaire db move app --node db2` moves an existing one. The master materialises `app` on `db2` from its stanza (section 10), streams WAL from the live cluster until it's caught up, then cuts over in seconds: stop the old cluster, promote the new one, switch the pooler entries. Roles, passwords and the database name don't change. **The URL's host does:** `db1.<domain>/app` becomes `db2.<domain>/app`, and the command prints the new URLs. Then the old cluster is dropped.
- The node runs its database's backups, verify and drills on its own timers, renews its own certificate, and keeps serving if the master is down (6.7).

**Deferred to Phase 10:** what happens to the old URL during a move (a forwarding window on the old node, a clean cut, or something in front of the nodes). It depends on choices that belong to that phase, such as load balancers.

### 7.7 Disk

`min_wal_size` and `max_wal_size` come from each cluster's share of the data disk, in proportion to its `memory`. The archive spool's `archive-push-queue-max` becomes 4 GiB in total, split per stanza (floor 512 MiB). Space checks for restores and forks look at the filesystem, not the cluster. A per-cluster disk quota isn't possible without a filesystem per cluster, so a cluster that fills the disk still affects its neighbours. Doctor warns on growth, and the README says so.

## 8. Routing, ports and TLS

### 8.1 Two PgBouncers, both routing by database name

Database names are unique in a setup, so a PgBouncer can route by name to the right cluster. **Decided: two PgBouncers from one template:**

| | Transaction pooler (today's) | Session pooler (new) |
| --- | --- | --- |
| Port | 6432, behind nginx ALPN on 443 | **5432**, on `wg0` and to the allowlist when `expose_postgres` is set |
| `pool_mode` | `transaction` | `session` |
| Who | `<db>_app`, `<db>_ro` | Everyone, including `<db>_migrate` and `<db>_owner` |
| Auth function | `vaire.pgbouncer_get_auth`, which refuses members of `vaire_direct_only` (as today) | `vaire.pgbouncer_get_auth_session`, which refuses only `postgres` and the `vaire_*` service roles |
| Unit | `pgbouncer.service` (Debian's) | `vaire-pgbouncer-session.service`, `/etc/pgbouncer/session.ini` |
| TLS | nginx terminates | PgBouncer terminates (`client_tls_sslmode = require`, the node's certificate), the same requirement PostgreSQL has on 5432 today |
| Size per database | `pool_size` (default from `max_connections`) | `session_pool_size` (default 10) |

Both use the same `[databases]` shape:

```ini
[databases]
app           = host=/var/run/postgresql port=5433 auth_dbname=app__auth
app__auth     = host=/var/run/postgresql port=5433 dbname=postgres
billing       = host=/var/run/postgresql port=5434 auth_dbname=billing__auth
billing__auth = host=/var/run/postgresql port=5434 dbname=postgres
```

- The `*` wildcard goes away. A database that doesn't exist is refused by PgBouncer instead of by PostgreSQL.
- `auth_query` runs on the cluster the client is going to (through the per-entry `auth_dbname`), so `pgbouncer_auth`, both auth functions and `vaire_direct_only` exist in every cluster, created by `postgres-roles-v1` as today. **To verify on the PGDG PgBouncer** (Debian 13 and Ubuntu 26.04): per-database `auth_dbname` pointing at another `[databases]` entry. If that doesn't work, the fallback is the `vaire` schema and functions in each application database.
- `max_db_connections` per entry and pooler come from the cluster's `max_connections`: `pool_size + session_pool_size + 3 reserved ≤ max_connections`. Config validation refuses settings that don't fit. `max_client_conn` stays server-wide per pooler.
- Adding or removing a database is a `RELOAD` of both, never a restart.
- nginx on 443 doesn't change: ALPN `postgresql` → TLS terminated → transaction pooler → by database name.

### 8.2 Clusters listen on sockets only

With both poolers in front, **no cluster needs a TCP listener in Phase 3.** Each cluster gets `listen_addresses = ''` and a port number from 5433–5479, which only names its Unix socket (`/var/run/postgresql/.s.PGSQL.5433`). Temporary clusters (restores, forks, drills) use 5480–5499, as the scratch cluster on 5499 does today. Ports are recorded in config and immutable while the cluster exists.

What follows from it:

- ufw opens 5432 (the session pooler) on `wg0` and to the allowlist, as today, and nothing else. No port range, no per-database rules.
- The `migrate` URL that `db create` prints keeps port 5432: `postgres://app_migrate:…@db1.example.com:5432/app`.
- `vaire sql`, `psql`, `console`, `dump` and `tunnel --target postgres` still reach 5432, and `--db` picks the database, which the session pooler routes. The `pgPort = 5432` constant in `internal/cli/sql.go` stays. Only the address changes for a database on a worker (6.3).
- vaire's own work (steps, `node dump`, pgBackRest, restores) uses each cluster's socket on the node, never the poolers.
- Phase 6 and Phase 10 add a `wg0` listener on the cluster's port **for replication only**, because replication connections don't go through vaire's poolers.

What it costs: one more process, one more hop for migrations, and `pg_stat_activity.client_addr` shows the pooler instead of the client (PgBouncer's `application_name_add_host` puts it back in `application_name`). Session pooling is transparent for advisory locks, `SET`, temp tables, `LISTEN` and prepared statements, because a client keeps its server connection for the whole session.

### 8.3 Certificates

One certificate per node, for its one hostname, as today. The session pooler, nginx and every cluster's `ssl_cert_file` point at `/etc/vaire/tls/server.crt`. Clusters don't serve TLS while they listen only on sockets, but keep the setting for the replication listener later. `certs-renew` reloads both PgBouncers and nginx, then each cluster in turn. Creating, dropping, forking or moving a database never touches the certificate or DNS.

### 8.4 One hostname per server, routing by database name (changed in revision 4)

**Decided: never a hostname per database.** Each node has exactly one hostname, and the database name picks the database.

- **Hostnames:** `domain` in `cluster.toml` becomes the setup's base (`example.com`, or `data.example.com`), and each node is `<node>.<domain>`. The master is `db1.<domain>` by default, and `node add` suggests the next free `dbN`. Any DNS label works as a node name. That's one DNS record per node, made once when the node is set up.
- **Routing:** PostgreSQL sends the database name in its startup message, before authentication, and in a URL that name is the path: `postgres://app_app:…@db1.example.com:443/app`. That's the `db1.<domain>/app` shape, and it already works. nginx sends ALPN `postgresql` on 443 to the transaction pooler, both poolers route by the name (8.1), and drivers need nothing special.
- **Alternatives considered:**
  - SNI per database, which needs a hostname and a certificate SAN per database. That's the DNS sprawl this avoids.
  - A port per database: a firewall rule per database, ports some networks block, and a port that changes on a move.
  - Routing by user name: `app_app` already implies `app`, but drivers, poolers and tools key on the database name, and it buys nothing over it.
- **What keeps the URL:** restore and undo (in place), resize and restarts. A fork is a new database on the same node, so it gets a new path.
- **What changes it, accepted:** moving a database to another node changes the host (7.6). Failing over to another node in Phase 11 would too, unless the HA design routes around it (15.2).

## 9. Backups

### 9.1 One bucket, one repository, a stanza per cluster

pgBackRest supports several stanzas in one repository with one `pgbackrest.conf`:

```ini
[global]
repo1-path=/<prefix>/pgbackrest
repo1-cipher-pass=…            # one repository, one cipher: the recovery kit doesn't grow
repo1-retention-full-type=time
…
[app-3f9a1c]
pg1-path=/var/lib/postgresql/18/app
pg1-port=5433
pg1-socket-path=/var/run/postgresql
repo1-retention-full=28        # per-stanza retention
```

- **Stanza name: `<cluster>-<6 hex>`**, generated at create and immutable. A database dropped and created again under the same name gets a new stanza, because the old one has a different system identifier and still holds the dropped database's restore window.
- **Retention per cluster.** `retention_full` (the restore window in weeks) moves into the cluster's config, defaulting to the server-level value. A staging database can keep one week while production keeps four, the way a provider's backup window is an instance setting.
- **The recovery kit doesn't change**: bucket, keys, one cipher pass, the dump age identity. The list of stanzas is derived from the config, or from listing `<prefix>/pgbackrest/backup/` during DR.
- **`archive_mode = on` from creation.** Every cluster starts with `archive_command = '/bin/true'` (WAL discarded, not piled up) and switches to `pgbackrest archive-push` with a **reload** when backups are on. So `vaire backup setup` no longer restarts anything.
- **Dropped databases stay restorable for their window.** `db drop` stops and removes the cluster but keeps the stanza. A timer runs `pgbackrest stanza-delete --force` once the window has passed. `vaire backup list` shows dropped stanzas, and restoring one recreates the cluster (10.1).

### 9.2 Timers per cluster

`timers-v1` renders template units: `vaire-backup-full@<cluster>.timer`, `vaire-backup-diff@<cluster>`, `vaire-backup-incr@<cluster>`, `vaire-backup-verify@<cluster>`, `vaire-restore-drill@<cluster>`, and `vaire-db-dump@<db>` per database. Schedules come from the cluster's config, defaulting to the server's `[backup.database]`.

### 9.3 Staggering

N fulls at Sunday 01:00 would fight for the disk and the uplink. Each cluster's schedule gets a fixed offset derived from its stanza id, spread over the maintenance window, and the `node run` backup jobs take a server-level "heavy I/O" lock (shared lock, timeout 0, `skipped` when busy, as today) so that at most one full or verify runs at a time.

### 9.4 Verify and drill

- `verify` runs per stanza (`pgbackrest verify --stanza`), so a big repository is no longer one long download.
- The **drill** becomes per cluster and cheaper: fork the stanza into a temporary cluster (10.2), `pg_amcheck` the one database, compare with live, tear down. Drills are staggered over the month, one cluster at a time.
- `backup list [--db]` shows per database: the window, sets, last verify, last drill and dumps. Without `--db` it shows one row per database.

## 10. Restores, forks and followers: one primitive

Providers have one restore primitive: build a **new instance at a point in time**. Everything else is what you do with it. vaire adopts it as an internal operation, **materialise**: `pgbackrest restore --stanza=<stanza> --pg1-path=<data_root>/<name> --type=time --target=<ts> --target-action=pause` (or `pg_basebackup` from the live cluster when no point in time is given), registered as cluster `18/<name>` on a temporary port, with archiving off, `hot_standby = on`, small `shared_buffers`, and the live cluster's connection settings (hot standby needs them at least as high as the primary's). The result is a read-only copy, paused in recovery, with the target not yet committed.

Four commands use it:

| Command | Materialise into | Then |
| --- | --- | --- |
| `vaire restore <db>` | `<cluster>__restoring` | Inspect, then **cut over in place** (promote) or abort |
| `vaire db fork <src> --name <new> [--at]` | `<new>` | Promote right away, rename, new stanza: a new database |
| `vaire restore drill` | `<cluster>__drill` | `pg_amcheck`, compare, drop |
| `vaire db follow <src> --name <new>` (Phase 6) | `<new>` | Keep streaming from the source instead of pausing (10.4) |

### 10.1 `vaire restore <db>`

The command line, the confirm tokens and the "ready, then promote" safety model stay. Underneath:

1. **Preflight:** target inside this stanza's window, free space for **one** copy of the cluster (today it's the scratch copy plus the reloaded copy), and no open restore of the cluster.
2. **Materialise** into `<cluster>__restoring` on a port from 5480–5499.
3. **Ready:** `vaire sql --db <db>__restoring` reaches the paused copy through the session pooler (a temporary `[databases]` entry), and `restore status` compares row estimates with the live cluster. The window could later allow "move the target forward" before promoting, at no extra cost.
4. **Promote** (a few seconds): stop the live cluster (fast), stop the copy, rename the data directories (live → `<cluster>__pre_restore_<ts>`, copy → live path, same filesystem, atomic), and start `18/<cluster>` with its own configuration and cgroup limits. It reaches the target, vaire calls `pg_promote()`, and the cluster opens on a new timeline with archiving on to the same stanza. Then a full backup job starts. Role names and passwords are unchanged, so clients reconnect as before. This step is where vaire goes further than providers: they hand you a new instance and a new connection string. On a VPS, keeping the URL is worth the swap.
5. **Undo:** the same swap with the leftover. The old cluster resumes on its old timeline, and vaire then starts a full backup and records the timeline switch in `records.json` (6.1), so later restores pass `--target-timeline` explicitly instead of following the abandoned branch (`latest` would pick it). **This is the subtlest part of the RFC.** It has to be exercised in the manual test.
6. **Abort and cleanup:** drop the copy or the leftover cluster (`pg_dropcluster`).
7. **The provider's way, too:** `vaire db fork <db> --name <db>_restored --at <ts>` gives a separate database at that point, without touching the live one. The guide tells agents to prefer it when only some rows are needed.

`restore --dump <id>` keeps its role, for points older than the window: initdb a temporary cluster, create the roles, `pg_restore` the dump into it, then the same swap.

Compared with today: no `--db-include`, no dump and reload, no index rebuilds, no extension-ownership warning, about half the free space, and a restored database that is byte for byte what was there (bloat included).

### 10.2 `vaire db fork <src> [--name <new>] [--at <time>]`

Materialise into `<new>` from the source's stanza (`--at`), or with `pg_basebackup` of the live source (no `--at`, which also works without backups configured). Then promote it at once, and:

- `ALTER DATABASE <src> RENAME TO <new>`, and `ALTER ROLE <src>_owner|_migrate|_app|_ro RENAME TO <new>_…`. Ownership and default privileges follow the role OIDs. `ALTER ROLE <new>_migrate SET role = '<new>_owner'` is rewritten, and new passwords are set as verifiers and printed once.
- The fork gets its own stanza (`<new>-<id>`), switches archiving on, and takes its first full backup. Its system identifier equals the source's, which pgBackRest should accept across stanzas because they are separate paths. To verify.
- Size, limits and port come from the budget and the defaults, as with `db create`. It lands on the source's node unless `--node` says otherwise (Phase 10).

### 10.3 What happens to the Phase 3 code

| Goes | Stays (adapted) |
| --- | --- |
| `internal/backup/swap.go` (database rename swap) | `driver.go`, `pgbackrest.go` (gain `--stanza`, `--pg1-port`), `progress.go`, `s3.go`, `dumps.go`, `space.go`, `metrics.go` (a `db` label) |
| `CopyDatabase` and the scratch → live `pg_dump` pipe; `createLoadTarget`, `finishLoad`, `loadFromScratch` in `internal/node/restore.go` | `scratch.go`, which becomes "materialise a temporary cluster" (restores, forks, drills, followers) |
| `--db-include`, `restore_load_errors`, the reserved database names `__restoring` / `__pre_restore_` (they become cluster names) | `LoadDump` (for `--dump`), `compareRows`, the job model, `state.json` for jobs and `records.json` for restores, drills and last results per database |
| The open finding about the `pg_stat_statements` owner warning | Long-retention dumps, `dump list/get`, the final dump on `db drop` |

### 10.4 Followers (Phase 6, new in revision 4)

**Decided: physical standby followers**, with `wal_level = replica`. A follower is a cluster of its own that streams from one source cluster:

- **`vaire db follow app --name app_follower [--node db2] [--size small]`** materialises from `app`'s stanza (or `pg_basebackup` of the live source when there are no backups), sets `primary_conninfo` to the source and keeps streaming with `hot_standby = on`. The connection uses the source's socket on the same node, or its `wg0` replication listener from another node (8.2). A physical replication slot on the source keeps WAL until the follower has it, capped by `max_slot_wal_keep_size` so that a dead follower can't fill the source's disk.
- **Name and routing:** a physical copy has the source's databases and roles, so inside it `app` is still called `app`. The follower's node adds a pooler alias, `app_follower = host=/var/run/postgresql port=5436 dbname=app`, and clients use `db1.<domain>/app_follower` (or `db2.<domain>/app_follower`) with `app_ro`.
- **Read-only, same credentials.** Roles, passwords and grants replicate from the source, so there's nothing to hand out. `app_app` connects too, and writes fail as they do on any standby.
- **Its own size and limits**, with the hot-standby floors (connection settings at least the source's), which vaire copies, as for the paused copy in 10.1.
- **No stanza of its own** while following. The source's backups cover the data.
- **`vaire db unfollow app_follower [--name <new>]`** promotes it into an independent database: the renames of a fork (10.2), a new stanza and a first full. It isn't failover: the source keeps running.
- **Visibility:** `db list` and `status` show the source, the lag in bytes and seconds, and the state. `doctor` warns on lag and on a slot that's holding WAL.
- **Dropping a source** with followers is refused until they're unfollowed or dropped.
- Phase 10's replicas are the same mechanism on another node. Phase 11's HA standbys are managed by Patroni instead.

## 11. Desired state and the CLI

### 11.1 `cluster.toml`

```toml
schema = 1
name = "mbvlabs"                     # the setup (context name); its master is the first node
domain = "example.com"               # nodes are <node>.example.com; the master is db1 by default
# tls, transport, network, maintenance, metrics: unchanged

[server]
admin_user = "vaire"
swap_mb = 1024
data_root = "/var/lib/postgresql/18"  # was data_directory; each cluster is <data_root>/<cluster>
memory_reserve = ""                   # empty = max(512MiB, 25% of RAM)

[postgres]                            # setup-wide defaults for every cluster
version = 18
profile = "oltp"
client_connections = 1000             # transaction pooler max_client_conn
session_client_connections = 100      # session pooler max_client_conn
locale_provider = "builtin"
locale = "C.UTF-8"
default_size = "medium"               # db create without --size (7.4)

[backup]                              # bucket and defaults; optional (6.5)
provider = "r2"
# …
[backup.database]                     # defaults for each cluster's schedules and window
full_schedule = "Sun *-*-* 01:00:00"
retention_full = 4

[master_backup]                       # 6.5; the bucket and prefix come from [backup]
retention_days = 30
retention_weeks = 52
local_keep = 10                       # bundles kept on the master itself

[[clusters]]                          # replaces [[databases]]
name = "app"                          # database, cluster and pooler name
node = "db1"                          # placement; Phase 3 accepts only the master
port = 5433                           # socket name, assigned at create, immutable
stanza = "app-3f9a1c"                 # assigned at create, immutable
size = "large"                        # nano | small | medium | large | xlarge | custom (7.4)
memory = ""                           # empty = from size; tuning and MemoryHigh
memory_max = ""                       # opt-in hard cap
cpu_weight = 0                        # 0 = from size
cpu_quota = ""                        # opt-in, e.g. "150%"
io_weight = 100
profile = "oltp"                      # optional override
statement_timeout = "60s"
extensions = ["pg_stat_statements"]
pool_size = 0                         # 0 = from max_connections
session_pool_size = 10
[clusters.settings]                   # per-cluster [postgres.settings]
work_mem = "32MB"
[clusters.backup]                     # per-cluster window, schedules and dumps
retention_full = 1
dump_schedule = ""

[[clusters]]                          # a follower (Phase 6)
name = "app_follower"
node = "db1"
port = 5436
follows = "app"                       # no stanza while following
size = "small"
```

Nodes (Phase 10) live in `nodes.toml` beside it, written only by `node add/remove`, because they carry WireGuard keys and addresses rather than intent.

`config set` addresses clusters by name: `clusters.app.size`, `clusters.app.memory`, `clusters.app.settings.work_mem`, `clusters.app.backup.retention_full`. Immutable keys gain `clusters.*.port`, `clusters.*.stanza`, `clusters.*.name` and `clusters.*.follows` (only `db unfollow` clears it). Changing a cgroup key is a `systemctl daemon-reload` plus `set-property`, with no restart.

**Decided: one file, `[[clusters]]` inside `cluster.toml`.**

### 11.2 Steps

The step engine gains **per-cluster step instances**. A step such as `postgres-config-v1` runs once per cluster, journaled as `postgres-config-v1@app` with its own input hash, so changing `app` re-runs nothing for `billing`.

| Step | Change |
| --- | --- |
| `kernel-tuning-v1` | `vm.nr_hugepages` is the sum over clusters |
| `postgres-packages-v1` | Unchanged; `createcluster.d` still sets `create_main_cluster = false` |
| `postgres-cluster-v1@<cluster>` | `pg_createcluster 18 <cluster> -p <port> --datadir <data_root>/<cluster>`; the systemd drop-in with the cgroup limits and `OOMScoreAdjust` |
| `tls-v1` | One SAN, the node's hostname; certs-renew reloads both poolers, nginx and every cluster |
| `firewall-v1` | 5432 (the session pooler) on `wg0` and the allowlist, as today; nothing per cluster. On the master, the `wg0 → wg0` route rule from Phase 10 |
| `postgres-config-v1@<cluster>` | `/etc/postgresql/18/<cluster>/conf.d`, tuned from the cluster's `memory`, `listen_addresses = ''`; restart classification per cluster |
| `postgres-roles-v1@<cluster>` | Service roles and both auth functions in each cluster; one database's roles |
| `pgbouncer-v1` | Both poolers' `[databases]` entries per database (8.1); the session pooler's unit; reload |
| `pgbackrest-v1` | One config with a section per stanza; `stanza-create` and the first full per cluster |
| `timers-v1` | Template units per cluster (9.2) |

Constants that become functions of the cluster name: `PGCluster`, `PGConfDir`, `PGConfD`, `PGServiceName`, `PGServiceDropInDir`, the log path in `internal/node/logs.go`, `DefaultDataDirectory`, and `ScratchPort`, which becomes the temporary range.

**Locks:** the global `/run/lock/vaire.lock` stays for server-level steps. Each cluster gets `/run/lock/vaire-<cluster>.lock`, so an `apply` that restarts `billing` doesn't wait for a promote on `app`.

**Confirm tokens:** `<cluster>/restart` names one cluster, which is the database's name. `apply` takes `--confirm` more than once (`--confirm app/restart --confirm billing/restart`). Server-wide restarts (`pg update`) use `<server>/restart`, which restarts the clusters one at a time.

### 11.3 Every command, and what happens to it

| Command | Change |
| --- | --- |
| `version`, `commands`, `guide agents`, `skill install` | Guide text only (the model, the tokens, fork before restore for partial recovery, URLs by node) |
| `login`, `context list\|show\|use\|remove` | Contexts point at the master by hostname; `show` lists nodes and databases |
| `server init` (was `cluster init`) | Sets up the master (`db1.<domain>` by default) with no databases, and creates the recovery identity. `--data-directory` becomes `--data-root`. The profile and connection questions become setup defaults, plus `default_size`. The bucket stays optional; without one, it ends with the `no_offsite_backup` warning |
| `server init --restore [--bundle <ts> \| --bundle-file <f>]` (Phase 5) | Rebuilds the master from a bundle (6.6); clusters come back `awaiting_recovery` |
| `server backup now\|list\|verify\|export` (Phase 5) | New: the master backup (6.5). `verify` decrypts on the operator's machine; `export` saves the latest bundle there |
| `server takeover --node` (Phase 10) | New: a worker becomes the master from the latest bundle (6.7) |
| `node add\|list\|remove` (Phase 10) | New: workers, driven from the master, each with one hostname; `--dedicated <db>` (7.6) |
| `plan`, `apply` | Per-cluster step instances in the plan; repeatable `--confirm <cluster>/restart`; `--db` to limit to one cluster; the `no_offsite_backup` warning |
| `config show\|get\|set\|edit\|validate` | `clusters.<name>.*` keys; immutable `port`, `stanza`, `name`, `follows` |
| `status`, `doctor`, `top`, `queries` (Phase 4) | Per database (`--db`), with a server summary; doctor reads cgroup pressure, offsite-backup state and (from Phase 6) follower lag |
| `logs` | `postgresql@18-<cluster>` units, `--db`; the session pooler's unit |
| `sql`, `psql`, `console` | 5432 is now the session pooler, and `--db` routes. For a database on a worker, the CLI looks up its node and connects to that node's WireGuard address through the master (6.3). `--db <db>__restoring` reaches a restore copy |
| `tunnel` | `--db` picks the node (6.3); `--target postgres` is that node's session pooler |
| `ssh`, `wg export`, `upgrade`, `node upgrade` | Unchanged; `ssh --node` reaches a worker through the master (Phase 10) |
| `secrets export` | Document `cluster` → `server`; `stanza` is dropped from it |
| `db list` | Adds node, URL host, size, limits, stanza, follower source and lag, and state (running, restoring, following, awaiting_recovery) |
| `db create` | Creates the cluster: port, size, limits, `pg_createcluster`, config, roles, pooler entries, stanza and first full (with backups), timers. `--size`, `--memory`, `--profile`, `--node` (Phase 10). Steps down to a size that fits, and prints oversubscription warnings (7.5). URLs are `<node>.<domain>/<db>` |
| `db resize` | New: change size or explicit limits; live where possible, `<cluster>/restart` or `--defer-restart` for the rest (7.4) |
| `db recover` (Phase 5) | New: materialise an `awaiting_recovery` cluster from its stanza after a rebuild (6.6) |
| `db move` (Phase 10) | New: move a database's cluster to another node; prints the new URLs (7.6); what happens to the old URL is decided in Phase 10 |
| `db follow` (Phase 6) | New: a read-only follower streaming from a source, on this node or another (10.4) |
| `db unfollow` (Phase 6) | New: promote a follower into an independent database with its own stanza (10.4) |
| `db drop` | Final dump, stop, `pg_dropcluster`, remove the pooler entries and the timers; keep the stanza for its window. Refused while followers exist |
| `db credentials` | URLs with the node's hostname |
| `db fork` | Materialise, promote, rename, own stanza (10.2) |
| `ext list\|add\|remove` (Phase 7) | Per database; a preload extension restarts only its cluster |
| `tune show\|explain\|apply` | Per cluster (`--db`), plus the server sheet: budget, sizes, allocation, warnings (7.5) |
| `backup setup` | Bucket, cipher and keys as today, using the identity `server init` made; then a stanza and first full per existing cluster, with a **reload** instead of a restart, and the first master bundle upload |
| `backup now\|verify` | `--db` (default: every cluster, one at a time) |
| `backup list` | Per database; dropped stanzas still in their window |
| `restore <db>` and `status\|promote\|undo\|abort\|cleanup` | Same commands and tokens; materialise and swap underneath (10.1) |
| `restore drill` | `--db`; per-cluster drills on the timer |
| `dump`, `dump list\|get` | Unchanged for the user; `node dump` uses the cluster's socket |
| `job list\|follow` | Jobs carry `db` |
| `vps backup\|restore` (Phase 5) | **Removed.** The master backup replaces the restic VPS backup, and `restic` goes from the dependencies |
| `maint *` (Phase 4) | Per cluster |
| `pg update` (Phase 8) | One package update, then a rolling restart per cluster |
| `node run <job>` | Template-unit jobs take `--db` |
| `node dump <db>` | Connects to the cluster's socket |
| `node rpc`, `version`, `selfcheck`, `ssh-revert`, `unlock-ssh` | Unchanged |

**Decided: database is the only noun; `cluster init` becomes `server init`; contexts name servers.**

## 12. Roles and credentials

- **Per cluster:** the `postgres` superuser (local peer only), `vaire_admin`, `vaire_monitor`, `pgbouncer_auth`, `vaire_direct_only`, the `vaire` schema with both auth functions, and the one database's four roles. `pg_hba.conf` and `pg_ident.conf` are the same template for each, and only allow the socket (plus replication from the mesh for a source with followers).
- **Service credentials are setup-wide:** the same `vaire_admin` and `vaire_monitor` passwords are set (as verifiers) in every cluster on every node. That keeps the secrets document unchanged when a database or a node is added, which is today's rule: `db create` never makes the handoff stale. Separate passwords per cluster would add nothing against an attacker: every cluster is reachable with the same operator key and sudo. Providers do separate them, but their instances are on separate VMs. `pgbouncer_auth` keeps using peer authentication.
- **Application passwords:** per database, generated by the operator, printed once, never stored, as today.

## 13. The existing server

The `mbvlabs` server is a test server. **Decided:** it's torn down and rebuilt with the new release. Nothing is moved, and no conversion code is written.

## 14. What we lose, what we gain, and the alternatives

### 14.1 The design as a whole: pros and cons

The design is one database per cluster by default, sized and limited, one hostname per server, on a master node that's the source of truth, with workers (including one-database nodes) driven from it, and a small state backup instead of a VPS backup. Judged for a single operator:

**Pros**

- **One place for everything.** One endpoint, one desired state, one revision number, one history (the bundles). The operator, and agents acting for them, never wonder which node knows the truth. No consensus protocol, no distributed config, no conflicts: workers converge on what the master says.
- **Cheap, current disaster recovery for the control plane.** The master's own state is kilobytes, pushed on every change, so state RPO is about zero and a rebuild is minutes of steps. It's smaller and more precise than a weekly restic backup of `/etc`, `/home` and `/root`, and it removes restic, its repository and its password.
- **A rebuild is invisible to the operator.** WireGuard key, host keys and certificate come back, the context points at a hostname, and nothing on the operator's machine changes.
- **DNS grows with servers, not databases.** One record per node, made once. Creating, forking or dropping a database never touches DNS or the certificate.
- **Sizes speak the user's language.** "Staging is `small`, production is `large`" is one word each, adjustable both ways, the way a provider's plan is. Defaults step down to what fits instead of failing.
- **The user stays in charge.** Oversubscription is a warning with numbers, never a block. vaire refuses only what PostgreSQL itself couldn't start.
- **Growth without rewrites.** When a database outgrows the server, it moves to its own node, driven from the same master. Its URL's host changes; its name, roles and passwords don't, and the rest of the server gets its share back.
- **The data plane outlives the control plane.** From Phase 10, databases on workers keep serving, backing up and renewing certificates while the master is down.
- **A short path to HA.** The bundle already is the complete control state, so `server takeover` (Phase 10) and a replicated store (Phase 11) are steps on the same road, not a redesign.

**Cons**

- **The master is a single point of failure.** In Phase 3 it's the only server, so losing it loses everything until the rebuild. From Phase 10, losing it freezes every change, every restore and every new database, and blocks the operator's normal access to workers. That's acceptable for one operator with good backups. It isn't HA, and the README should say so.
- **The master is also a worker.** A heavy database on the master competes with the control plane. The reserve protects vaire's own processes, but a busy master makes `apply` slower. Later a master could hold no databases, at the cost of another VPS.
- **Compromising the master compromises the fleet.** It holds the fleet key and the desired state for every node. With one operator that's the same trust as today's single server, but it concentrates value. HA (Phase 11) narrows it:
  - **Separate keys per master.** Each master has its own fleet key and WireGuard key, authorised separately on every worker, so one compromised master can be revoked without rekeying the rest.
  - **Behind a load balancer or a shared name.** Operators reach whichever master is healthy, so revoking one doesn't cut them off.
  - **What HA alone doesn't fix:** any master can write desired state that the others replicate. The fix for that is **operator-signed revisions**: workers, and other masters, apply only desired state signed by an operator's key, so a master relays and schedules changes but can't forge them. A revision is already one document, so it fits the model. It's worth doing with HA, not before.
- **Recovery needs the bucket and the recovery kit.** Lose both and the master can only be re-created, not rebuilt. That's what explicit storage is for, and why `doctor` warns until a bucket exists (6.5).
- **Split brain is possible during a rebuild** if the old master comes back. The epoch fences vaire's own writes, not pgBackRest's `archive-push`, so "power off the old VPS first" is a rule the user has to follow.
- **Sizes are soft.** `MemoryHigh` throttles, it doesn't partition. Disk space and the page cache stay shared, and I/O weights depend on the disk scheduler. A provider's VM is a harder boundary.
- **Overcommitting is one flag away.** A user who passes `--allow-overcommit` on a 2 GiB VPS will get a slow server, and support questions that start "vaire is slow".
- **More moving parts per server:** N clusters, N stanzas, two poolers, per-cluster timers, cgroups, plus the bundle job.
- **Moving a database changes its URL's host.** That's the price of one DNS record per server. How to soften it is decided with Phase 10's load-balancing choices (7.6).
- **Teams are deferred.** Concurrent operators, scoped access, auditing and access control aren't designed here, and a proper team setup isn't worth building now. The hub and the single desired-state file should extend to them (base revisions already reject stale writes), but that's an assumption, not a design.

### 14.2 Alternatives to the master node

1. **Every node equal, with a replicated store from the start** (etcd or Raft among nodes): no single point of failure, but it needs three nodes for a quorum. That's wrong for the common case of one VPS. It's Phase 11's job.
2. **The bucket as the source of truth** (nodes stateless, desired state in object storage with conditional writes): no master, but every command depends on the bucket's availability and latency, and locking over S3 is fragile. It also makes the bucket mandatory.
3. **The operator's machine as the source of truth** (like local Terraform state): simple for one person, but a lost laptop loses the state, agents in CI need a copy, and timers on servers can't consult it.
4. **Keep the restic VPS backup:** it catches files vaire doesn't know about, but restores far more than a rebuild needs, misses changes between weekly runs, and adds a tool and a password. If users need extra paths later, they can be added to the bundle explicitly.

### 14.3 Lose

- **Density.** About 50–100 MiB fixed per idle cluster, plus its buffers. A 1 GiB VPS stops being a sensible home for several separate databases.
- **One-instance simplicity.** A memory budget, cgroup limits, a second pooler, per-cluster step instances, N stanzas, N sets of timers, staggering.
- **Server-wide changes cost N operations:** a certificate reload, a minor update or a shared setting touches every cluster.
- **HA gets heavier** (N Patroni scopes, section 16).
- **Stable URLs across nodes.** A database that moves gets a new host.
- **Not lost:** cross-database queries, which PostgreSQL doesn't have anyway (`postgres_fdw` and `dblink` still work, over the socket). Roles shared across databases, which vaire doesn't use.
- **Isolation is partial:** disk space, and disk I/O where the scheduler ignores weights, are still shared. That's the difference between a cgroup and a provider's VM.

### 14.4 Gain

- **Native PITR per database:** a physical restore and WAL replay, about 1× the space, no logical edge cases, with an inspectable paused copy before promote.
- **Its own config, tuning, `shared_preload_libraries`, restarts and retention per database.** An extension that needs preloading restarts one database.
- **A failure and resource boundary:** a crash, a full WAL, a stuck archiver, a bad setting or a memory blow-up stays in one database.
- **Upgrades per database:** a minor restart can roll one at a time, and a future major upgrade can move one database at a time, with 18 and 19 side by side.
- **Real followers and replicas:** physical streaming per database (10.4).
- **Portability:** a cluster can later move to its own server by restoring its stanza there. Fork, drill, followers and DR all use the same native path.
- **The provider model, on the user's servers:** an instance per database, restores as new instances, pools each.
- **Less and simpler restore code.** The rename swap, the dump/reload pipe and the reserved-name rules go away.
- **No PostgreSQL TCP listener** in Phase 3. Only the poolers face the network.

### 14.5 Alternatives to a cluster per database

1. **Keep the shared cluster, harden the logical restore.** Parallel directory-format `pg_dump -j` and `pg_restore -j`, better space checks, and fixing the warnings. That's cheaper now and keeps density, and it is DigitalOcean's surface. But DigitalOcean doesn't restore one database either. PostgreSQL has no per-database WAL, so the restore stays a logical copy (slow on large databases, about 2× the space), and preload, restarts, retention and failures stay shared.
2. **Hybrid with two restore paths:** shared by default, with per-database restore from the shared cluster kept alongside native restore for dedicated clusters. That means two restore paths, two backup layouts and two sets of tests forever: the most code, and the worst of both. A shared cluster that restores as a whole (5.1) avoids that, and was declined for simplicity.
3. **One server per database** (option B in section 5). Kept as a placement choice in Phase 10, the equivalent of a provider's dedicated instance.

**Recommendation:** Option C, now, with cgroup limits, sizes, one hostname per server, on a master node with a bundle backup. The parts I'm least sure of, and that the manual test must settle: the per-database `auth_dbname` routing in PgBouncer, the timeline handling on `restore undo`, the real per-cluster memory overhead, whether `IOWeight` is enforced on the VPS disk, and a fork's shared system identifier across stanzas.

## 15. Roadmap

### 15.1 Phase 3 is re-scoped, not closed

**Decided: now.** Phase 3 becomes **"Databases as clusters, backups and native restore"**, and its current restore checks are skipped. The order of work, as stacked branches:

1. **Config and naming:** `[[clusters]]`, `data_root`, sizes, budget and limit fields, `server`, `hostname` and `role = "master"` in contexts and `node.toml`, `domain` as the base with the master at `db1.<domain>`, `records.json` split out of `state.json`, the recovery identity created at `server init`, `server init`. Validation, `config set` keys.
2. **Per-cluster steps:** step instances in the engine and journal, per-cluster locks, `postgres-cluster/config/roles` per cluster, socket-only clusters and port allocation, the memory budget in `internal/tune`, `nr_hugepages` as a sum, the cgroup drop-ins.
3. **Routing:** both poolers with `[databases]` per database and `auth_dbname`, the session pooler's unit and auth function, the firewall change, the node's one hostname and certificate, URLs as `<node>.<domain>/<db>`.
4. **`db create/drop/list/resize` as clusters**, with size defaults, and the oversubscription warnings.
5. **Backups per stanza:** multi-stanza `pgbackrest.conf`, `archive_mode = on` from creation, reload-only `backup setup`, template timers, staggering, the heavy-I/O lock, per-stanza verify, dropped-stanza expiry.
6. **Materialise and native restore:** the temporary cluster, status, promote with the data-directory swap, undo with timeline recording, abort, cleanup, `--dump`.
7. **Native fork and per-cluster drill.**
8. **Docs:** MASTER (decisions table, architecture, files on a node, the backups section, roadmap, risks, a short "how this compares to managed providers"), IMPLEMENTATION (config, steps, tuning, the backups and restores chapter, roles and PgBouncer, timers), README (density and isolation limits, URLs by node, and that until Phase 5 a lost server is recovered by re-creating it and restoring each stanza by hand), MANUAL-TESTING. `PHASE-3-PLAN.md` is superseded.

Each branch ends with `just lint` and a build for all targets, as before.

**New exit test (manual, real VPS, R2):** rebuild `mbvlabs`. Create `app` and `other` (two clusters), check the memory split and the cgroup limits, and that `other` keeps serving while `app` restarts. Run a memory-heavy query in `app` and see it throttled while `other` stays responsive. Connect a migration tool through `db1.<domain>:5432/app` and an application through `db1.<domain>:443/app`. Back up, write rows to `app`, `restore app --time` and inspect the paused copy, then promote while `other` keeps writing. Undo and cleanup. Fork `app` at a time and without one. A drill per cluster. Drop a database and see its stanza stay listed. Add a preload setting to one cluster and check that only it restarts. Create a third database that overcommits the server and see the warning, then resize one down. A second `apply` changes nothing.

### 15.2 Later phases

| Phase | Effect |
| --- | --- |
| 4 Observability & maintenance | `postgres_exporter` with one target per cluster (multi-target), `pgbackrest_exporter` over every stanza, cgroup pressure (PSI) per cluster, `status/top/queries/doctor` per database, doctor checks for the memory budget, `CommitLimit` and offsite backups, `maint` per cluster |
| 5 Master backup & DR | **Decided: as planned, re-scoped from restic to the bundle**, and moved ahead of followers and extensions on 2026-10-14. The bundle and its triggers, retention and local copies (6.5), `server backup now\|list\|verify\|export`, `server init --restore` with the epoch fence (6.6), `db recover`. restic and `vps backup\|restore` are dropped |
| 6 Followers | **Decided:** physical standby followers; `wal_level` drops from `logical` to `replica`. `db follow` and `db unfollow` (10.4), the `wg0` replication listener, slots with `max_slot_wal_keep_size`, lag in `status` and `doctor`. Logical replication stays possible later as an opt-in |
| 7 Extensions | Per database; preload and restart affect one cluster. The package install stays server-wide |
| 8 Upgrades | `pg update` restarts the clusters one at a time. Major upgrades (backlog) become feasible per database |
| 9 Operators & agents | Scopes can be per database (an agent that can read `app` only) |
| 10 Multi-node & replicas | Workers driven from the master, each with one hostname; hub routing, forwarding for `sql`/`psql`/`console`/`tunnel` and the fleet key (6.2, 6.3); `node` placement, dedicated nodes and `db move` and what happens to the old URL (7.6); `server takeover` (6.7); followers on other nodes |
| 11 HA | The master stops being a single point: desired state and records in a replicated store, or a standby master following the bundles; separate keys per master behind a load balancer or a shared name, and operator-signed revisions (14.1). Patroni per cluster and a shared etcd. With one hostname per node, failover would move a database's host, so clients use libpq multi-host URLs (`postgres://db1.<domain>,db2.<domain>/app?target_session_attrs=read-write`), or every node in the HA group routes `app` to the current primary through its poolers. That's decided when the phase starts. HA might be offered only for clusters on dedicated nodes |

**Phase 5 exit test (manual):** rebuild `mbvlabs` from scratch, make changes, destroy the VPS, run `server init --restore` on a new one, then `db recover --all`. The operator's context works without edits, records (timeline switches, dropped stanzas) are intact, the old master's jobs stop on the epoch, and the measured RTO goes into MASTER. Then the same without a bucket: `server backup export`, destroy, `server init --restore --bundle-file`.

## 16. Risks and things to verify on a VPS

- **PgBouncer per-database `auth_dbname`** pointing at a `[databases]` alias on another socket, on the PGDG build for Debian 13 and Ubuntu 26.04. Fallback: the auth functions in each application database.
- **Two PgBouncers on one server:** the second unit, its own pidfile, socket directory and admin console; `client_tls_*` on 5432 with the same certificate.
- **Timeline after `restore undo`.** pgBackRest's default `--target-timeline=latest` would follow the abandoned branch. vaire has to record the switch and pass the timeline explicitly. Test a PITR after an undo.
- **Hot-standby parameter floors** for the paused copy and for followers: `max_connections`, `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions` and `max_locks_per_transaction` have to be at least the source cluster's values. vaire copies them.
- **A fork's system identifier** equals its source's. That should be fine in a separate stanza; confirm `stanza-create` and `check` accept it.
- **Memory overhead per idle cluster:** measure it, and set the 256 MiB floor and the README's density numbers from the result.
- **cgroups:** that `MemoryHigh` throttles the cluster before the global OOM killer acts, that shared memory is charged to the cluster's cgroup, and whether `IOWeight` has any effect on the VPS disk scheduler.
- **Shared disk space:** a cluster can still fill the disk for everyone. State it plainly in the README.
- **Forwarding through the master** (Phase 10): `ip_forward` and the ufw route rule limited to `wg0`, return routes on workers, and the throughput of large transfers (`dump get`, `psql \copy`) through the hub.
- **Master backup:** that the bundle restores to an identical master (keys, records, certificate), that the dirty-flag job coalesces bursts, and that a stale epoch stops an old master's jobs.
- **Split brain on rebuild:** an old master that comes back still runs `archive-push`. The rebuild has to say "power it off first", and `db recover` should check the stanza's latest WAL against the bundle's records before opening a cluster.
- **Oversubscription in practice:** the size table and the README guidance are guesses until the per-cluster overhead is measured.
- **HA complexity** (Phase 11): N Patroni scopes per node, and URLs that name nodes. Re-examine when that phase starts. Dedicated nodes for HA clusters may be the simpler answer.

## Sources

- DigitalOcean: [forking a PostgreSQL cluster](https://docs.digitalocean.com/products/databases/postgresql/how-to/fork-clusters/), [PostgreSQL limits](https://docs.digitalocean.com/products/databases/postgresql/details/limits/), [connection pools](https://docs.digitalocean.com/products/databases/how-to/postgresql/manage-connection-pools)
- Heroku: [fork](https://devcenter.heroku.com/articles/heroku-postgres-fork), [rollback](https://devcenter.heroku.com/articles/heroku-postgres-rollback), [plans](https://devcenter.heroku.com/articles/heroku-postgres-plans)
- CloudNativePG: [maintainers on multiple databases per cluster](https://github.com/cloudnative-pg/cloudnative-pg/discussions/497), [documentation](https://cloudnative-pg.io/docs/1.30/)
