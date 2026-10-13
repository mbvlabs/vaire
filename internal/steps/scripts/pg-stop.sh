#!/usr/bin/env bash
# Stop one cluster before a restart-requiring change.
set -euo pipefail

: "${CLUSTER:?CLUSTER is required}"

systemctl stop "postgresql@18-${CLUSTER}.service" 2>/dev/null || pg_ctlcluster 18 "${CLUSTER}" stop -m fast
printf 'cluster %s stopped\n' "${CLUSTER}"
