#!/usr/bin/env bash
set -euo pipefail

channel_root="@@CHANNEL_ROOT@@"
release_channel="@@CHANNEL@@"
cosign_version="3.1.3"

fail() { printf 'vaire installer: %s\n' "$1" >&2; exit 1; }

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

json_string() {
  sed -n "s/.*\"${1}\": \"\\([^\"]*\\)\".*/\\1/p" "$2" | head -1
}

print_path_hint() {
  if [ "$2" = fish ]; then
    printf '  fish_add_path %s\n' "$1"
    return
  fi
  # The printed line is for the user to run; $PATH must stay literal.
  # shellcheck disable=SC2016
  printf '  export PATH="%s:$PATH"\n' "$1"
}

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac

case "${os}-${arch}" in
  linux-amd64) cosign_sha256="4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71" ;;
  linux-arm64) cosign_sha256="c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a" ;;
  darwin-amd64) cosign_sha256="2347488e5d5b25336644024dfeca5601b190e91197a71a917bda44744aff106c" ;;
  darwin-arm64) cosign_sha256="5cf948c2f4dfe59687bdd0b8523709067383e03982cc543475c8a7dc70e92a76" ;;
  *) fail "unsupported platform ${os}/${arch}" ;;
esac

case "${channel_root}" in
  https://*) ;;
  *) fail "release base URL must use HTTPS" ;;
esac

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

curl -fsSL --proto '=https' --tlsv1.2 \
  "https://github.com/sigstore/cosign/releases/download/v${cosign_version}/cosign-${os}-${arch}" \
  -o "${work_dir}/cosign"
chmod 0755 "${work_dir}/cosign"
got="$(sha256_hex "${work_dir}/cosign")"
[ "${got}" = "${cosign_sha256}" ] || fail "cosign checksum verification failed"

curl -fsSL --proto '=https' --tlsv1.2 \
  "${channel_root}/manifest.json" -o "${work_dir}/manifest.json"
curl -fsSL --proto '=https' --tlsv1.2 \
  "${channel_root}/manifest.json.sigstore.json" -o "${work_dir}/manifest.json.sigstore.json"

# Trust args are filled by scripts/build-release.sh (the channel's identity regexp).
# shellcheck disable=SC2086
if ! verify_out="$("${work_dir}/cosign" verify-blob "${work_dir}/manifest.json" \
  --bundle "${work_dir}/manifest.json.sigstore.json" \
  @@COSIGN_TRUST_ARGS@@ 2>&1)"; then
  fail "release manifest signature verification failed: ${verify_out}"
fi

manifest_channel="$(json_string channel "${work_dir}/manifest.json")"
[ "${manifest_channel}" = "${release_channel}" ] || fail "release manifest channel is invalid"
manifest_version="$(json_string version "${work_dir}/manifest.json")"
release_base_url="$(json_string releaseBaseUrl "${work_dir}/manifest.json")"
# The site origin holds /releases/<version>/ and every channel directory
# (the root, /edge and each major line such as /1.x): validate the manifest's
# release base against it, so a line's install.sh works too.
origin="${release_base_url%/releases/*}"
case "${release_base_url}" in
  "${origin}"/releases/*) ;;
  *) fail "release manifest is invalid" ;;
esac
case "${channel_root}" in
  "${origin}" | "${origin}"/*) ;;
  *) fail "release manifest is invalid" ;;
esac

manifest_sha256="$(awk -v key="\"${os}/${arch}\"" '
  index($0, key) { found = 1 }
  found && match($0, /"sha256": "[0-9a-f]{64}"/) {
    print substr($0, RSTART + 11, 64)
    exit
  }
' "${work_dir}/manifest.json")"
[ -n "${manifest_sha256}" ] || fail "release manifest is missing ${os}/${arch}"

artifact_url="${release_base_url}/${os}/${arch}/vaire"
curl -fsSL --proto '=https' --tlsv1.2 "${artifact_url}" -o "${work_dir}/vaire"
curl -fsSL --proto '=https' --tlsv1.2 \
  "${artifact_url}.sigstore.json" -o "${work_dir}/vaire.sigstore.json"
# shellcheck disable=SC2086
if ! verify_out="$("${work_dir}/cosign" verify-blob "${work_dir}/vaire" \
  --bundle "${work_dir}/vaire.sigstore.json" \
  @@COSIGN_TRUST_ARGS@@ 2>&1)"; then
  fail "vaire signature verification failed: ${verify_out}"
fi
got="$(sha256_hex "${work_dir}/vaire")"
[ "${got}" = "${manifest_sha256}" ] || fail "checksum verification failed"
chmod 0755 "${work_dir}/vaire"

smoke="$("${work_dir}/vaire" version --output json)" || fail "downloaded vaire failed to run"
smoke_version="$(printf '%s\n' "${smoke}" | sed -n 's/.*"version": "\([^"]*\)".*/\1/p' | head -1)"
[ "${smoke_version}" = "${manifest_version}" ] || fail "downloaded vaire reported ${smoke_version}, expected ${manifest_version}"

if [ "$(id -u)" -eq 0 ]; then
  dest_dir="/usr/local/bin"
else
  dest_dir="${VAIRE_INSTALL_DIR:-$HOME/.local/bin}"
  mkdir -p "${dest_dir}"
fi
dest="${dest_dir}/vaire"
tmp_dest="${dest_dir}/.vaire-new.$$"
cp "${work_dir}/vaire" "${tmp_dest}"
chmod 0755 "${tmp_dest}"
mv "${tmp_dest}" "${dest}"

printf 'vaire %s installed to %s\n' "${manifest_version}" "${dest}"

case ":${PATH}:" in
  *":${dest_dir}:"*) ;;
  *)
    shell_name="${SHELL##*/}"
    [ -n "${shell_name}" ] || shell_name=bash
    printf '\n%s is not on PATH. Add it with:\n' "${dest_dir}"
    print_path_hint "${dest_dir}" "${shell_name}"
    ;;
esac

if [ "${VAIRE_INSTALL_ONLY:-0}" = 1 ]; then
  exit 0
fi

# A supported server release, where vaire server init --local can run.
supported_server() {
  [ "${os}" = linux ] && [ -r /etc/os-release ] || return 1
  # shellcheck disable=SC1091
  case "$(. /etc/os-release && printf '%s-%s' "${ID:-}" "${VERSION_ID:-}")" in
    debian-13 | ubuntu-26.04) return 0 ;;
  esac
  return 1
}

# curl | sudo bash feeds this script on stdin, so the question goes to the
# terminal directly.
if [ "$(id -u)" -eq 0 ] && supported_server && { : </dev/tty; } 2>/dev/null; then
  printf '\nInitialise this server now (vaire server init --local)? [y/N] ' >/dev/tty
  answer=""
  read -r answer </dev/tty || answer=""
  case "${answer}" in
    y | Y | yes | YES)
      rm -rf "${work_dir}"
      exec "${dest}" server init --local </dev/tty
      ;;
  esac
  printf '\nNext: vaire server init --local, or vaire --help\n'
  exit 0
fi
printf '\nNext: vaire --help\n'
