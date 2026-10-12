#!/usr/bin/env bash
# Stop the 18/main cluster before a restart-requiring change.
set -euo pipefail

systemctl stop postgresql@18-main.service 2>/dev/null || pg_ctlcluster 18 main stop -m fast
printf 'postgresql stopped\n'
