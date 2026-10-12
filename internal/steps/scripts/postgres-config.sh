#!/usr/bin/env bash
# Step 11 (postgres-config-v1): reload PostgreSQL with the rendered files. The
# Go step writes them owned by postgres:postgres with the right modes before
# this runs, so there is nothing to fix up here.
set -euo pipefail

: "${PG_CONF_DIR:?PG_CONF_DIR is required}"

pg_ctlcluster 18 main reload
printf 'configuration reloaded\n'
