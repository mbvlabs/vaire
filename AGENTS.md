# Working on eru

eru is a Go CLI that manages PostgreSQL 18 servers, one cluster per database. Start with `docs/MASTER.md` (design, decisions, status), then `docs/IMPLEMENTATION.md` (details). Live test results and open bugs are in `docs/MANUAL-TESTING.md`. For work in progress, read any `docs/HANDOFF-*.md` first.

## Rules

- **Commits:** only when the user asks, or when a handoff's steps say to. No AI attribution: no `Co-Authored-By` trailers and no "Generated with …" lines.
- **No local test harnesses:** no docker, incus or scratch VMs. Implement with unit tests, then build and lint. Real testing is manual on the test VPS (see the handoff).
- **Fix root causes:** no repair commands, stopgaps or healing code for a broken server. A bug that a correct release fixes is fixed, released and upgraded.
- **Supported platforms:** servers run only the latest Debian and Ubuntu LTS (Debian 13, Ubuntu 26.04). Operator machines run Linux and macOS.
- **Notes:** findings, plans and to-do lists go in `docs/*.md` in this repo.
- **Plans, specs and RFCs** are opened for review with `plannotator annotate <file> --tailscale` (run it in the background). Give the user only the link, with no summary of the content.
- **Chat:** never cite section numbers ("§12"); describe the content instead.
- **Commands for the user:** complete, copy-pasteable commands with real values, one step at a time, short replies. Run eru, curl and ssh checks yourself on thelab. Hand the user only what needs sudo, a TOTP code, an interactive prompt or another machine.
- **Out of scope:** don't bring up the deferred "old master comes back and is fenced" live test.
- **Release model:** `master` is edge; `1.x` is the whole v1 line (alphas, final, minors, patches). Changes go in by pull request; v1 fixes target `1.x`, which is merged up into `master` with a merge commit; no cherry-picks. Details in `docs/PLAN-v1-release.md`. Never cut a branch or push a tag without the user.

## Verify before every commit

```
go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint
```

## Release and deploy (edge)

1. Push to `master`. Each push publishes an edge release: `gh run list --repo mbvlabs/eru --limit 1`, then `gh run watch <id> --repo mbvlabs/eru --exit-status`.
2. On thelab: `eru upgrade --yes`, then `eru -c <context> apply --auto-upgrade-nodes`, then `eru -c <context> doctor`.

## Machines

- **thelab** (this machine, `thelab.tailbbed89.ts.net`): the dev box. eru contexts live in `~/.config/eru/contexts/`. You can run `eru`, `curl` and `eru ssh --command` here.
- **The user's desktop:** holds the only root SSH key, so `server init` and `server init --restore` run there with `--identity ~/.ssh/mbvlabs_prod_one`. Give the user the full command.
- **Test VPS** `46.224.129.96` (`db2.mbvlabs.com`, Hetzner, Ubuntu 26.04): rebuilt in place when needed, so the IP stays the same.
- **Contexts:**
  - `nobucket`: the live server on the VPS, with no bucket.
  - `mbvlabs`: Cloudflare R2 bucket `eru-backups-test`, prefix `mbvlabs`; it has no live server right now.
  - Read the bucket from thelab with `curl --aws-sigv4 "aws:amz:auto:s3" --user "$AK:$SK"`, taking the endpoint and keys from `~/.config/eru/contexts/mbvlabs/credentials.json` with jq. Never print the secret values.
