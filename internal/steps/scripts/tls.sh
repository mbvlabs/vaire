#!/usr/bin/env bash
# Step 10 (tls-v1). PHASE=install: refuse a Caddy box, install nginx with the
# stream module, give postgres and pgbouncer the ssl-cert group, and place a
# placeholder certificate so PostgreSQL, PgBouncer and nginx can start.
# PHASE=start (after vaire wrote nginx.conf and vaire-certs.service): validate and
# (re)start nginx and start issuance in the background. TLS never blocks on
# ACME.
set -euo pipefail

: "${NGINX_MIN_VERSION:?NGINX_MIN_VERSION is required}"
: "${TLS_HOSTNAME:?TLS_HOSTNAME is required}"
: "${ACME_WEBROOT:?ACME_WEBROOT is required}"
: "${PHASE:?PHASE is required}"

if [ "${PHASE}" = "install" ]; then
  if dpkg-query -W -f='${Status}' caddy 2>/dev/null | grep -q 'install ok installed'; then
    printf 'caddy is installed: this server was set up by an older vaire that used Caddy.\n' >&2
    printf 'vaire now serves TLS with nginx and does not migrate. Rebuild the server and run server init again.\n' >&2
    exit 1
  fi

  DEBIAN_FRONTEND=noninteractive apt-get -o dpkg::lock::timeout=300 install -y --no-install-recommends \
    nginx libnginx-mod-stream ssl-cert
  version="$(dpkg-query -W -f='${Version}' nginx)"
  if ! dpkg --compare-versions "${version}" ge "${NGINX_MIN_VERSION}"; then
    printf 'nginx %s is older than %s\n' "${version}" "${NGINX_MIN_VERSION}" >&2
    exit 1
  fi
  apt-mark hold nginx libnginx-mod-stream >/dev/null
  # vaire's nginx.conf defines every server; the distribution's default site
  # would also listen on 80.
  rm -f /etc/nginx/sites-enabled/default

  id -u postgres >/dev/null 2>&1 && usermod -aG ssl-cert postgres
  id -u pgbouncer >/dev/null 2>&1 && usermod -aG ssl-cert pgbouncer

  install -d -m 0755 /etc/vaire/tls
  install -d -m 0700 /etc/vaire/acme
  install -d -m 0755 "${ACME_WEBROOT}" "${ACME_WEBROOT}/.well-known" "${ACME_WEBROOT}/.well-known/acme-challenge"
  if [ ! -f /etc/vaire/tls/server.crt ]; then
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
      -keyout /etc/vaire/tls/server.key -out /etc/vaire/tls/server.crt \
      -subj "/CN=${TLS_HOSTNAME}" -addext "subjectAltName=DNS:${TLS_HOSTNAME}"
  fi
  chown root:root /etc/vaire/tls
  chown root:ssl-cert /etc/vaire/tls/server.key
  chmod 0640 /etc/vaire/tls/server.key
  chown root:root /etc/vaire/tls/server.crt
  chmod 0644 /etc/vaire/tls/server.crt
  printf 'nginx %s installed\n' "${version}"
  exit 0
fi

nginx -t
systemctl daemon-reload
systemctl --quiet enable nginx
systemctl reload-or-restart nginx
# timers-v1 installs and enables the hourly vaire-certs.timer; issuance starts
# here so the certificate doesn't wait for it.
systemctl start --no-block vaire-certs.service
printf 'nginx started; certificate issuance runs in the background\n'
