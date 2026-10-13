#!/usr/bin/env bash
# postgres-cluster-v1@<cluster>: create the cluster 18/<cluster> on its socket
# port if it is missing and start it. The Go step has written the systemd
# drop-in with the cgroup limits; a running cluster takes changed limits
# live through set-property, never a restart. Then assert data checksums.
set -euo pipefail

: "${CLUSTER:?CLUSTER is required}"
: "${PORT:?PORT is required}"
: "${DATA_DIR:?DATA_DIR is required}"
: "${LOCALE_PROVIDER:?LOCALE_PROVIDER is required}"
: "${LOCALE:?LOCALE is required}"
: "${MEMORY_HIGH:?MEMORY_HIGH is required}"
: "${MEMORY_MAX:?MEMORY_MAX is required}"
: "${CPU_WEIGHT:?CPU_WEIGHT is required}"
: "${IO_WEIGHT:?IO_WEIGHT is required}"
CPU_QUOTA="${CPU_QUOTA:-}"

unit="postgresql@18-${CLUSTER}.service"

cluster_exists() {
  pg_lsclusters -h | awk -v c="${CLUSTER}" '$1 == "18" && $2 == c { found = 1 } END { exit !found }'
}

if ! cluster_exists; then
  parent="$(dirname "${DATA_DIR}")"
  if [ ! -d "${parent}" ]; then
    install -d -o postgres -g postgres -m 0700 "${parent}"
  fi
  # pg_createcluster has no --locale-provider; the provider and its locale
  # go to initdb after --.
  provider_args=(--locale-provider="${LOCALE_PROVIDER}")
  case "${LOCALE_PROVIDER}" in
    builtin) provider_args+=(--builtin-locale="${LOCALE}") ;;
    icu) provider_args+=(--icu-locale="${LOCALE}") ;;
  esac
  pg_createcluster 18 "${CLUSTER}" \
    --port="${PORT}" \
    --datadir="${DATA_DIR}" \
    --locale="${LOCALE}" \
    --start-conf=auto \
    -- --auth-local=peer --auth-host=scram-sha-256 "${provider_args[@]}"
fi

systemctl daemon-reload
if systemctl is-active --quiet "${unit}"; then
  # The drop-in holds the limits across reboots; --runtime applies them to
  # the running cgroup without writing a second copy.
  systemctl set-property --runtime "${unit}" \
    "MemoryHigh=${MEMORY_HIGH}" "MemoryMax=${MEMORY_MAX}" \
    "CPUWeight=${CPU_WEIGHT}" "CPUQuota=${CPU_QUOTA}" "IOWeight=${IO_WEIGHT}"
else
  systemctl start "${unit}"
fi

checksums="$(runuser -u postgres -- psql -X -q -h /var/run/postgresql -p "${PORT}" -tAc 'SHOW data_checksums')"
if [ "${checksums}" != "on" ]; then
  printf 'data_checksums is %s, want on\n' "${checksums}" >&2
  exit 1
fi

printf 'cluster 18/%s is online\n' "${CLUSTER}"
