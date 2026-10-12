#!/usr/bin/env bash
# Step 13 (pgbouncer-v1): fix ownership and modes, then enable and restart
# PgBouncer with the rendered configuration.
set -euo pipefail

: "${PGBOUNCER_DIR:?PGBOUNCER_DIR is required}"

chown postgres:postgres "${PGBOUNCER_DIR}/pgbouncer.ini" "${PGBOUNCER_DIR}/userlist.txt"
chmod 0640 "${PGBOUNCER_DIR}/pgbouncer.ini" "${PGBOUNCER_DIR}/userlist.txt"

systemctl daemon-reload
systemctl enable pgbouncer
systemctl restart pgbouncer

printf 'pgbouncer restarted\n'
