# Operator and device enrollment

**Status:** Planned for one VPS. RC2 ships the identity groundwork, not enrollment. Bootstrap ownership, rootless access management, and the admin-first flow over a temporary WireGuard peer are agreed.

## Outcome

An owner can grant someone Vaire operator access without sharing a private key or a recovery bundle. One operator can enroll multiple devices. The server can say which operator and device performed a management action, and the owner can revoke a lost device without interrupting the others.

An **operator** is a named account scoped to a workspace's installation. A **device** is a separately enrolled CLI installation with its own SSH and WireGuard keys. A **node** is a VPS that Vaire manages. Device and node labels are for display and selection, not authentication.

## Bootstrap and ownership

- On a fresh install, `vaire server init` makes the person running setup the initial **workspace owner** and enrolls the setup device as their first device. There is no separate signup or owner-claim step.
- Initial host provisioning keeps its existing privilege requirements. After bootstrap, the owner manages Vaire accounts and devices from the normal CLI over SSH and WireGuard. They do not need a root login, host shell, or manual `sudo` for enrollment, approval, or revocation. Vaire owner is a workspace permission, not a VPS root account.
- Setup still produces the existing recovery handoff. Normal account and device management does not require opening or sharing that handoff. Approval comes from an authenticated owner device; the joining device generates and keeps its own private keys.

Two workspace roles are proposed: **owner** (workspace admin, can manage users and devices) and **member** (default, can run supported management commands but cannot grant or revoke access). The bootstrap user is the first owner; an owner can explicitly add another owner. Neither role grants general sudo, host administration, or copies of shared credentials. The exact member command set is a design decision; existing approval requirements still apply. This milestone does not change PostgreSQL's `_ro`, `_app`, or `_migrate` roles, introduce per-person SQL credentials, or claim to attribute direct SQL activity. Multi-node access, service accounts, SSO, custom roles, and TUI work are out of scope. Both client database ports continue to go through PgBouncer; future tooling for deeper administration belongs in the admin TUI, not in a public PostgreSQL listener.

## Terminal flow

The owner starts enrollment and shares a single-use, short-lived setup code through a trusted channel. The recipient does not need an existing workspace, a server address, or a file to send back.

```console
owner@desktop$ vaire -w prod user add alice
Created user alice.

owner@desktop$ vaire -w prod device add alice work-laptop
Share this one-time setup code with Alice: vaire-invite:…

alice@work-laptop$ vaire login
Paste a transfer bundle or setup code: vaire-invite:…
Connected to prod / db1 as alice / work-laptop.

alice@work-laptop$ vaire -w prod whoami
Operator  alice
Device    work-laptop
Workspace prod
Node      db1
Identity  verified by db1
```

Run `vaire -w prod device add alice home-desktop` for Alice's second machine. Both devices can connect at the same time. The owner can distinguish the devices and revoke them separately:

```console
owner@desktop$ vaire -w prod device list
USER       DEVICE        ID          STATUS
owner      desktop       dev-074e91  active
alice      work-laptop   dev-14a6f2  active
alice      home-desktop  dev-be71c2  active

owner@desktop$ vaire -w prod device revoke alice work-laptop
Revoke work-laptop (alice) in prod? [y/N]: y
Revoked work-laptop. Alice's other devices are unchanged.
```

The setup code is scoped to one workspace, user, and device label. It uses a temporary WireGuard peer that can reach only the restricted enrollment path, not general management or the poolers. Whoever redeems it first binds that slot to keys generated on the joining machine. No long-lived private key is sent or issued by the owner. The temporary peer is removed on redemption, cancellation, or expiry. Login must authenticate the intended server and confirm management access with the device's permanent peer before reporting success. Normal management remains SSH over WireGuard.

One code cannot enroll two machines. This is not hardware binding: someone who deliberately copies the resulting on-disk private keys can impersonate that device. Do not offer an export or transfer flow for enrolled device credentials; issue a separate code for each machine.

## Command and API contract

| CLI | Node operation | Behavior |
| --- | --- | --- |
| `vaire user add <user> [--role owner\|member]` | `user.add` | Create a workspace user, defaulting to `member`. Use `--role owner` explicitly to grant workspace administration. Does not enroll a device. |
| `vaire user list` | `user.list` | Show users, roles, and status. |
| `vaire user revoke <user>` | `user.revoke` | Confirm and revoke all of the user's devices and outstanding setup codes; preserve history. |
| `vaire device add <user> <device>` | `device.add` | Issue one setup code for an existing user and a named device. Never replace an active device implicitly. |
| `vaire device list [--user <user>]` | `device.list` | Show device ID and status, including pending and revoked; never show codes or keys. |
| `vaire device revoke <user> <device>` | `device.revoke` | Confirm and revoke one device or cancel its pending setup. Bind confirmation to the resolved device ID. |
| `vaire login` | Restricted redemption, then `whoami` | Accept a pasted setup code or the existing recovery handoff. Never require the code in command-line arguments. |
| `vaire whoami [--json]` | `whoami` | Show the server-verified user, device, and node. A legacy shared login is explicitly labeled as such. |

`user` and `device` commands use normal workspace selection and are owner-only on the node. Setup codes are secrets: return a code only to the issuing command, and never persist the plaintext in run logs, activity history, or diagnostics. Each enrolled device gets a distinct WireGuard peer key and tunnel IP. Multiple terminals on that device use the existing shared tunnel; separate devices can be active simultaneously.

## Acceptance

- On a fresh single-node installation, the device that ran `vaire server init` can run `whoami` as the initial owner without a second signup. After setup, that owner can create another operator, enroll two devices, and revoke access without logging in as root, using manual `sudo`, or opening the recovery handoff.
- Each device generates its own keys. The code transfers no long-lived private keys, database passwords, approval factors, or recovery material. Expired or already redeemed codes cannot enroll a machine, including when two clients try the same code concurrently. An interrupted successful redemption may be resumed only with the original device keys.
- Issuance identifies the intended workspace, user, and device label. Non-owners cannot grant or revoke access, including by calling the server directly. Separately enrolled devices for the same user can operate simultaneously with distinct identities.
- `whoami --json` reports the verified operator, device, workspace, and responding node. If the node is unreachable, the command fails rather than claiming the local profile is verified. Activity records distinguish the two devices making managed changes; client-supplied labels cannot impersonate another actor. Revocation does not erase historical attribution.
- Revoking one device removes its management access, including any open management session or tunnel. The other device keeps working. Revoking the operator removes access for all that operator's devices. The last owner cannot accidentally remove their own remaining access. Revocation does not undo actions already accepted or revoke PostgreSQL credentials obtained separately.
- A supported single-node upgrade preserves working owner access. A legacy handoff imported on two machines still represents one shared identity; migration must not claim to distinguish those machines. Re-enroll each device to get distinct identities. Preserve bundle import for recovery/compatibility, not as the way to add operators.
- On a disposable VPS, verify owner upgrade, enrollment, attribution, revocation with an active tunnel, and the surviving device. No fleet or HA setup is required.

## Release and decisions

Land changes on `master` first and bring them to `1.x` by fast-forward where possible, or backport if the branches have diverged. RC2 is fresh-install only; RC1-to-RC2 upgrade is not supported. Validate workstation-driven node upgrades between supported signed builds before V1.

Review the redemption path and the non-owner command set before implementation. Technical design must show what authenticated identity the node can trust, how it enforces owner permissions without granting host administration, how open sessions are revoked, and how legacy owner access and recovery survive migration.
