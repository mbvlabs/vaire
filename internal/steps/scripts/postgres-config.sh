#!/usr/bin/env bash
# Step 10 (postgres-config-v1): fix ownership and modes of the rendered files
# and reload PostgreSQL.
set -euo pipefail

: "${PG_CONF_DIR:?PG_CONF_DIR is required}"

chown postgres:postgres "${PG_CONF_DIR}/conf.d/"*.conf
chmod 0644 "${PG_CONF_DIR}/conf.d/"*.conf
chown postgres:postgres "${PG_CONF_DIR}/pg_hba.conf" "${PG_CONF_DIR}/pg_ident.conf"
chmod 0640 "${PG_CONF_DIR}/pg_hba.conf" "${PG_CONF_DIR}/pg_ident.conf"

pg_ctlcluster 18 main reload
printf 'configuration reloaded\n'
