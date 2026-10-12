#!/usr/bin/env bash
# Apply the sysctl drop-in without touching systemd units.
set -euo pipefail

sysctl --system >/dev/null
printf 'sysctl applied\n'
