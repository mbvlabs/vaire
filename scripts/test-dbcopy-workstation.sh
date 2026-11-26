#!/usr/bin/env bash
set -euo pipefail

# Disposable local smoke fixture. TMPDIR must be on a filesystem with room
# for a PostgreSQL cluster. The cluster uses a loopback-only fixed port.
pgbin="${1:?usage: bash scripts/test-dbcopy-workstation.sh POSTGRES_BIN_DIR [CLIENT_BIN_DIR]}"
client="${2:-/usr/lib/postgresql/18/bin}"
port=25433
work="$(mktemp -d "${TMPDIR:?set TMPDIR to a scratch directory}/vaire-copy-smoke.XXXXXXXX")"
started=0
cleanup() {
  if [ "$started" = 1 ]; then "$pgbin/pg_ctl" -D "$work/data" -m immediate -w stop >/dev/null; fi
  rm -rf "$work"
}
trap cleanup EXIT
if ss -ltn "( sport = :$port )" | grep -q LISTEN; then
  echo "fixture port $port is occupied" >&2; exit 1
fi
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost \
  -addext 'subjectAltName=DNS:localhost' -keyout "$work/server.key" -out "$work/server.crt" >/dev/null 2>&1
chmod 600 "$work/server.key"
"$pgbin/initdb" -D "$work/data" -A trust -U vaire >/dev/null
"$pgbin/pg_ctl" -D "$work/data" -l "$work/server.log" \
  -o "-c listen_addresses=127.0.0.1 -c port=$port -c unix_socket_directories=$work -c ssl=on -c ssl_cert_file=$work/server.crt -c ssl_key_file=$work/server.key -c shared_preload_libraries=pg_stat_statements" -w start >/dev/null
started=1
"$client/psql" -h "$work" -p "$port" -U vaire -d postgres -v ON_ERROR_STOP=1 <<'SQL'
CREATE ROLE target_owner NOLOGIN;
CREATE ROLE target_migrate LOGIN;
GRANT target_owner TO target_migrate;
ALTER ROLE target_migrate SET role = target_owner;
CREATE DATABASE source;
CREATE DATABASE source_serial;
CREATE DATABASE source_ext;
CREATE DATABASE target OWNER target_owner;
SQL
"$client/psql" -h "$work" -p "$port" -U vaire -d source -v ON_ERROR_STOP=1 <<'SQL'
CREATE SEQUENCE items_id_seq;
CREATE TABLE items (id integer NOT NULL, value integer NOT NULL);
INSERT INTO items VALUES (nextval('items_id_seq'), 19), (nextval('items_id_seq'), 23);
CREATE INDEX items_value_idx ON items(value);
CREATE TABLE item_values (value integer PRIMARY KEY);
INSERT INTO item_values VALUES (19), (23);
ALTER TABLE items ADD CONSTRAINT items_value_fkey FOREIGN KEY (value) REFERENCES item_values(value);
CREATE FUNCTION add_one(integer) RETURNS integer LANGUAGE SQL IMMUTABLE RETURN $1 + 1;
CREATE FUNCTION add_two(integer, integer) RETURNS integer LANGUAGE SQL IMMUTABLE RETURN $1 + $2;
SQL
"$client/psql" -h "$work" -p "$port" -U vaire -d source_serial -v ON_ERROR_STOP=1 -c 'CREATE TABLE serial_items (id serial PRIMARY KEY)' >/dev/null
"$client/psql" -h "$work" -p "$port" -U vaire -d source_ext -v ON_ERROR_STOP=1 -c 'CREATE EXTENSION pg_stat_statements' >/dev/null
"$client/psql" -h "$work" -p "$port" -U vaire -d target -v ON_ERROR_STOP=1 -c 'CREATE EXTENSION pg_stat_statements' >/dev/null
umask 0077
VAIRE_COPY_SMOKE_PORT="$port" VAIRE_COPY_SMOKE_CA="$work/server.crt" VAIRE_COPY_SMOKE_CLIENT="$client" \
  go test ./internal/cli -run '^TestWorkstationCopyRealArchiveSmoke$' -count=1 -v
"$pgbin/pg_ctl" -D "$work/data" -m fast -w stop >/dev/null
started=0
"$pgbin/pg_ctl" -D "$work/data" -l "$work/server.log" \
  -o "-c listen_addresses=127.0.0.1 -c port=$port -c unix_socket_directories=$work -c ssl=off" -w start >/dev/null
started=1
VAIRE_COPY_SMOKE_PLAINTEXT_PORT="$port" VAIRE_COPY_SMOKE_CLIENT="$client" \
  go test ./internal/cli -run '^TestWorkstationCopyPlaintextSourceSmoke$' -count=1 -v
