#!/usr/bin/env bash
# Step 13 (pgbouncer-v1): start both poolers with the rendered configuration,
# or reload the ones already running: the transaction pooler (Debian's
# pgbouncer.service on 6432) and the session pooler (vaire's own unit on 5432).
# Adding or removing a database only changes [databases], which a reload
# picks up without dropping a client (RFC 8.1). The Go step writes the files
# owned by postgres:postgres with the right modes, so there is nothing to fix
# up here.
set -euo pipefail

: "${PGBOUNCER_DIR:?PGBOUNCER_DIR is required}"
: "${SESSION_SERVICE:?SESSION_SERVICE is required}"

systemctl daemon-reload
for unit in pgbouncer "${SESSION_SERVICE}"; do
  systemctl --quiet enable "${unit}"
  if systemctl is-active --quiet "${unit}"; then
    systemctl reload "${unit}"
    printf '%s reloaded\n' "${unit}"
  else
    systemctl restart "${unit}"
    printf '%s started\n' "${unit}"
  fi
done
