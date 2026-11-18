#!/usr/bin/env bash
set -euo pipefail

release_root="${1:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
channel="${2:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
bucket="${3:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
endpoint="${4:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
mode="${5:-all}"
case "${mode}" in
  all | artifacts | channel) ;;
  *) echo "upload mode must be all, artifacts or channel" >&2; exit 1 ;;
esac

case "${channel}" in
  stable)
    channel_dir="${release_root}"
    dest_prefix="s3://${bucket}"
    ;;
  edge)
    channel_dir="${release_root}/edge"
    dest_prefix="s3://${bucket}/edge"
    ;;
  rc)
    channel_dir="${release_root}/rc"
    dest_prefix="s3://${bucket}/rc"
    ;;
  stable-[0-9]*.x)
    line="${channel#stable-}"
    channel_dir="${release_root}/${line}"
    dest_prefix="s3://${bucket}/${line}"
    ;;
  *)
    echo "channel must be stable, stable-<major>.x (for example stable-1.x), rc or edge" >&2
    exit 1
    ;;
esac

manifest_value() {
  sed -n "s/.*\"${1}\": *\"\\([^\"]*\\)\".*/\\1/p" "$2" | head -1
}

# The moving channel pointer must only receive a release that belongs to it.
guard_channel_manifest() {
  local manifest="$1" version manifest_channel want_channel
  version="$(manifest_value version "${manifest}")"
  manifest_channel="$(manifest_value channel "${manifest}")"
  [ -n "${version}" ] || { echo "channel manifest has no version" >&2; exit 1; }
  case "${channel}" in
    stable | stable-[0-9]*.x)
      want_channel=stable
      [[ "${version}" != *-* ]] || {
        echo "refusing to publish prerelease ${version} as a stable pointer" >&2
        exit 1
      }
      if [[ "${channel}" == stable-*.x ]]; then
        line="${channel#stable-}"
        line="${line%.x}"
        [ "${version%%.*}" = "${line}" ] || {
          echo "refusing to publish ${version} on the ${channel} pointer" >&2
          exit 1
        }
      fi
      ;;
    rc)
      want_channel=rc
      [[ "${version}" =~ -rc\.[0-9]+$ ]] || {
        echo "refusing to publish ${version} as the rc pointer" >&2
        exit 1
      }
      ;;
    edge)
      want_channel=edge
      [[ "${version}" =~ ^edge-[0-9a-f]{12}$ || "${version}" =~ -(alpha|beta)\.[0-9]+$ ]] || {
        echo "refusing to publish ${version} as the edge pointer" >&2
        exit 1
      }
      ;;
  esac
  [ "${manifest_channel}" = "${want_channel}" ] || {
    echo "channel manifest names ${manifest_channel}, expected ${want_channel}" >&2
    exit 1
  }
}

[ -d "${release_root}/releases" ] || {
  echo "release tree is missing ${release_root}/releases" >&2
  exit 1
}
if [ "${mode}" != artifacts ]; then
  for name in install.sh manifest.json manifest.json.sigstore.json; do
    [ -f "${channel_dir}/${name}" ] || {
      echo "release tree is missing ${channel_dir}/${name}" >&2
      exit 1
    }
  done
  guard_channel_manifest "${channel_dir}/manifest.json"
fi

if [ "${mode}" != channel ]; then
  aws s3 cp "${release_root}/releases" "s3://${bucket}/releases" --recursive \
    --cache-control 'public,max-age=31536000,immutable' --endpoint-url "${endpoint}"
fi
if [ "${mode}" = artifacts ]; then
  exit 0
fi
aws s3 cp "${channel_dir}/install.sh" "${dest_prefix}/install.sh" \
  --cache-control no-cache --content-type text/x-shellscript --endpoint-url "${endpoint}"
aws s3 cp "${channel_dir}/manifest.json.sigstore.json" \
  "${dest_prefix}/manifest.json.sigstore.json" \
  --content-type application/json --cache-control no-cache \
  --endpoint-url "${endpoint}"
aws s3 cp "${channel_dir}/manifest.json" "${dest_prefix}/manifest.json" \
  --content-type application/json --cache-control no-cache \
  --endpoint-url "${endpoint}"
