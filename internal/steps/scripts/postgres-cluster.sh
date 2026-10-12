#!/usr/bin/env bash
# Step 9 (postgres-cluster-v1): create the 18/main cluster if it is missing,
# apply the systemd drop-in, start it and assert data checksums are on.
set -euo pipefail

: "${DATA_DIR:?DATA_DIR is required}"
: "${LOCALE_PROVIDER:?LOCALE_PROVIDER is required}"
: "${LOCALE:?LOCALE is required}"

cluster_exists() {
  pg_lsclusters -h | awk '$1 == "18" && $2 == "main" { found = 1 } END { exit !found }'
}

if ! cluster_exists; then
  parent="$(dirname "${DATA_DIR}")"
  if [ ! -d "${parent}" ]; then
    install -d -o postgres -g postgres -m 0700 "${parent}"
  fi
  pg_createcluster 18 main \
    --datadir="${DATA_DIR}" \
    --locale-provider="${LOCALE_PROVIDER}" \
    --locale="${LOCALE}" \
    --start-conf=auto \
    -- --auth-local=peer --auth-host=scram-sha-256
fi

systemctl daemon-reload
systemctl restart postgresql@18-main.service 2>/dev/null || pg_ctlcluster 18 main restart

checksums="$(runuser -u postgres -- psql -X -q -tAc 'SHOW data_checksums')"
if [ "${checksums}" != "on" ]; then
  printf 'data_checksums is %s, want on\n' "${checksums}" >&2
  exit 1
fi

printf 'cluster 18/main is online\n'
