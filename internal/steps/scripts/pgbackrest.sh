#!/usr/bin/env bash
# Step 14 (pgbackrest-v1): the directories pgBackRest needs, owned by
# postgres. eru writes pgbackrest.conf and backup.env itself, then runs
# stanza-create, check and the first full backup as postgres.
set -euo pipefail

: "${LOCAL_REPO:?LOCAL_REPO is required}"

install -d -o root -g root -m 0755 /etc/pgbackrest
install -d -o postgres -g postgres -m 0750 /var/spool/pgbackrest
install -d -o postgres -g postgres -m 0770 /var/log/pgbackrest
if [ "${LOCAL_REPO}" = "true" ]; then
  install -d -o postgres -g postgres -m 0750 /var/lib/pgbackrest
fi
printf 'pgbackrest directories ready\n'
