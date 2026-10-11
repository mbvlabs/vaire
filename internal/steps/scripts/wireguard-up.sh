#!/usr/bin/env bash
# Step 3 (wireguard-v1), part 2: open the WireGuard port and bring wg0 up with
# the rendered config. A running interface is updated in place with syncconf so
# connected operators keep their tunnel.
set -euo pipefail

: "${WG_LISTEN_PORT:?WG_LISTEN_PORT is required}"

ufw allow "${WG_LISTEN_PORT}/udp" >/dev/null
systemctl enable wg-quick@wg0 >/dev/null 2>&1
if wg show wg0 >/dev/null 2>&1; then
  wg syncconf wg0 <(wg-quick strip wg0)
  printf 'updated wg0 in place\n'
else
  systemctl restart wg-quick@wg0
  printf 'started wg0\n'
fi
wg show wg0 | grep -v 'private key' || true
