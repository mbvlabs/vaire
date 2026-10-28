# How vaire's WireGuard setup works

> An illustrated explanation of the implementation, not a proposal to change it.
> Examined 2026-10-26 at source commit `a7075d8d4859`.
> Addresses below illustrate the defaults, not an inventory of the live VPS.

## The short answer: we hide the configuration workflow, not the credentials

**Users do not need to create, find, import or maintain a WireGuard `.conf` file for normal vaire use.** vaire generates the identity, stores it in a managed context, and configures its embedded WireGuard implementation automatically.

However, **the operator's WireGuard private key still exists on disk**, alongside an SSH private key and other credentials. Today those local files have restrictive permissions, but vaire does not encrypt them at rest. The portable transfer bundle is encrypted; the working context created from it is not.

```text
Typical manual WireGuard workflow

  generate keys -> edit client.conf -> store it safely
                -> import into a VPN app / run wg-quick
                -> connect another application through the VPN

vaire's normal workflow

  server init or login -> vaire-managed context
                      -> run a vaire command
                      -> private connection starts automatically

  No operator-managed .conf file, VPN app, root, or OS VPN interface.
  Still a private key to protect, now managed inside vaire's files.
```

## What WireGuard supplies, and what vaire adds

WireGuard creates an encrypted IP connection between peers. Each peer has a private key and knows the other peer's public key. It does not require a `.conf` file specifically: that is a convenient input format for tools such as `wg-quick`, not part of the network protocol.

vaire adds provisioning, credential storage, transport lifecycle, SSH and database access:

```text
                       vaire's responsibilities
  +------------------------------------------------------------+
  | Generate identities | Save/import context | Start transport |
  | Pin SSH host keys   | Execute node work   | Connect to DBs  |
  | Bootstrap firewall  | Export encrypted access handoff       |
  +-----------------------------+------------------------------+
                                |
                 configures and uses WireGuard
                                |
  +-----------------------------v------------------------------+
  | WireGuard: peer authentication + encrypted IP packets       |
  +------------------------------------------------------------+
```

There is no hosted vaire coordination service in this path. The operator contacts the VPS directly using the endpoint and keys in its context.

## The actual network: userspace on the laptop, wg0 on the server

The two ends deliberately work differently:

```text
OPERATOR MACHINE                               VPS / MASTER
normal user, Linux or macOS                     Linux

vaire ssh -------+                             +----------------------+
vaire plan ------|  local Unix socket           | kernel WireGuard     |
vaire sql -------+--> shared tunnel daemon      | interface: wg0       |
                       |                       | address: 10.42.0.1   |
                       | wireguard-go          |                      |
                       | + userspace IP stack  | sshd              :22|
                       | address: 10.42.255.1  | session pooler  :5432|
                       |                       | tx pooler       :6432|
                       +==== encrypted UDP ===>+                      |
                            public IP:51820    +----------------------+

No OS wg0 on the operator.                      Real OS wg0 here.
No operator route-table changes.                Managed by wg-quick.
```

The daemon is another invocation of the same binary, `vaire tunnel serve`; it is not a separate installation. It embeds `wireguard-go` and a userspace network stack. vaire's dialer asks that stack to open connections to private addresses.

**This is not a whole-machine VPN.** A browser or arbitrary application on the laptop cannot automatically reach `10.42.0.1` just because vaire is running. Only connections using vaire's dialer, or a local forward exposed by vaire, enter this tunnel. Ordinary Internet traffic is unaffected.

The default address allocation is:

| Purpose | Default |
| --- | --- |
| Overall private address space | `10.42.0.0/16` |
| Node addresses | `10.42.0.0/24`; master `10.42.0.1` |
| Operator addresses | `10.42.255.0/24`; first operator `10.42.255.1` |
| Public WireGuard endpoint | VPS public IP, UDP `51820` |

The client configures the master's node `/24` as `AllowedIPs`. The server associates the operator's public key with just its individual address, `10.42.255.1/32`. In WireGuard, `AllowedIPs` selects the peer for outgoing destinations and restricts source addresses accepted from that peer; it is not a database permission system.

The client initiates contact, so the server's operator peer has no fixed endpoint. WireGuard learns where to reply from authenticated packets. A 25-second persistent keepalive helps preserve NAT mappings. The embedded interface uses MTU 1380 to leave room for tunnel overhead.

Source: `internal/transport/wireguard.go`, `internal/cli/session.go` (`tunnelConfig`), `internal/steps/wireguard.go`.

## Where the usual client configuration went

The equivalent of a client `.conf` is assembled from several managed values:

```text
~/.config/vaire/contexts/<name>/

  wg.key -------------------------> operator private key
  context.toml -------------------> operator private address
                 +----------------> server public IP + UDP port
                 +----------------> server public key
                 +----------------> server private address
                                          |
                                          v
                             in-memory WireGuard config
                                          |
                                          v
                              wireguard-go device.IpcSet
```

`IpcSet` here is a Go method call that configures the embedded device. It does not mean vaire exports a client `.conf` or sends the private key to the server. The daemon reads `wg.key` itself; vaire does not put that key in the daemon's command-line arguments.

The server **does** use a conventional configuration file. vaire generates and maintains it on the user's behalf:

```text
VPS
  /etc/wireguard/vaire.key ------> server private key
  /etc/wireguard/vaire.pub ------> server public key
  /etc/vaire/operators.toml -----> operator public keys + addresses
                  |
                  v
  /etc/wireguard/wg0.conf
    [Interface]
    PrivateKey = <server private key>
    Address = 10.42.0.1/16
    ListenPort = 51820

    [Peer]
    PublicKey = <operator public key>
    AllowedIPs = 10.42.255.1/32
                  |
                  v
            wg-quick@wg0 -> kernel WireGuard interface
```

The server key and config are root-owned, mode `0600`, inside `/etc/wireguard` with mode `0700`. Updates to a running interface use `wg syncconf` rather than taking it down.

**Neither side needs the other's WireGuard private key.** Public keys are exchanged; private keys remain on their respective sides, with the backup/transfer copies described below.

Source: `internal/contexts/contexts.go`, `internal/transport/wireguard.go`, `internal/steps/wireguard.go`, `internal/steps/scripts/wireguard-{key,up}.sh`.

## First setup: how we reach a server before the private network exists

For remote `server init`, the initial trust path is public SSH using the operator's existing provider/root access:

```text
OPERATOR                                          FRESH VPS
  |
  | generate operator WireGuard + SSH keys
  | save private pending state for resumability
  |
  +---- public SSH using existing access ----------> install vaire
  |                                                 create admin user
  |---- operator PUBLIC keys ---------------------> register operator
  |                                                 generate SERVER WG key
  |                                                 start wg0; allow UDP
  |<--- server WG PUBLIC key + SSH host identity ---+
  |
  +==== WireGuard =================================> private network
  +---- new SSH connection INSIDE WireGuard -------> prove access works
  |
  +---- arm 2-minute rollback; close public SSH ---> firewall lockdown
  +---- another fresh private SSH connection ------> confirm lockdown
  |                                                 cancel rollback timer
  |
  +---- finish setup through WireGuard ------------> remaining node setup
  |
  +-> show secrets handoff; confirm it is stored
  +-> install local context; remove pending state
```

If lockdown is not confirmed, the server's timer restores the previous SSH/firewall snapshot. The point is to prove the new path before committing to it.

**An important implementation detail:** SSH-only-over-WireGuard is enforced by UFW rules, not by binding sshd only to the private address. sshd can have wildcard listeners while public TCP/22 remains blocked. This avoids relying on wg0 being ready before sshd starts.

For `server init --local`, setup runs on the VPS and the handoff is imported on the operator machine with `vaire login`. Public SSH remains open pending operator confirmation, with a 30-minute rollback window. The first remote session confirms access through WireGuard.

Source: `internal/cli/serverinit.go`, `internal/node/ssh.go`, `internal/steps/sshd.go`, `internal/steps/firewall.go`; bootstrap details in `IMPLEMENTATION.md`.

## Everyday use: one tunnel, multiple independent connections

The first command needing the server starts the context's daemon automatically. Later commands attach to it through a Unix socket, normally `tunnel.sock` in the context directory.

```text
Terminal A: vaire ssh ---- own SSH connection ----+
                                                 |
Terminal B: vaire plan --- own SSH connection ----+--> ONE WG device
                                                 |    ONE operator identity
Terminal C: vaire sql ---- DB connection ---------+    ONE shared UDP transport
                                                           |
                                                           v
                                                        server wg0
```

The daemon shares **TCP connectivity, not one SSH login**. Each command retains its own SSH client and performs its own version handshake with the node. A SQL command also opens that control session, but its SQL traffic connects directly to the session pooler through WireGuard; it is not carried through SSH port forwarding.

Why share the tunnel? WireGuard identifies a peer by its public key. Two independent devices using the same operator identity can cause the server to switch its reply endpoint between them, disrupting existing traffic. One shared device avoids that competition between commands using the same local context.

Commands keep a local attachment, called a lease, while running. The daemon stays alive while leases or data streams exist, then exits after **10 minutes unused** by default. `tunnel_persist` in the context or `VAIRE_TUNNEL_PERSIST` can override that duration.

The local socket is mode `0600`; the daemon also checks that callers have the same OS user ID and permits TCP destinations only within its configured node prefix. This protects against other local users, not other programs running as you.

Bootstrap still uses a temporary in-process tunnel. The explicit `VAIRE_TUNNEL=inprocess` fallback also exists and takes an exclusive per-context lock.

Source: `internal/cli/tunneld.go`, `internal/transport/shared.go`, `internal/transport/sharedserver.go`, `internal/cli/tunnellock.go`.

## What travels inside the tunnel

WireGuard provides network access. SSH and PostgreSQL still authenticate separately:

```text
Control operations
  vaire plan/apply/etc.
    -> local shared daemon -> WireGuard -> 10.42.0.1:22
    -> SSH key authentication + pinned server SSH host key
    -> sudo vaire node rpc -> requested work

Database operations
  vaire sql/psql/console
    -> local shared daemon -> WireGuard -> 10.42.0.1:5432
    -> PostgreSQL TLS + database authentication
    -> session PgBouncer -> database's Unix socket -> PostgreSQL

External GUI / migration tool
  tool -> 127.0.0.1:6544 -> running vaire tunnel forward
       -> shared daemon -> WireGuard -> 10.42.0.1:5432
       -> session PgBouncer -> database's Unix socket -> PostgreSQL
```

The forward above uses the session-pooler target. The default `vaire tunnel` target is instead the transaction pooler: local port `6543` to server port `6432`. `--target postgres` means the **session pooler**, not a directly exposed PostgreSQL cluster. Defaults advance to an available local port when needed.

With ACME TLS, vaire's database clients verify the server certificate against the node hostname even though the network connection uses its private IP. An external client using a local forward must likewise preserve the hostname for full certificate verification; a loopback address alone will not match it. The explicit self-signed mode encrypts without certificate verification and produces a warning.

Application traffic can separately use allowlisted public pooler access. It does not have to run through the operator's WireGuard connection. WireGuard peers always get firewall access to both poolers, but still need database credentials. Actual PostgreSQL clusters listen on Unix sockets only.

Source: `internal/cli/session.go`, `internal/cli/sql.go`, `internal/cli/tunnel.go`, `internal/cli/console.go`, `internal/steps/firewall.go`.

## What the user still has to keep safe

There are three different things worth separating:

| Thing | Purpose | Protection today |
| --- | --- | --- |
| Working operator context | Automatic everyday access | Directory `0700`, files `0600`; private keys and credentials are not encrypted by vaire at rest |
| Secrets handoff / transfer bundle | Save access credentials; import them on another machine | Plaintext handoff is sensitive; armored transfer bundle is age-encrypted with a generated passphrase |
| Master backup bundle | Restore the server's identity and durable state | Encrypted to the recovery identity; includes the server's WireGuard key and operator registry |

The working context normally looks like:

```text
~/.config/vaire/contexts/<name>/       directory: owner only
  context.toml       endpoint, public keys, addresses, node cache
  wg.key             operator WireGuard PRIVATE key
  id_ed25519         operator SSH PRIVATE key
  known_hosts        pinned server SSH host keys
  credentials.json   sudo/DB/service credentials and recovery kit
  tunnel.sock        local daemon connection, while running
  tunnel.log         daemon diagnostics
  tunnel*.lock*      coordination files
```

`$XDG_CONFIG_HOME` can relocate the configuration root. For long paths, the socket alone can live in a private runtime directory instead.

The import/export lifecycle is:

```text
                           encrypted portable copy
working context -----> secrets export -----> age transfer bundle
      ^                                           + passphrase
      |                                                |
      +------------------ login <----------------------+
                   decrypt, validate, write
                   owner-only working files
```

**`vaire login` is credential import, not a remote account login or new-device enrollment.** It recreates the identity contained in the bundle without registering a fresh peer on the server. Normal commands can then work without re-entering the bundle passphrase.

**The secrets-export master password is not a context-encryption password.** It gates the CLI's reveal/export flow; it does not encrypt `wg.key`, `id_ed25519` or `credentials.json`. A process running as the same OS user, or root, can read those files directly. The export gate can also use a node-verified TOTP code, but neither mechanism changes that local-file trust boundary.

OS keychain storage is listed as a later improvement in `IMPLEMENTATION.md`; it is not the storage mechanism used today.

Creating a new export gives the new bundle its own passphrase. It does **not** rotate the operator's WireGuard identity, revoke older bundles, or make their credentials stop working.

So the intended UX promise is: **“vaire manages the working files; you safeguard the access handoff and the machine that holds the context.”** It is not: “no secrets are stored locally” or “every command requires unlocking an encrypted vault.”

Source: `internal/secretsdoc/document.go`, `internal/secretsdoc/context.go`, `internal/contexts/contexts.go`, `internal/cli/login.go`, `internal/cli/secrets.go`, `internal/masterbackup/files.go`.

## Limits and places where the design documents get ahead of the code

- **One local context is not one globally coordinated identity.** Importing the same bundle on two machines, or under two context names, copies the same WireGuard identity. Their separate daemons can compete for the server's peer endpoint. The shared daemon solves concurrent commands within one context on one machine, not simultaneous use of copied identities everywhere.
- **Separate operator/device enrollment is future work.** The roadmap reserves operator add/revoke and scoped identities for a later phase. Importing a bundle is not a substitute for those features.
- **The implemented network is operator-to-master.** Worker routing through a master hub, fleet networking and Tailscale transport are roadmap designs, not a currently implemented multi-node mesh.
- **`vaire wg export` is currently a stub.** The command and flags appear in the CLI tree, but its handler is `notImplemented`. References describing system-config export as available are ahead of the code.
- **The old “no persistent daemon; tear down on command exit” description is outdated.** Normal context-based operation uses the shared local daemon above. This does not introduce a long-running vaire control daemon on the VPS: node work is still invoked through SSH and timers.

These observations describe this checkout; no transport or credential-storage changes are proposed or made here.

Source: `internal/cli/tree.go`, `internal/cli/tunneld.go`, `PLAN-shared-tunnel.md`, and the roadmap in `MASTER.md`. Recorded concurrency, idle reuse and timeout testing—with coverage limits—are in `MANUAL-TESTING.md`.

## The mental model to keep

```text
Encrypted handoff = portable access/recovery record to keep safe.
Local context     = installed, usable credentials managed by vaire.
Shared daemon     = automatic private networking for vaire connections.
Server wg0        = standard WireGuard, configured and maintained by vaire.
SSH / DB auth     = separate access checks inside that private network.
```

**vaire builds an automatic access experience on top of standard WireGuard. It removes manual VPN-file management, not the need to store and protect private keys.**
