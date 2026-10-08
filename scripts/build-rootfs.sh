#!/usr/bin/env bash
#
# Build the VyOS root filesystem tarball the Dockerfile adds to the image.
#
#   1. download the pinned nightly ISO and its minisign signature
#   2. verify the signature against the key committed in keys/
#   3. convert the ISO with upstream's scripts/iso-to-oci
#
# The ISO is cached in .cache/, the result lands in build/rootfs.tar.xz.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VYOS_VERSION:-$(tr -d '[:space:]' < "${ROOT}/VYOS_VERSION")}"
ARCH="${VYOS_ARCH:-amd64}"
CACHE_DIR="${CACHE_DIR:-${ROOT}/.cache}"
BUILD_DIR="${BUILD_DIR:-${ROOT}/build}"
PUBKEY="${ROOT}/keys/vyos-nightly.pub"

# Pinned so that a local run and a CI run verify with the same code.
MINISIGN_GO="aead.dev/minisign/cmd/minisign@v0.3.0"

ISO="vyos-${VERSION}-generic-${ARCH}.iso"
BASE_URL="https://github.com/vyos/vyos-nightly-build/releases/download/${VERSION}"

mkdir -p "${CACHE_DIR}" "${BUILD_DIR}"

fetch() {
  local name="$1"
  if [[ -s "${CACHE_DIR}/${name}" ]]; then
    echo "I: using cached ${name}"
    return
  fi
  echo "I: downloading ${name}"
  curl --fail --silent --show-error --location --retry 3 \
    --output "${CACHE_DIR}/${name}.part" "${BASE_URL}/${name}"
  mv "${CACHE_DIR}/${name}.part" "${CACHE_DIR}/${name}"
}

verify() {
  echo "I: verifying signature of ${ISO}"
  if command -v minisign >/dev/null 2>&1; then
    minisign -V -p "${PUBKEY}" -m "${CACHE_DIR}/${ISO}"
  elif command -v go >/dev/null 2>&1; then
    go run "${MINISIGN_GO}" -V -p "${PUBKEY}" -m "${CACHE_DIR}/${ISO}"
  else
    echo "E: neither minisign nor go is available to verify the ISO" >&2
    exit 2
  fi
}

fetch "${ISO}.minisig"
fetch "${ISO}"
verify

# iso-to-oci writes vyos-<version>-oci-<arch>.tar.xz into its working
# directory, where <version> is read from the ISO and not from our pin.
workdir="$(mktemp -d "${BUILD_DIR}/rootfs.XXXXXX")"
trap 'rm -rf "${workdir}"' EXIT

# The tarball must record the ownership stored in the squashfs. Unpacked as a
# regular user every file would belong to that user, and VyOS then refuses to
# commit anything because sudo rejects a sudoers file it does not own.
if [[ "${EUID}" -eq 0 ]]; then
  as_root=()
elif command -v fakeroot >/dev/null 2>&1; then
  as_root=(fakeroot --)
else
  echo "E: run as root or install fakeroot, file ownership must be preserved" >&2
  exit 2
fi
(cd "${workdir}" && "${as_root[@]}" "${ROOT}/scripts/iso-to-oci" "${CACHE_DIR}/${ISO}")

shopt -s nullglob
tarballs=("${workdir}"/vyos-*-oci-*.tar.xz)
if [[ "${#tarballs[@]}" -ne 1 ]]; then
  echo "E: expected exactly one rootfs tarball, found ${#tarballs[@]}" >&2
  exit 1
fi
mv "${tarballs[0]}" "${BUILD_DIR}/rootfs.tar.xz"
echo "I: wrote ${BUILD_DIR}/rootfs.tar.xz"
