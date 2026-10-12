#!/usr/bin/env bash
# Step 13 (pgbouncer-v1): enable and restart PgBouncer with the rendered
# configuration. The Go step writes the files owned by postgres:postgres with
# the right modes, so there is nothing to fix up here.
set -euo pipefail

: "${PGBOUNCER_DIR:?PGBOUNCER_DIR is required}"

systemctl daemon-reload
systemctl enable pgbouncer
systemctl restart pgbouncer

printf 'pgbouncer restarted\n'
