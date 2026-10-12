#!/usr/bin/env bash
# Step 13 (pgbouncer-v1): fix ownership and modes, then enable and restart
# PgBouncer with the rendered configuration.
set -euo pipefail

: "${PGBOUNCER_DIR:?PGBOUNCER_DIR is required}"

files=("${PGBOUNCER_DIR}/pgbouncer.ini" "${PGBOUNCER_DIR}/pgbouncer_hba.conf" "${PGBOUNCER_DIR}/userlist.txt")
chown postgres:postgres "${files[@]}"
chmod 0640 "${files[@]}"

systemctl daemon-reload
systemctl enable pgbouncer
systemctl restart pgbouncer

printf 'pgbouncer restarted\n'
