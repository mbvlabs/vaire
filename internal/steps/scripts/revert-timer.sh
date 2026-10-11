#!/usr/bin/env bash
# Arm or disarm a transient systemd timer that runs /usr/local/bin/eru with
# EXEC_ARGS after ON_ACTIVE seconds. Commit-confirm (§8.3) and break-glass
# (§8.4) use it to undo an SSH change that nobody confirmed.
set -euo pipefail

: "${UNIT:?UNIT is required}"
: "${ACTION:?ACTION is required}"
: "${ON_ACTIVE:=0}"
: "${EXEC_ARGS:=}"

systemctl stop "${UNIT}.timer" >/dev/null 2>&1 || true
systemctl reset-failed "${UNIT}.timer" "${UNIT}.service" >/dev/null 2>&1 || true

case "${ACTION}" in
  arm)
    [ "${ON_ACTIVE}" -gt 0 ] || { printf 'ON_ACTIVE must be positive\n' >&2; exit 2; }
    read -r -a args <<< "${EXEC_ARGS}"
    systemd-run --quiet --unit="${UNIT}" --on-active="${ON_ACTIVE}s" \
      --timer-property=AccuracySec=1s --description="eru ${UNIT}" \
      /usr/local/bin/eru "${args[@]}"
    printf 'armed %s.timer for %s seconds\n' "${UNIT}" "${ON_ACTIVE}"
    ;;
  disarm)
    printf 'disarmed %s.timer\n' "${UNIT}"
    ;;
  *)
    printf 'unknown ACTION %s\n' "${ACTION}" >&2
    exit 2
    ;;
esac
