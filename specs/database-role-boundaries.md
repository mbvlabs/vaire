# Vaire database roles: keep application access separate from ownership

**Proposal for review.** Follow PlanetScale Postgres’s least-privilege model: use an application credential for routine data access and a separate, more privileged credential for migrations. **No manual migration window.** This is about what Vaire enforces, independent of any application framework.

## 1. Who connects where?

```text
 Application ── app credential ──► transaction pooler (:6432 or TLS :443) ──┐
                                                                          │
 Reports ────── ro credential ───► transaction pooler (:6432 or TLS :443) ──┼─► app cluster
                                                                          │
 Migrations ─── migrate credential ► session pooler (:5432) ───────────────┘
                  │
                  └──► transaction pooler (:6432 / :443) ──✕ login refused
```

This shows the intended paths, not an exclusive port rule for `app` or `ro`. The migration login is refused by the transaction pooler. Use the session pooler over WireGuard by default; existing explicit network exposure settings are unchanged.

## 2. What can each login do?

```text
 <db>_ro ───────────────► SELECT on granted application objects
 <db>_app ──────────────► SELECT / INSERT / UPDATE / DELETE
                                 │
                                 └──✕ no application schema ownership or DDL

 <db>_migrate ── assumes ──► <db>_owner (NOLOGIN)
                                 ├──► Create, alter, and drop owned schema objects
                                 └──► Read and write data for backfills and migrations
```

| Capability | `ro` | `app` | `migrate` |
| --- | --- | --- | --- |
| Read granted application tables | Yes | Yes | Yes |
| Insert, update, or delete rows | No | Yes | Yes |
| Manage permanent schema objects | No | No | Yes, as owner |
| Assume the database owner role | No | No | Yes |
| Administer the VPS, act as PostgreSQL superuser, or use the credential on another managed database | No | No | No |

A migration such as adding a column and then backfilling it needs both `ALTER TABLE` and `UPDATE`. The migration role remains an owner-level credential for that database, not a DDL-only filter. That includes destructive changes to owned objects. If someone deliberately gives an application that credential and a reachable session-pooler route, Vaire cannot tell its `UPDATE` from a backfill. Keep the migration credential out of runtime configuration; the enforced boundary is that an **app** credential cannot elevate itself.

## What would change?

Vaire already provisions `<db>_owner`, `<db>_migrate`, `<db>_app`, and `<db>_ro`, and excludes the migration role from transaction-pooler authentication. Keep those roles, URLs, and default connection behavior. The first increment is to verify the actual SQL grants and pooler behavior, then clarify the credential guidance wherever Vaire hands out URLs. Change enforcement only if verification finds a gap in the agreed boundary; report the gap before proposing a broader change.

No new commands, TTLs, SQL classification, default privilege expansions, or changes to TOTP/operator protection. In particular, the migration credential is not an operator TOTP credential, and marking a database protected does not make its application roles read-only.

## How we’ll verify it

Run on two disposable databases using the generated credentials and normal connection routes:

1. Via `migrate` on the session pooler, create a permanent table, alter it, and backfill rows. Confirm the migration login is refused on the transaction pooler, including its TLS-on-443 route.
2. Via `app`, read and modify those rows, including tables created after initial setup. Confirm it cannot create a permanent table in the application schema, alter or drop the migrated table, or assume `<db>_owner`.
3. Via `ro`, read the migrated rows and confirm a write is denied even if the session’s read-only setting is disabled. That proves grants enforce the boundary.
4. Try the first database’s credentials against the second database’s route. Confirm they cannot authenticate there. Verify `migrate` has no superuser, role-creation, or database-creation privileges.

**Review decision:** Keep Vaire’s existing role split and session-only migration login, with no activation step. Verify and document the boundary before changing any privileges.

Reference: https://planetscale.com/docs/postgres/connecting/roles#example-role-configurations
