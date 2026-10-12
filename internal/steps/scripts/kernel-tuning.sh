#!/usr/bin/env bash
# Step 7 (kernel-tuning-v1): apply the sysctl drop-in and disable transparent
# huge pages. The files are written by Go; this only reloads the settings.
set -euo pipefail

sysctl --system >/dev/null

systemctl daemon-reload
systemctl enable --now eru-thp.service

printf 'kernel tuning applied\n'
