#!/usr/bin/env bash
# Install or remove an sshd drop-in, validate the whole config with sshd -t,
# and reload sshd. A config that fails validation is rolled back. On Ubuntu,
# socket activation is replaced by ssh.service so the config applies (§8.3).
# Used by sshd-hardening-v1, ssh.lockdown, ssh-revert and unlock-ssh.
set -euo pipefail

: "${SSHD_DROPIN_PATH:?SSHD_DROPIN_PATH is required}"
: "${SSHD_DROPIN_ACTION:?SSHD_DROPIN_ACTION is required}"
: "${SSHD_DROPIN:=}"
: "${SSHD_EXPECT:=}"

install -d -m 0755 /etc/ssh/sshd_config.d /run/sshd

backup=""
cleanup() { [ -z "${backup}" ] || rm -f "${backup}"; }
trap cleanup EXIT
if [ -f "${SSHD_DROPIN_PATH}" ]; then
  backup="$(mktemp)"
  cp -p "${SSHD_DROPIN_PATH}" "${backup}"
fi

restore() {
  if [ -n "${backup}" ]; then
    cp -p "${backup}" "${SSHD_DROPIN_PATH}"
  else
    rm -f "${SSHD_DROPIN_PATH}"
  fi
}

case "${SSHD_DROPIN_ACTION}" in
  write)
    tmp="$(mktemp "${SSHD_DROPIN_PATH}.XXXXXX")"
    printf '%s' "${SSHD_DROPIN}" > "${tmp}"
    chmod 0644 "${tmp}"
    mv -f "${tmp}" "${SSHD_DROPIN_PATH}"
    ;;
  remove)
    rm -f "${SSHD_DROPIN_PATH}"
    ;;
  *)
    printf 'unknown SSHD_DROPIN_ACTION %s\n' "${SSHD_DROPIN_ACTION}" >&2
    exit 2
    ;;
esac

if ! sshd -t; then
  restore
  printf 'sshd -t rejected the config; restored the previous drop-in\n' >&2
  exit 1
fi

if [ -n "${SSHD_EXPECT}" ]; then
  effective="$(sshd -T 2>/dev/null)"
  while IFS= read -r expected; do
    [ -n "${expected}" ] || continue
    if ! printf '%s\n' "${effective}" | grep -qxF "${expected}"; then
      restore
      printf 'another sshd drop-in overrides "%s"; restored the previous drop-in\n' "${expected}" >&2
      exit 1
    fi
  done <<< "${SSHD_EXPECT}"
fi

if [ "$(systemctl is-enabled ssh.socket 2>/dev/null || true)" = enabled ] || systemctl is-active --quiet ssh.socket 2>/dev/null; then
  systemctl disable --now ssh.socket
  rm -f /etc/systemd/system/ssh.service.d/00-socket.conf /etc/systemd/system/ssh.socket.d/addresses.conf
  systemctl daemon-reload
  systemctl enable ssh.service >/dev/null 2>&1
  systemctl restart ssh.service
  printf 'replaced ssh.socket activation with ssh.service\n'
else
  systemctl enable ssh.service >/dev/null 2>&1 || true
  systemctl reload-or-restart ssh.service
fi
printf 'sshd reloaded with %s (%s)\n' "${SSHD_DROPIN_PATH}" "${SSHD_DROPIN_ACTION}"
