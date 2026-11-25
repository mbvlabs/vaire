#!/usr/bin/env bash
set -euo pipefail

# Exercise a relocated client against a disposable, local TLS-only server.
psql="${1:?usage: test-psql-tls.sh PSQL POSTGRES_BIN_DIR}"
pgbin="${2:?usage: test-psql-tls.sh PSQL POSTGRES_BIN_DIR}"
work="$(mktemp -d "${TMPDIR:-/tmp}/vaire-psql-tls.XXXXXXXX")"
started=0
cleanup() {
  if [ "$started" = 1 ]; then "$pgbin/pg_ctl" -D "$work/data" -m immediate stop >/dev/null; fi
  rm -rf "$work"
}
trap cleanup EXIT

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=vaire-test-ca \
  -keyout "$work/ca.key" -out "$work/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj /CN=localhost \
  -keyout "$work/server.key" -out "$work/server.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:localhost\nextendedKeyUsage=serverAuth\n' > "$work/ext"
openssl x509 -req -in "$work/server.csr" -CA "$work/ca.crt" -CAkey "$work/ca.key" \
  -CAcreateserial -days 1 -extfile "$work/ext" -out "$work/server.crt" >/dev/null 2>&1
chmod 600 "$work/server.key"
"$pgbin/initdb" -D "$work/data" -A trust -U vaire >/dev/null
cat >> "$work/data/postgresql.conf" <<EOF
listen_addresses = '127.0.0.1'
port = 25432
unix_socket_directories = '$work'
ssl = on
ssl_cert_file = '$work/server.crt'
ssl_key_file = '$work/server.key'
EOF
"$pgbin/pg_ctl" -D "$work/data" -l "$work/log" -w start >/dev/null
started=1
connection="hostaddr=127.0.0.1 port=25432 user=vaire dbname=postgres sslmode=verify-full sslrootcert=system"
export SSL_CERT_FILE="$work/ca.crt"
env -u LD_LIBRARY_PATH -u DYLD_LIBRARY_PATH -u DYLD_FALLBACK_LIBRARY_PATH \
  "$psql" "$connection host=localhost" -X -Atqc 'select 1' | grep -Fx 1
if env -u LD_LIBRARY_PATH -u DYLD_LIBRARY_PATH -u DYLD_FALLBACK_LIBRARY_PATH \
  "$psql" "$connection host=127.0.0.1" -X -Atqc 'select 1' >"$work/out" 2>&1; then
  echo "TLS hostname mismatch unexpectedly succeeded" >&2
  exit 1
fi
grep -Eiq 'certificate.*(does not match|not match)' "$work/out" || { cat "$work/out" >&2; exit 1; }

# A custom archive exercises both clients and their shared libpq/TLS closure.
client_dir="$(dirname "$psql")"
connection="$connection host=localhost"
"$psql" "$connection" -X -v ON_ERROR_STOP=1 -c 'create table sidecar_smoke (value integer); insert into sidecar_smoke values (42)' >/dev/null
env -u LD_LIBRARY_PATH -u DYLD_LIBRARY_PATH -u DYLD_FALLBACK_LIBRARY_PATH \
  "$client_dir/pg_dump" -Fc -f "$work/smoke.dump" "$connection"
"$psql" "$connection" -X -v ON_ERROR_STOP=1 -c 'drop table sidecar_smoke' >/dev/null
env -u LD_LIBRARY_PATH -u DYLD_LIBRARY_PATH -u DYLD_FALLBACK_LIBRARY_PATH \
  "$client_dir/pg_restore" --exit-on-error -d "$connection" "$work/smoke.dump"
"$psql" "$connection" -X -Atqc 'select value from sidecar_smoke' | grep -Fx 42
