#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DIST_DIR="${ROOT_DIR}/dist"

mkdir -p "${DIST_DIR}"

cd "${ROOT_DIR}"

ARCH=$(go env GOARCH)
PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig:/usr/local/lib/pkgconfig}"
export PKG_CONFIG_PATH

REMOTE_TAGS=""
if [ "${DJONEHUB_REMOTE_OPUS:-0}" = "1" ]; then
  if ! OPUS_VERSION=$(pkg-config --modversion opus); then
    echo "Remote media builds require libopus 1.5.2 or 1.6.1; install opus and check PKG_CONFIG_PATH" >&2
    exit 1
  fi
  case "${OPUS_VERSION}" in
    1.5.2|1.6.1) ;;
    *)
      echo "Unsupported libopus version ${OPUS_VERSION}; supported versions: 1.5.2, 1.6.1" >&2
      exit 1
      ;;
  esac
  echo "Remote media build using libopus ${OPUS_VERSION}"
  REMOTE_TAGS="remoteopus"
fi

# Publish fresh inodes atomically. Overwriting an executable with cp while
# macOS still caches its code signature can make the next launch get SIGKILL.
BUILD_FILE=$(mktemp "${DIST_DIR}/.djonehub-macos-${ARCH}.XXXXXX")
PUBLISH_FILE=$(mktemp "${DIST_DIR}/.djonehub-macos-publish.XXXXXX")
trap 'rm -f "$BUILD_FILE" "$PUBLISH_FILE"' EXIT HUP INT TERM
CGO_ENABLED=1 GOOS=darwin GOARCH="${ARCH}" go build \
  -p 2 -tags "${REMOTE_TAGS}" \
  -trimpath -ldflags="-s -w" \
  -o "${BUILD_FILE}" ./cmd/djonehub-macos

cp "${BUILD_FILE}" "${PUBLISH_FILE}"
chmod 755 "${BUILD_FILE}" "${PUBLISH_FILE}"
mv -f "${BUILD_FILE}" "${DIST_DIR}/djonehub-macos-${ARCH}"
mv -f "${PUBLISH_FILE}" "${DIST_DIR}/djonehub-macos"

echo "macOS binaries written to ${DIST_DIR}"
