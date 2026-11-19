#!/usr/bin/env bash
# Classify a version before a workflow job selects its GitHub environment.
set -euo pipefail

version="${1:?expected a release version without v}"
number='(0|[1-9][0-9]*)'
if [[ ! "${version}" =~ ^${number}\.${number}\.${number}(-(alpha|beta|rc)\.${number})?$ ]]; then
  echo "invalid tagged release version" >&2
  exit 1
fi

case "${version}" in
  *-alpha.* | *-beta.*) channel=edge; environment=prerelease ;;
  *-rc.*) channel=rc; environment=prerelease ;;
  *) channel="stable-${version%%.*}.x"; environment=stable ;;
esac

printf 'channel=%s\nenvironment=%s\n' "${channel}" "${environment}"
