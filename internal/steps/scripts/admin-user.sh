#!/usr/bin/env bash
# Step 2 (admin-user-v1): create the admin user, install the operators' SSH
# keys, set the sudo password when one is given, and install the validated
# sudoers drop-in that lets the admin user run eru node commands as root.
set -euo pipefail

: "${ADMIN_USER:?ADMIN_USER is required}"
: "${AUTHORIZED_KEYS:?AUTHORIZED_KEYS is required}"
: "${SUDOERS:?SUDOERS is required}"
: "${SUDOERS_PATH:?SUDOERS_PATH is required}"
: "${SUDO_PASSWORD:=}"

if ! id "${ADMIN_USER}" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "${ADMIN_USER}"
  printf 'created user %s\n' "${ADMIN_USER}"
fi
usermod --shell /bin/bash "${ADMIN_USER}"
usermod --append --groups sudo "${ADMIN_USER}"

if [ -n "${SUDO_PASSWORD}" ]; then
  printf '%s:%s\n' "${ADMIN_USER}" "${SUDO_PASSWORD}" | chpasswd
  printf 'set the sudo password for %s\n' "${ADMIN_USER}"
fi

home="$(getent passwd "${ADMIN_USER}" | cut -d: -f6)"
[ -n "${home}" ] || { printf 'no home directory for %s\n' "${ADMIN_USER}" >&2; exit 1; }
install -d -m 0700 -o "${ADMIN_USER}" -g "${ADMIN_USER}" "${home}/.ssh"
keys_tmp="$(mktemp "${home}/.ssh/.authorized_keys.XXXXXX")"
trap 'rm -f "${keys_tmp}"' EXIT
printf '%s' "${AUTHORIZED_KEYS}" > "${keys_tmp}"
chown "${ADMIN_USER}:${ADMIN_USER}" "${keys_tmp}"
chmod 0600 "${keys_tmp}"
mv -f "${keys_tmp}" "${home}/.ssh/authorized_keys"

sudoers_tmp="$(mktemp /etc/sudoers.d/.eru.XXXXXX)"
trap 'rm -f "${keys_tmp}" "${sudoers_tmp}"' EXIT
printf '%s' "${SUDOERS}" > "${sudoers_tmp}"
chmod 0440 "${sudoers_tmp}"
visudo -cf "${sudoers_tmp}" >/dev/null
mv -f "${sudoers_tmp}" "${SUDOERS_PATH}"
printf 'installed %s\n' "${SUDOERS_PATH}"
