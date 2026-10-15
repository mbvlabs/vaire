#!/usr/bin/env bash
# Step 16 (metrics-v1). PHASE=users: the exporters' system users and the
# textfile directory, before eru writes the units and fetches the binaries.
# PHASE=start (after eru wrote everything): reload systemd, start the
# exporters in START_UNITS (restarting running ones, since their unit or
# binary may have changed), stop and disable those in STOP_UNITS, reload nginx
# and fail2ban. PHASE=disable ([metrics] enabled = false): stop and disable
# every exporter and reload nginx without the metrics include, which eru has
# already removed.
set -euo pipefail

: "${PHASE:?PHASE is required}"

if [ "${PHASE}" = "users" ]; then
  : "${EXPORTER_USERS:?EXPORTER_USERS is required}"
  : "${TEXTFILE_DIR:?TEXTFILE_DIR is required}"
  for user in ${EXPORTER_USERS}; do
    if ! id -u "${user}" >/dev/null 2>&1; then
      useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --user-group "${user}"
      printf 'created system user %s\n' "${user}"
    fi
  done
  install -d -m 0755 "${TEXTFILE_DIR}"
  exit 0
fi

systemctl daemon-reload

if [ "${PHASE}" = "disable" ]; then
  : "${ALL_UNITS:?ALL_UNITS is required}"
  for unit in ${ALL_UNITS}; do
    if systemctl list-unit-files --quiet "${unit}" >/dev/null 2>&1; then
      systemctl disable --now "${unit}" >/dev/null 2>&1 || true
    fi
  done
  nginx -t
  systemctl reload nginx
  printf 'metrics disabled: exporters stopped, /metrics removed\n'
  exit 0
fi

for unit in ${STOP_UNITS:-}; do
  systemctl disable --now "${unit}" >/dev/null 2>&1 || true
  printf '%s stopped\n' "${unit}"
done
for unit in ${START_UNITS:-}; do
  systemctl --quiet enable "${unit}"
  systemctl restart "${unit}"
  printf '%s started\n' "${unit}"
done
nginx -t
systemctl reload nginx
if systemctl is-active --quiet fail2ban; then
  systemctl reload fail2ban || systemctl restart fail2ban
fi
printf 'metrics endpoint reloaded\n'
