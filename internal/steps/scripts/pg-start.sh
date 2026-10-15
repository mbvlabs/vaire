#!/usr/bin/env bash
# Start one cluster after a restart-requiring change.
set -euo pipefail

: "${CLUSTER:?CLUSTER is required}"

systemctl start "postgresql@18-${CLUSTER}.service" || pg_ctlcluster 18 "${CLUSTER}" start
printf 'cluster %s started\n' "${CLUSTER}"
