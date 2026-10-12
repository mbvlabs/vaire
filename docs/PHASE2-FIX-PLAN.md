# Plan: fix everything in PHASE2-FINDINGS.md

One branch (`fix/phase2-findings`), one PR, built in the order below so each part builds on the one before. Nothing gets committed until you ask. The two "fixed locally" changes (`loadPending` → `ParseAnswers` and the `pg_createcluster` initdb args) are kept as they are and become the first commit.

The questions are at the end. Parts 4 and 5 depend on them.

---

## 1. Small bugs and validation (do first, low risk)

| Finding | Change |
| --- | --- |
| Bare `email` line in self-signed mode | Goes away with the Caddy removal (part 4). Until then, guard it with `{{- if .ACMEEmail }}` so the commits in between are correct. A render test covers both modes. |
| `--resume` "unknown keys: bootstrap" | Keep the local fix. Add a test: `SaveAnswers` → `loadPending` round trip with a `[bootstrap]` table. |
| `pg_createcluster --locale-provider` | Keep the local fix. Add a script-level test that runs the script with a stubbed `pg_createcluster` (prints its argv) for `builtin`, `icu` and `libc`. |
| `acme_email` optional | Remove the required check in `config/cluster.go`. Update the cluster.toml validation rules in IMPLEMENTATION.md. |
| ACME email prompt label | `"ACME contact email (optional, account recovery only)"`, with a description saying that renewal is automatic and that empty is fine. |
| Blank database answer becomes `app` | Prompt title `"Databases (comma separated, default app)"`, with the placeholder set to `app`. |
| `--identity` read too late | `collect()` (and `loadPending`) `stat`s and opens the file right after flag parsing, before any prompt or secret generation. A bad path is `usage` with the path in details. On `--resume` the stored identity is checked the same way. |
| `step_failed` hint during init | `stepFailed` takes a hint from the runner. `engine.Runner` gets a `ResumeHint` field, which `cluster.bootstrap` sets to `eru cluster init --resume --name <n>` and apply/tune/db leave at the current text. |

## 2. Step output on failure

- `engine.Shell.stream` keeps a ring buffer of the last 20 redacted lines.
- On a non-zero exit it returns a typed `*ScriptError{Step, Status, Tail []string}`. Its `Error()` keeps today's message, so nothing that matches on the message breaks.
- `stepFailed` unwraps `ScriptError` and adds `details.output_tail`. The tail travels back over RPC in the error details, which already pass through.
- Human error rendering in `internal/cli/output` prints the tail under the message (styled in part 3). JSON, agent and markdown modes get it as `details.output_tail`.
- Test: a script that prints 30 lines and runs `exit 3` → the error has exactly the last 20 lines, with redaction applied.

## 3. Charm CLI (bubbletea + lipgloss + bubbles + themed huh)

New package `internal/cli/ui`. It is the only place that knows about Charm, so the rest of the CLI calls a small interface:

```go
type UI interface {
    Phase(name, title string) Phase          // header + spinner; Phase.Done/Fail/Warn/Update(detail)
    Steps(total int) StepTracker             // progress bar for [i/n] node steps
    Wait(title string, tick func() (status string, done bool)) // live-status waits
    Panel(title string, body string)         // boxed blocks (DNS records, secrets passphrase)
    Warn(msg string)
    Summary(card SummaryCard)
    Error(err *output.CLIError)
    Confirm(title, word string) error        // typed-word confirm via huh
    Input/Secret/YesNo(...)
}
```

Two implementations:

- **`ui.Plain`**: today's line output, byte for byte where tests depend on it. Used for `--no-input`, non-TTY stdin or stderr, `--output json|agent|markdown`, `--stream`, `--quiet`, `--jq`, and `NO_COLOR`/`TERM=dumb` (plain lines, no ANSI).
- **`ui.Rich`**: one long-lived `tea.Program` writing to stderr. Node events from `progress.event` are sent to it as messages, and a `log` event becomes the spinner's live detail line (the last line of output, truncated), so long apt installs visibly move. `--verbose` still prints full log lines above the live area.

Picking one: `progress` (in `progress.go`) becomes a thin adapter that holds a `ui.UI`. All existing call sites (`r.p.phase`, `r.p.note`, `r.p.event`) keep working, and new call sites use the richer methods.

What each finding needs:

- **Spinners with elapsed time**, ✓/✗/⚠ on end, for every local phase (`init.secrets`, `init.first-contact`, `init.upload`, `init.host`, `init.switch`, `init.lockdown`, `init.dns`, `init.database`, `init.tls`, `init.secrets-doc`) and every node step.
- **Progress bars:** node steps use `[i/n]` from the step events. The binary upload counts bytes through an `io.Writer` wrapper in the SCP/upload path.
- **Live waits:**
    - DNS (if it still waits, see Q4): per resolver, `A 104.21.27.128 ✗ want 46.224.129.96`, plus a countdown.
    - Certificate wait: the `tls.status` state and seconds left.
    - WireGuard switch: `handshake attempt 3/10`. That needs the transport's retry loop to report its attempts through a callback.
    - Lockdown: the revert countdown.
- **Layout:** lipgloss phase headers (host → lockdown → DNS → database → TLS → secrets), a boxed DNS records panel (plain `TYPE NAME VALUE` lines inside, so it copies cleanly), a warning style, and a final summary card with connection URLs, TLS state and `next` commands.
- **Errors:** a red block with the code and message, the failed step, the `output_tail` in a dim box, and the hint as a copyable command.
- **Prompts:** a single `huh` theme (`huh.ThemeCharm` adjusted to eru's palette) for the interview, `Confirm` (typed word, validated inline), the host-key trust (fingerprint shown in the description, yes/no), the passphrase (`EchoModePassword`) and `PromptYes`. `promptLine`/`readSecretTTY` go through `huh` in Rich mode. The raw readers stay for Plain mode, where tests feed stdin.
- **Typed confirmation words:** `Type 'confirmed' once the records exist`, `Type 'stored' once both are stored safely`, and `Type '<token>' to confirm`. Accept `confirmed` (and still accept `added`, so a habit doesn't break). IMPLEMENTATION.md's DNS step is updated to match.
- **Other commands:** `apply`, `db create`, `db drop` and `tune apply` already go through `progress`, so they get spinners and step bars for free. `db drop`'s confirm goes through `ui.Confirm`.
- go.mod: `bubbletea`, `lipgloss` and `bubbles` move to direct dependencies.
- Tests: Plain mode golden outputs stay. Rich mode gets a `teatest` test for the phase/step model, plus a test that `ui.Select` picks Plain for every structured flag combination.

## 4. Replace Caddy with nginx and add Postgres on 443

### Packages and units
- Step 12 becomes `tls-v2`. The step ID changes, so the journal marks `tls-v1` as obsolete, which the runner already handles. `tls-v2` removes Caddy if it's present: `systemctl disable --now caddy`, `apt-mark unhold`, `apt-get purge caddy`, and remove `/etc/caddy`. The `/var/lib/caddy` data is moved to `/var/lib/eru/caddy-old` rather than deleted.
- Install `nginx` and `libnginx-mod-stream` from the distribution (Debian 13 / Ubuntu 26.04 both ship nginx ≥ 1.26 with `ssl_preread` and ALPN in the stream module). The packages are pinned in `internal/versions` by minimum version only, the same way as PgBouncer, and held in unattended-upgrades' blacklist (`nginx` replaces `caddy` in step 6).

### Certificates (depends on Q1)
Recommended: **eru does ACME itself** with `golang.org/x/crypto/acme`, which is already a dependency through `x/crypto`, so there's nothing new to pin, download or keep updated on the box.
- `eru node run certs-renew` (it replaces `certs-sync`): if the cert in `/etc/eru/tls` is missing, a placeholder, or within 30 days of expiry, it runs the HTTP-01 challenge. The token is written to `/var/lib/eru/acme/.well-known/acme-challenge/`, and nginx serves that directory on :80. Then it writes the cert and key atomically (same modes and `ssl-cert` group as today) and reloads PG, PgBouncer and nginx.
- The ACME account key lives in `/etc/eru/acme/account.key` (0600). It goes into the VPS-backup include list in place of `/var/lib/caddy`. `acme_email` is optional (part 1), and `acme_ca` keeps working as the directory URL.
- Units: `eru-certs.timer` (twice daily, randomized) + `eru-certs.service`. The `.path` unit goes away, because the source of truth is now eru itself.
- `tls-v2`'s Apply writes the placeholder (as today), starts nginx, then runs `certs-renew` once in the background (`systemctl start --no-block eru-certs.service`). TLS never blocks on ACME, and the certificate wait in init still polls `tls.status`.
- `self-signed` mode: eru makes a local CA (`/etc/eru/tls/ca.{crt,key}`) and a leaf signed by it, and renews the leaf on the same timer. This replaces Caddy's `tls internal`.

### nginx config (rendered by `internal/render`)
```nginx
# /etc/nginx/nginx.conf: stream {} + http {}; distro default sites removed
stream {
    map $ssl_preread_alpn_protocols $eru_upstream {
        ~\bpostgresql\b   eru_pg_tls;      # PG17+ sslnegotiation=direct
        default           eru_https;       # http/1.1, h2, no ALPN
    }
    server { listen 443; listen [::]:443; ssl_preread on; proxy_pass $eru_upstream; }
    upstream eru_https  { server 127.0.0.1:8443; }
    upstream eru_pg_tls { server 127.0.0.1:6433; }
    # TLS termination for direct-SSL Postgres clients -> plaintext PgBouncer on loopback (see Q2)
    server {
        listen 127.0.0.1:6433 ssl;
        ssl_certificate /etc/eru/tls/server.crt; ssl_certificate_key /etc/eru/tls/server.key;
        ssl_alpn postgresql;
        proxy_pass 127.0.0.1:6432;
    }
}
http {
    server { listen 80; listen [::]:80;
        location /.well-known/acme-challenge/ { root /var/lib/eru/acme; }
        location / { return 404; } }
    server { listen 127.0.0.1:8443 ssl; server_name {{ .Domain }};
        ssl_certificate ...; location / { return 404; } }   # Phase 6 adds /metrics here
}
```
- PgBouncer `pg_hba`/`auth` lets the loopback hop in without TLS (`host` rule for 127.0.0.1 only). Clients on 6432 and 5432 are unchanged.
- ufw: 443 is already open. Nothing new.
- `nginx -t` replaces `caddy validate`. `node logs` allowlist: `caddy` → `nginx`, `eru-certs-sync` → `eru-certs`.

### Spec and docs
IMPLEMENTATION.md: the TLS, metrics and Caddy section becomes "TLS, metrics and nginx", plus the `tls-v2` step row, the pinned versions, the secrets-document URLs (an extra `app_direct_tls` URL: `postgres://…@domain:443/app?sslmode=verify-full&sslnegotiation=direct&sslrootcert=system`), and the backup include list. MASTER.md: the client access, metrics edge and step 12 rows, and the Caddy design subsection. `agents.md`: the `tls_pending` wording.

## 5. DNS: stop blocking (depends on Q4)

Recommended:
- Show the records panel early (as today) and again at the DNS phase. Then do **one** resolver check (a couple of seconds, no polling) and continue to the database phase whatever the result.
- Mismatch → a `dns_mismatch` warning listing per-resolver answers. Answers inside Cloudflare's published ranges (embedded list of `ips-v4`/`ips-v6`, refreshed with each release) → a `dns_proxied` warning: "proxied by Cloudflare: database clients can't connect through it; set the record to DNS only (grey cloud)". The records panel always says "DNS only / not proxied".
- The typed `confirmed` prompt stays, but only as a pause ("press enter / type 'confirmed' once the records exist") so the operator has time to create them. `--dns-ready` skips it. `--skip-dns-check` is kept as an accepted no-op with a deprecation warning.
- The certificate wait becomes the real check: 120 s with live state. If it's still pending, the `tls_pending` warning includes the resolver answers and the Cloudflare hint.

## 6. Init housekeeping

- **Discard:** `eru cluster init --discard --name <n>` (`--yes` or a typed `'discard'` confirm). If `progress.json` shows the host phase ran, it warns: "the server at <ip> has already been bootstrapped; eru won't touch it". The `init_pending` error gets `details.pending_path` and a hint naming both `--resume` and `--discard`.
- **`~/.ssh/config`** (depends on Q3): recommended, read the matching `Host` block with `github.com/kevinburke/ssh_config` for `HostName`, `User`, `Port` and `IdentityFile` (`IdentitiesOnly` respected). Explicit flags win. The chosen source is shown in the first-contact phase line.
- **Encrypted default keys:** in a TTY, ask for the passphrase (via `ui.Secret`) for each encrypted default key that ssh-agent doesn't already hold. Non-interactively, the error names the skipped keys in `details.skipped_keys`.

## 7. Process notes

- **Review pass** of `internal/steps` and `internal/node` (role SQL reruns, `pg_hba` + PgBouncer auth, certs code, the `db drop` confirmation). Real bugs get fixed in this branch with a test each. Anything debatable goes into `docs/PHASE2-FINDINGS.md` under a new "Review" heading for you to decide.
- **Manual VPS test:** I can't run this; it stays open for you. I'll add a checklist to the findings file covering the extra paths this branch adds (resume, discard, self-signed init, 443 direct-TLS connect with `psql "sslnegotiation=direct"`, migration from a Caddy box).
- **Stacked-branch note:** nothing to code. I'll save it as a memory so I follow it next time.

## Verification

`just lint` and build only. Everything else is checked in the manual VPS test.

---

## Questions

:::question
How should nginx get certificates?

nginx has no ACME client. Whatever we pick replaces Caddy's automatic issuance and the certs-sync path watcher.

- [ ] eru does ACME itself (golang.org/x/crypto/acme, HTTP-01 via nginx webroot) — nothing extra on the box, one renewal path we own and test
- [ ] certbot from apt with the webroot plugin + deploy hook — well known, but another moving part and Python on the box
- [ ] acme.sh — single shell script, needs pinning/sha like Caddy did

Recommended: eru does ACME itself (golang.org/x/crypto/acme, HTTP-01 via nginx webroot)
:::

:::question
How should Postgres-over-443 reach PgBouncer?

PgBouncer doesn't speak direct-TLS negotiation (`sslnegotiation=direct`), so passthrough on 443 can only reach PostgreSQL itself.

- [ ] nginx terminates TLS (ALPN postgresql) and forwards plaintext to PgBouncer on loopback — pooled, works with every PG17+ client
- [ ] Passthrough to PostgreSQL 5432 (PG18 supports direct TLS natively) — no pooling on 443
- [ ] Both: ALPN postgresql → PgBouncer, plus SNI `direct.<domain>` → Postgres

Recommended: nginx terminates TLS (ALPN postgresql) and forwards plaintext to PgBouncer on loopback
:::

:::question
First contact and ~/.ssh/config: read it, or just document that it's ignored?

- [ ] Read the matching Host block (HostName, User, Port, IdentityFile) — one small dependency
- [ ] Document in --help and the auth error hint that it isn't used

Recommended: Read the matching Host block (HostName, User, Port, IdentityFile)
:::

:::question
The DNS step after "don't block on DNS": keep a typed pause before the database phase?

The findings ask both for a clearer `Type 'confirmed'` prompt and for not blocking on DNS.

- [ ] Keep a short typed 'confirmed' pause (skippable with --dns-ready), then one non-blocking check — gives time to create the record before the cert wait starts
- [ ] No pause at all: show records, check once, go straight on
- [ ] Keep polling but non-fatal: live per-resolver status for up to 60 s, then continue

Recommended: Keep a short typed 'confirmed' pause (skippable with --dns-ready), then one non-blocking check
:::

:::question
Existing Caddy boxes: migrate automatically in tls-v2, or is the only box the test VPS (which you'd reinstall)?

- [ ] Migrate automatically (stop + purge Caddy, keep its data aside)
- [ ] No migration: test VPS gets rebuilt; tls-v2 just refuses if caddy is installed

Recommended: Migrate automatically (stop + purge Caddy, keep its data aside)
:::
