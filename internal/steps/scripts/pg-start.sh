#!/usr/bin/env bash
# Start the 18/main cluster after a restart-requiring change.
set -euo pipefail

systemctl start postgresql@18-main.service 2>/dev/null || pg_ctlcluster 18 main start
printf 'postgresql started\n'
