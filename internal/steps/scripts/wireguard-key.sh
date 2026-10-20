#!/usr/bin/env bash
# Step 3 (wireguard-v1), part 1: make sure the node has a WireGuard key pair.
# vaire renders wg0.conf from it afterwards.
set -euo pipefail

: "${WG_PRIVATE_KEY_PATH:?WG_PRIVATE_KEY_PATH is required}"
: "${WG_PUBLIC_KEY_PATH:?WG_PUBLIC_KEY_PATH is required}"

install -d -m 0700 "$(dirname "${WG_PRIVATE_KEY_PATH}")"
umask 077
if [ ! -s "${WG_PRIVATE_KEY_PATH}" ]; then
  tmp="$(mktemp "${WG_PRIVATE_KEY_PATH}.XXXXXX")"
  wg genkey > "${tmp}"
  chmod 0600 "${tmp}"
  mv -f "${tmp}" "${WG_PRIVATE_KEY_PATH}"
  printf 'generated the node WireGuard key\n'
fi
tmp="$(mktemp "${WG_PUBLIC_KEY_PATH}.XXXXXX")"
wg pubkey < "${WG_PRIVATE_KEY_PATH}" > "${tmp}"
chmod 0600 "${tmp}"
mv -f "${tmp}" "${WG_PUBLIC_KEY_PATH}"
