#!/usr/bin/env bash
set -euo pipefail

release_root="${1:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
channel="${2:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
bucket="${3:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
endpoint="${4:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"

case "${channel}" in
  stable | edge) ;;
  *)
    echo "channel must be stable or edge" >&2
    exit 1
    ;;
esac

if [ "${channel}" = edge ]; then
  channel_dir="${release_root}/edge"
  dest_prefix="s3://${bucket}/edge"
else
  channel_dir="${release_root}"
  dest_prefix="s3://${bucket}"
fi

[ -d "${release_root}/releases" ] || {
  echo "release tree is missing ${release_root}/releases" >&2
  exit 1
}
for name in install.sh manifest.json manifest.json.sigstore.json; do
  [ -f "${channel_dir}/${name}" ] || {
    echo "release tree is missing ${channel_dir}/${name}" >&2
    exit 1
  }
done

aws s3 cp "${release_root}/releases" "s3://${bucket}/releases" --recursive \
  --cache-control 'public,max-age=31536000,immutable' --endpoint-url "${endpoint}"
aws s3 cp "${channel_dir}/install.sh" "${dest_prefix}/install.sh" \
  --cache-control no-cache --content-type text/x-shellscript --endpoint-url "${endpoint}"
aws s3 cp "${channel_dir}/manifest.json.sigstore.json" \
  "${dest_prefix}/manifest.json.sigstore.json" \
  --content-type application/json --cache-control no-cache \
  --endpoint-url "${endpoint}"
aws s3 cp "${channel_dir}/manifest.json" "${dest_prefix}/manifest.json" \
  --content-type application/json --cache-control no-cache \
  --endpoint-url "${endpoint}"
