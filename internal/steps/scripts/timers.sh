#!/usr/bin/env bash
# Step 17 (timers-v1): disable and remove the eru units no longer rendered,
# then enable every rendered timer. eru wrote the unit files before this runs.
set -euo pipefail

: "${SYSTEMD_DIR:?SYSTEMD_DIR is required}"
: "${ENABLE:?ENABLE is required}"
STALE="${STALE:-}"

for unit in ${STALE}; do
  case "${unit}" in
    *.timer|*.path|eru-fence.service) systemctl disable --now "${unit}" 2>/dev/null || true ;;
  esac
  rm -f "${SYSTEMD_DIR}/${unit}"
  printf 'removed %s\n' "${unit}"
done

systemctl daemon-reload

for unit in ${ENABLE}; do
  systemctl enable --now "${unit}" >/dev/null
  printf 'enabled %s\n' "${unit}"
done
