#!/usr/bin/env bash
# Step 12 (tls-v1): install pinned Caddy, place a placeholder certificate so
# PostgreSQL and PgBouncer can start, validate the Caddyfile and start Caddy,
# then enable the certs-sync units.
set -euo pipefail

: "${CADDY_VERSION:?CADDY_VERSION is required}"
: "${CADDY_SHA256:?CADDY_SHA256 is required}"
: "${DOMAIN:?DOMAIN is required}"

architecture="$(dpkg --print-architecture)"

installed_version=""
if command -v caddy >/dev/null 2>&1; then
  installed_version="$(caddy version | awk '{print $1}')"
fi

if [ "${installed_version}" != "v${CADDY_VERSION}" ]; then
  package="caddy_${CADDY_VERSION}_linux_${architecture}.deb"
  url="https://github.com/caddyserver/caddy/releases/download/v${CADDY_VERSION}/${package}"
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir}"' EXIT
  curl --fail --silent --show-error --location --retry 3 --output "${tmpdir}/${package}" "${url}"
  printf '%s  %s\n' "${CADDY_SHA256}" "${tmpdir}/${package}" | sha256sum --check --status
  DEBIAN_FRONTEND=noninteractive apt-get -o dpkg::lock::timeout=300 install -y --allow-downgrades --allow-change-held-packages "${tmpdir}/${package}"
fi
apt-mark hold caddy >/dev/null

# The ssl-cert group lets postgres and pgbouncer read the private key.
if ! getent group ssl-cert >/dev/null; then
  DEBIAN_FRONTEND=noninteractive apt-get -o dpkg::lock::timeout=300 install -y ssl-cert
fi
id -u postgres >/dev/null 2>&1 && usermod -aG ssl-cert postgres
id -u pgbouncer >/dev/null 2>&1 && usermod -aG ssl-cert pgbouncer

install -d -m 0755 /etc/eru/tls
if [ ! -f /etc/eru/tls/server.crt ]; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
    -keyout /etc/eru/tls/server.key -out /etc/eru/tls/server.crt \
    -subj "/CN=${DOMAIN}" -addext "subjectAltName=DNS:${DOMAIN}"
fi
chown root:ssl-cert /etc/eru/tls/server.key
chmod 0640 /etc/eru/tls/server.key
chmod 0644 /etc/eru/tls/server.crt

caddy validate --config /etc/caddy/Caddyfile

systemctl daemon-reload
systemctl enable --now caddy
systemctl reload-or-restart caddy

systemctl enable --now eru-certs-sync.path eru-certs-sync.timer

printf 'caddy installed and started\n'
