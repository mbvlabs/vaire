#!/usr/bin/env bash
# postgres-config-v1@<cluster>: reload the cluster with the rendered files.
# The Go step writes them owned by postgres:postgres with the right modes
# before this runs, so there is nothing to fix up here.
set -euo pipefail

: "${CLUSTER:?CLUSTER is required}"

pg_ctlcluster 18 "${CLUSTER}" reload
printf 'configuration of %s reloaded\n' "${CLUSTER}"
