#!/usr/bin/env bash
# Add, then delete, ufw rules given one rule spec per line in "ufw show added"
# form. Adding first means an SSH allow rule is in place before another one is
# removed. Used by ssh.lockdown, ssh.confirm, ssh-revert and unlock-ssh.
set -euo pipefail

: "${UFW_ADD:=}"
: "${UFW_DELETE:=}"

if [ -n "${UFW_ADD}" ]; then
  while IFS= read -r rule; do
    [ -n "${rule}" ] || continue
    read -r -a args <<< "${rule}"
    ufw "${args[@]}" >/dev/null
    printf 'allowed: %s\n' "${rule}"
  done <<< "${UFW_ADD}"
fi

if [ -n "${UFW_DELETE}" ]; then
  while IFS= read -r rule; do
    [ -n "${rule}" ] || continue
    read -r -a args <<< "${rule}"
    if ufw delete "${args[@]}" >/dev/null 2>&1; then
      printf 'removed: %s\n' "${rule}"
    fi
  done <<< "${UFW_DELETE}"
fi

if ufw status | grep -q '^Status: active'; then
  ufw reload >/dev/null
fi
