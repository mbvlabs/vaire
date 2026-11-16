# EFIN1 + EFIN11 fixed-overlay live result

**This is an exact baseline+overlay build, not the final candidate tree.** It is
a clean detached checkout of
`84f71bbaa1804fb9b7d581ca8a109192dc6375a9` at
`/home/mbv/work/vaire-efin-fix` with only the parent's EFIN1 and EFIN11 files
overlaid. The shared worktree currently has in-progress EFIN3 changes and was
not used for this binary. Final-candidate reruns wait for the freeze notice.

## Identity

- Base revision: `84f71bbaa1804fb9b7d581ca8a109192dc6375a9`
- Build: `vaire dev (84f71bbaa180-efin1-efin11, dev)`
- Binary SHA-256:
  `da72cce56f8e969edf61ec695ce0c1c1c83c064009a240c68468cbc903c82f93`
- Overlay file SHA-256:
  - `internal/node/follower.go`
    `e105fe7d5d8ec4f941403a4a09a7d788f04f17bf6c28a82a838f80abfcacc837`
  - `internal/node/unfollowrename.go`
    `616febc23456787a9bb26fa1b91e8ceb3872d5ef00cc86350ccd851c5341553f`
  - `internal/node/follower_lifecycle_test.go`
    `c0ec1d18bc95a5b101bec31fc14c7380b3c40b42598dc2e85a74524bc6395e1d`
  - `internal/node/unfollowrename_test.go`
    `aabae1d14476b9f4a3079833803ebae0bdc52507fcc5edd8c53a2998bf90fcfb`
  - `internal/node/extensionactivation.go`
    `d1d4ca8f369335db11b148680cf8aed095b547196c7100eecd736be0868024d0`
  - `internal/node/extensions_activation_test.go`
    `85e826cd21321689b0730721c88b622ed8e356617920c2ca1b76322cef1028db`
- Guest install for systemd jobs (which hardcode `/usr/local/bin/vaire`):
  old binary saved as `/usr/local/bin/vaire.pre-efin`; fixed overlay installed
  as `/usr/local/bin/vaire`. The direct RPC client was
  `/tmp/vaire-extension-lab-efin`.
- Focused unit tests in the overlay checkout: PASS
  `TestCronJobTableUsesPSQLBooleanWireFormat`,
  `TestCronJobTableRefusesUnusableFacts`,
  `TestUnfollowRenameQuiescesFollowerOIDAndRestoresOriginalFlag`,
  `TestUnfollowRenameDrainBoundsReconnectingCronLauncher`,
  `TestUnfollowRenameLeavesTheSourceClusterAlone`.

## Live cron-bearing unfollow

Setup before the fixed run: source `extcron` (port 5436) had real pg_cron 1.6
and job `extsleeptest` (`* * * * *`, `SELECT pg_sleep(180)`, database
`extcron`). Its follower `extcronfol` (port 5437) had been promoted by the
pre-fix candidate but the unfollow failed at EFIN11, leaving a durable unfollow
record. The follower still ran a real `pg_cron launcher` backend, PID 51622,
connected to the unrenamed `extcron` database.

Fixed resume at 15:23:44Z (job `01M7JFA06EHQ8ZWGR063Z95PFM`) succeeded:

```json
{"ok":true,"data":{"job":"db-unfollow","state":"succeeded",
 "result":{"name":"extcronfol","port":5437,"source":"extcron",
 "stanza":"extcronfol-9caf6f","warnings":null}}}
```

PostgreSQL log proves the drain reached the cron launcher, not a guessed PID or
application name:

```
2026-11-16 15:23:46.531 UTC [51622] FATAL: terminating background worker
  "pg_cron launcher" due to administrator command
```

After settlement, on the independent `extcronfol` (port 5437):

```
cron.job:       jobid=1 jobname=extsleeptest active=f database=extcronfol
settings:       cron.database_name=extcronfol  cron.launch_active_jobs=off
```

The source `extcron` was unchanged:

```
cron.job:       jobid=1 jobname=extsleeptest active=t database=extcron
settings:       cron.database_name=extcron  cron.launch_active_jobs=on
```

This is live evidence for the EFIN11 boolean fix and the EFIN1 gate/drain on a
real copied cron job. It is not full X7: the drill path was not run (no bucket),
but the fork path was run on the same overlay.

## Live fork of the cron-bearing source

`db.fork source=extcron name=extcroncopy size=nano` succeeded at 15:38Z (job
`01M7JG3QY864C2EP82DTHB16SW`, port 5438). On the owned copy:

```
cron.job:       jobid=1 jobname=extsleeptest active=f database=extcroncopy
settings:       cron.database_name=extcroncopy  cron.launch_active_jobs=off
```

The source stayed `active=t database=extcron`, `cron.database_name=extcron`,
`cron.launch_active_jobs=on`. The copy's `cron.job_run_details` count stayed 17
across a 65-second observation, so no copied launch occurred. A live pg_cron
launcher connection existed on the copy, which is why a rename/activation that
does not drain can still fail; the fork's own drain path is in the baseline
`forkrename.go`.

