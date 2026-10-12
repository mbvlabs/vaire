# Phase 2 findings to fix

Found while merging PR #6 (Phase 2, first cluster, `8a5c562`) and preparing the first manual test from a laptop against the edge build `edge-8a5c56209c23`. The VPS manual test hasn't run yet, so add whatever it turns up here.

First manual test run (2026-10-12): Hetzner VPS `46.224.129.96` on Ubuntu 26.04, cluster `testing-setup`, domain `database.mbvlabs.com`, `acme` mode, run from a laptop with `eru cluster init --host`. It got through the host phase, the lockdown and the first two database steps, then stopped at `postgres-cluster-v1`. Two fixes were made locally and aren't committed; they're marked "fixed locally" below. Everything else gets fixed in one go.

## Bugs

- [x] **Bare `email` line in the Caddyfile in `self-signed` mode**
    - Where: `internal/render/templates/Caddyfile.tmpl` writes `email {{ .ACMEEmail }}` with no condition.
    - Impact: a `self-signed` cluster has no ACME email, so the Caddyfile gets a bare `email` line. Caddy's `email` option takes exactly one argument, so `caddy validate` should reject it and step 12 (`tls-v1`) should fail. This hasn't been run yet.
    - Fix: wrap the line in `{{- if .ACMEEmail }}` … `{{- end }}`, like the `acme_ca` line under it. Then test a `--tls-mode self-signed` init.

- [x] **`cluster init --resume` always fails: "unknown keys: bootstrap"** (fixed locally, not released)
    - Seen in the first manual test. `contexts.SaveAnswers` writes the pending `answers.toml` as cluster.toml plus a `[bootstrap]` table, but `loadPending` in `internal/cli/clusterinit.go` read it back with the strict `config.ParseCluster`, which rejects unknown keys.
    - Impact: no unfinished init can be resumed.
    - Fix: `loadPending` now uses `config.ParseAnswers`, which strips `[bootstrap]` like the `--answers` path does. It still has to be tested with a real `--resume`.

- [x] **`postgres-cluster-v1` always fails: `pg_createcluster` has no `--locale-provider`** (fixed locally, not released)
    - Seen in the first manual test: step 9 exited with status 1. postgresql-common's `pg_createcluster` rejects unknown options, and `--locale-provider` isn't one of them.
    - Fix: `internal/steps/scripts/postgres-cluster.sh` now passes `--locale-provider` and `--builtin-locale` (or `--icu-locale`) to initdb after `--`. It still has to be tested on a real VPS.
- [x] **A failed step doesn't show the script's output**
    - Seen in the first manual test: the error was only "script for postgres-cluster-v1 exited with status 1". The script's output is streamed as log events and printed only with `--verbose` (`internal/engine/shell.go`).
    - Fix: keep the last ~20 lines of output and put them in the `step_failed` error, in its details and in human output.
- [x] **The `step_failed` hint during init says "re-run apply"**
    - During `cluster init` there's no context yet, so `eru apply` can't work. The hint should say `eru cluster init --resume`.

## CLI and UX improvements

- [x] **Build a proper, good-looking CLI with the Charm packages: more feedback and loading indicators everywhere** (high priority)
    - Seen in the first manual test: `cluster init` looks like raw log output, and long waits show nothing. That covers the DNS check (up to 5 min of silence), package installs, the WireGuard switch, the lockdown and the certificate wait. It's hard to tell whether eru is working, waiting for you, or stuck.
    - Where: `internal/cli/interview.go` builds a plain `huh` form with no theme. The progress output in `internal/cli/progress.go` is plain `fmt` lines (`init.secrets started: …`, `[3/7] step started`). The confirm prompts in `internal/cli/prompts.go` and `promptLine`/`readSecretTTY` are raw terminal reads.
    - Why: MASTER.md and IMPLEMENTATION.md specify `huh` for prompts and `bubbletea` + `lipgloss` + `bubbles` for TUIs and progress, the same Charm stack as deploycrate. `bubbletea`, `lipgloss` and `bubbles` are only indirect dependencies in `go.mod` today.
    - Fix:
        - **Loading indicators:** a `bubbles` spinner on every phase and step that runs, with the step name and elapsed time, replaced by ✓/✗ when it ends. A progress bar for the database steps (`[3/7]`) and for the binary upload.
        - **Feedback while waiting:** every wait says what it's waiting for and shows live status. For DNS, each resolver's current answer against the expected IP and a countdown. For the certificate, the TLS state. For WireGuard, which handshake attempt it's on. Never more than a few seconds without visible change.
        - **Layout:** `lipgloss` styling for the phase headers (host → lockdown → DNS → database → TLS → secrets), a boxed DNS records panel ready to copy, warnings in a distinct style, and a final summary card with connection URLs and next steps.
        - **Errors:** a styled error block with the failed step, the last lines of its output (see the step output finding above) and the exact command to continue.
        - **Prompts:** a themed `huh` form for the interview, and `huh` confirms and inputs for the typed confirmations (`confirmed`, `stored`, confirm tokens), the host key trust and the passphrase.
        - Plain line output stays for `--no-input`, non-TTY, `--output json|agent|markdown` and `--stream`.
        - Apply the same treatment to the other long-running commands (`apply`, `db create/drop`, `tune apply`), not just `cluster init`.
- [x] **Make `acme_email` optional in `acme` mode**
    - Where: `internal/config/cluster.go:485` makes it required; the prompt is in `internal/cli/interview.go:113`.
    - Why: Caddy and Let's Encrypt both work without an email. It's only an account contact, and possibly what lets Caddy fall back to ZeroSSL.
    - Fix: drop the required check, and say in the prompt what the email is for and that leaving it empty is fine. Update the spec where it describes cluster.toml validation.
- [x] **Fix the ACME email prompt label**
    - Where: `internal/cli/interview.go:113`, "ACME email (certificate expiry notices)".
    - Why: Let's Encrypt stopped sending expiry emails in 2025, and Caddy renews certificates automatically anyway.
- [x] **`--identity` is only read after the run has started**
    - Seen in the first manual test: a mistyped `--identity /home/.ssh/…` failed only after the cluster name prompt, the bootstrap confirmation, secret generation, the DNS records and the host key prompt. It left an unfinished init that needs `--resume`.
    - Fix: check that the `--identity` file exists and is readable when the answers are collected, before secrets are generated or anything is prompted.
- [x] **No way to discard an unfinished init**
    - Seen in the first manual test: with `--resume` broken, the only way out was to delete `$XDG_CONFIG_HOME/eru/pending/<name>` (or `~/.config/eru/pending/<name>`) by hand. The `init_pending` error only suggests `--resume` and doesn't show the path.
    - Fix: add a discard option, e.g. `eru cluster init --discard --name <n>`, warning when the host phase already ran. Put the pending path in the error details and the hint.
- [x] **Typed confirmation words aren't marked as something to type**
    - Seen in the first manual test: "Type added once the records exist:" reads like a sentence, not an instruction to type the word `added`.
    - Where: `internal/cli/clusterinit_db.go:57` ("Type added …"), `internal/cli/clusterinit.go:1253` ("Type stored …") and `internal/cli/prompts.go:87` ("Type %s to confirm").
    - Fix: change the DNS prompt to `Type 'confirmed' once the records exist:` and accept `confirmed` instead of `added`. Update the DNS step in IMPLEMENTATION.md's remote init sequence to match. Quote the other words the same way: `Type 'stored' once both are stored safely:`, and the token in the confirm prompt. This fits with moving these prompts to `huh` (see the Charm finding above).
- [x] **The DNS check waits silently for up to 5 minutes**
    - Seen in the first manual test: after typing `added`, nothing was printed while `verifyDNS` (`internal/cli/dns.go:42`) polled every 10 s. It looked like a hang.
    - Fix: print a spinner or a line per attempt showing what each resolver returned against what was expected, e.g. `1.1.1.1: A 104.21.27.128 (want 46.224.129.96)`.
- [x] **Detect Cloudflare-proxied records**
    - Seen in the first manual test: `database.mbvlabs.com` was proxied by Cloudflare (orange cloud), so it resolved to Cloudflare IPs (104.21.x, 172.67.x, 2606:4700::) and the check could never pass. Postgres and PgBouncer can't run behind Cloudflare's HTTP proxy anyway.
    - Fix: when the answers are in Cloudflare's ranges, say so: "the record is proxied by Cloudflare; set it to DNS only (grey cloud)". Mention DNS only in the records prompt too.
- [x] **First contact ignores `~/.ssh/config`**
    - Where: `firstContactAuth` in `internal/cli/clusterinit.go:1000`.
    - Impact: `IdentityFile`, `User` and `Port` set for a host are ignored, so you have to repeat them with `--identity` and `--host user@ip:port`.
    - Fix: either read the matching `Host` block, or say in `--help` and the auth error hint that ssh config isn't used.
- [x] **Default keys with a passphrase are skipped silently**
    - Where: `internal/cli/clusterinit.go:1030`. Default `~/.ssh/id_*` keys are loaded with no passphrase prompt.
    - Impact: if your `id_ed25519` has a passphrase and isn't in ssh-agent, it's skipped and you just get "first contact needs an SSH credential".
    - Fix: in a terminal, ask for the passphrase as `--identity` already does, or name the skipped keys in the error.
- [x] **A blank database answer quietly becomes `app`**
    - Where: `internal/cli/interview.go:144`.
    - Fix: show `app` as the visible default in the prompt, e.g. "Databases (comma separated, default app)", so clearing the field doesn't look like "no database".

## Design changes

- [x] **Replace Caddy with nginx and serve Postgres on 443 through `stream`**
    - Why: nginx has layer 4 proxying built in (`stream` + `ssl_preread`), so it can route Postgres connections that use TLS (PG17+ `sslnegotiation=direct`, ALPN `postgresql`) on 443 to PgBouncer by SNI/ALPN. Caddy needs the third-party `caddy-l4` plugin and a custom build.
    - Cost: nginx doesn't get certificates by itself. eru would need certbot or acme.sh, a renewal timer and a reload hook into certs-sync.
    - Requirement: the database hostname must resolve to the server, i.e. "DNS only" (grey cloud) in Cloudflare. A proxied record ends the connection at Cloudflare, which only passes HTTP.
    - Spec: the TLS, metrics and Caddy part of IMPLEMENTATION.md, the `tls-v1` step, the pinned versions, and the connection URLs in the secrets document.
- [x] **Don't block init on DNS; trust the certificate wait instead**
    - Show the records, then go on to the database phase. The existing certificate wait already shows whether the certificate was issued, and anything other than an issued certificate is only a warning.
    - Warn, without blocking, when the record resolves to Cloudflare's ranges: "proxied: database clients can't connect through Cloudflare; set it to DNS only".
    - Spec: the DNS step in the remote init sequence.

## Process notes

- [ ] **Run the Phase 2 manual test and record it**
    - The test is in the Phase 2 entry of `docs/MASTER.md`: `cluster init`, then a `sslmode=verify-full` app connection to port 6432, `eru sql --db app "select 1"`, `eru apply` changing nothing, and `db drop` with only `--yes` being refused.
    - When it passes, record the date, eru version and VPS (provider, OS, size), and flip Phase 2 from 🟡 to ✅.
- [x] **Give the Phase 2 code a review pass**
    - The PR was 95 files and about 7.7k lines, merged with lint and builds only and no review.
    - Start with `internal/steps` and `internal/node`: rerunnable role SQL, `pg_hba` and PgBouncer auth, certs-sync, and the `db drop` confirmation check.
- [ ] **Stacked branches and squash merges**
    - PR #6 conflicted in 21 files because it was branched from the unmerged Phase 1 commit, which master then got as a squash commit.
    - Next time, once the parent PR merges, run `git rebase --onto origin/master <old parent commit>` before opening or updating the child PR.
    - Also make sure the squash title is right. Master now has "feat: pahse 2 first cluser (#6)", which can't be fixed without rewriting master.
