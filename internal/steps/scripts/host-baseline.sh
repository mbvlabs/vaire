#!/usr/bin/env bash
# Step 6 (host-baseline-v1): apply the rendered journald, unattended-upgrades,
# fail2ban and logrotate files, set the timezone, enable chrony, and create a
# swap file of SWAP_MB when the host has no swap (0 = no swap file).
set -euo pipefail

: "${SWAP_MB:?SWAP_MB is required}"
: "${TIMEZONE:?TIMEZONE is required}"

install -d -m 2755 /var/log/journal
systemctl restart systemd-journald

if [ "$(timedatectl show -p Timezone --value 2>/dev/null || true)" != "${TIMEZONE}" ]; then
  timedatectl set-timezone "${TIMEZONE}"
  printf 'timezone set to %s\n' "${TIMEZONE}"
fi

systemctl enable --now chrony >/dev/null 2>&1
systemctl enable --now unattended-upgrades >/dev/null 2>&1 || true
systemctl enable fail2ban >/dev/null 2>&1
systemctl restart fail2ban

if [ "${SWAP_MB}" -gt 0 ]; then
  if swapon --noheadings --raw 2>/dev/null | grep -q .; then
    printf 'swap is already active\n'
  else
    if [ ! -f /swapfile ]; then
      if ! fallocate -l "${SWAP_MB}M" /swapfile; then
        dd if=/dev/zero of=/swapfile bs=1M count="${SWAP_MB}" status=none
      fi
    fi
    chmod 0600 /swapfile
    mkswap /swapfile >/dev/null
    swapon /swapfile
    printf 'created a %s MiB swap file\n' "${SWAP_MB}"
  fi
  if swapon --noheadings --raw 2>/dev/null | awk '{print $1}' | grep -Fxq /swapfile \
    && ! grep -q '^/swapfile[[:space:]]' /etc/fstab; then
    printf '/swapfile none swap sw 0 0\n' >> /etc/fstab
  fi
fi
