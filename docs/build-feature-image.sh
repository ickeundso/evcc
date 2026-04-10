#!/bin/bash
set -euo pipefail

# Build evcc feature branch Docker image and push to ghcr.io
#
# Usage:
#   ./docs/build-feature-image.sh [--push]
#
# Prerequisites:
#   - Docker with buildx support
#   - GHCR_TOKEN env var or `gh auth token` for ghcr.io authentication

GHCR_IMAGE="ghcr.io/ickeundso/evcc-feature"
VERSION="0.303.2-use-ml.4"

# Determine platform - default amd64, override with PLATFORM env var
PLATFORM="${PLATFORM:-linux/amd64}"

# Derive arch suffix from platform (amd64 or aarch64)
case "${PLATFORM}" in
    linux/amd64)  ARCH="amd64" ;;
    linux/arm64)  ARCH="aarch64" ;;
    *)            ARCH="${PLATFORM#linux/}" ;;
esac

GHCR_FULL="${GHCR_IMAGE}-${ARCH}"

echo "=== Building evcc feature branch image ==="
echo "Image:    ${GHCR_FULL}:${VERSION}"
echo "Platform: ${PLATFORM}"
echo ""

# Build the image using the existing Dockerfile
docker build \
    --platform "${PLATFORM}" \
    --build-arg RELEASE=1 \
    -t "${GHCR_FULL}:${VERSION}" \
    -t "${GHCR_FULL}:latest" \
    -f Dockerfile \
    .

echo ""
echo "=== Build complete ==="

if [[ "${1:-}" == "--push" ]]; then
    echo "=== Logging in to ghcr.io ==="
    if [[ -n "${GHCR_TOKEN:-}" ]]; then
        echo "${GHCR_TOKEN}" | docker login ghcr.io -u ickeundso --password-stdin
    else
        gh auth token | docker login ghcr.io -u ickeundso --password-stdin
    fi

    echo "=== Pushing to ghcr.io ==="
    docker push "${GHCR_FULL}:${VERSION}"
    docker push "${GHCR_FULL}:latest"
    echo "=== Push complete ==="
else
    echo ""
    echo "To push, re-run with --push:"
    echo "  ./docs/build-feature-image.sh --push"
fi
