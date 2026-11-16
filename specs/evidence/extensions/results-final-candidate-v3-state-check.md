# V3 lab state check — STOP before provisioning (2026-11-16)

Read-only inspection only. No VM start, boot, reset, disk replacement,
network/sudo change, install or guest write was performed.

## Identity checks

- Source manifest digest:
  `66ae70f96f88f0577dac3388b9da6f26add06a897ad047950e762ab8c27ff7ea`
  (687 files); `sha256sum -c` passed.
- V3 binary: `bin/vaire-extension-final-v3`
  `069516576918705d82b137994adf6dfe3588951a83a1ac75b8a2d3db99ca2c6f`
  `vaire dev (84f71bbaa180-ext-v3-66ae70f96f88, dev)`.

## Host state (read-only)

```text
./lab status                         VM stopped
tmux -L vps-lab ls                   no server running
fc-vpslab                            DOWN / NO-CARRIER
/dev/kvm                             user mbv read/write OK now
state/rootfs.ext4                    present, mtime 18:19
state/base.ext4                      present, mtime 18:19
```

## Guest disk state (read-only `debugfs`, not booted)

`state/rootfs.ext4` is an ext4 `volume name "vps-lab"` image. Inspecting
directory metadata only:

```text
/etc/vaire            absent
/var/lib/vaire        absent
/var/lib/postgresql   absent
/usr/local/bin        empty (only . and ..)
```

This is a **pristine disposable baseline**, not a vaire node and not an
operator handoff. There is no `cluster.toml`, no PostgreSQL cluster and no
installed vaire binary to test against. V1 fixtures are also gone.

## Verdict

**BLOCKED — stop for parent/user decision.** Running V3 requires booting this
baseline and provisioning a new vaire node plus operator handoff, or pointing
at a different guest that is already a vaire fixture. I did not start the VM,
replace the disk, run `server init`, or attempt interactive sudo. This is a
provisioning/ownership decision, not a functional FAIL.

No eregion repository/state, credentials, external bucket or production state
was accessed.
