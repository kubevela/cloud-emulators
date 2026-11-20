#!/usr/bin/env bash
# Download AWS Emulator's packages and build the image from them.
#
#   aws/build.sh fetch            download both packages; print the version they hold
#   aws/build.sh local [TAG]      fetch, then build an image for this machine (default
#                                 tag aws-emulator:local)
#
# AWS serves only the newest package (LTS/latest), so the version is whatever is there
# now. The publish workflow runs `fetch` and builds both architectures itself.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(dirname "$HERE")
BASE=${AWS_EMULATOR_BASE_URL:-https://downloads.aws.com/aws-emulator/packages/aws-emulator/LTS/latest}
INFO=$HERE/release-info

fetch() {
  curl -fsSL --proto '=https' --tlsv1.2 -o "$HERE/aws-emulator-amd64.rpm" "$BASE/aws-emulator-x86_64.rpm"
  curl -fsSL --proto '=https' --tlsv1.2 -o "$HERE/aws-emulator-arm64.rpm" "$BASE/aws-emulator-aarch64.rpm"
}

# The version is in the package header; rpm reads it without installing anything.
version_of() {
  if command -v rpm >/dev/null 2>&1; then
    rpm -qp --queryformat '%{VERSION}' "$1" 2>/dev/null
  elif command -v python3 >/dev/null 2>&1; then
    python3 "$HERE/rpm_version.py" "$1"
  else
    docker run --rm -v "$HERE:/w:ro" public.ecr.aws/amazonlinux/amazonlinux:2023-minimal \
      rpm -qp --queryformat '%{VERSION}' "/w/$(basename "$1")" 2>/dev/null
  fi
}

versions() {
  local a b
  a=$(version_of "$HERE/aws-emulator-amd64.rpm")
  b=$(version_of "$HERE/aws-emulator-arm64.rpm")
  if [[ -z "$a" || "$a" != "$b" ]]; then
    echo "the two packages disagree on the version (amd64 '$a', arm64 '$b'); AWS may be mid-release, try again" >&2
    exit 1
  fi
  [[ "$a" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "unexpected version '$a' in the package" >&2; exit 1; }
  echo "$a"
}

# What goes into the image beside the emulator: the packages' checksums (AWS does not
# sign them, so this records exactly what was packaged) and the fidelity report AWS
# published for that release, when there is one.
release_info() {
  rm -rf "$INFO"
  mkdir -p "$INFO"
  (cd "$HERE" && sha256sum aws-emulator-amd64.rpm aws-emulator-arm64.rpm) >"$INFO/SHA256SUMS"
  echo "$1" >"$INFO/VERSION"
  curl -fsSL --proto '=https' --tlsv1.2 -o "$INFO/scores.json.gz" \
    "https://github.com/aws/aws-emulator/releases/download/v$1/scores.json.gz" 2>/dev/null ||
    { rm -f "$INFO/scores.json.gz"; echo "no scores.json.gz for v$1; the image is built without it" >&2; }
}

case "${1:-}" in
  fetch)
    fetch
    v=$(versions)
    release_info "$v"
    echo "$v"
    ;;
  local)
    fetch
    v=$(versions)
    release_info "$v"
    docker build -f "$HERE/Dockerfile" --build-arg "VERSION=$v" -t "${2:-aws-emulator:local}" "$ROOT"
    echo "built ${2:-aws-emulator:local} from AWS Emulator $v"
    ;;
  *)
    sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
