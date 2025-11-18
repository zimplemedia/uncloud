#!/usr/bin/env bash
set -euo pipefail

VERSION=${1:-"0.1.1"}
DIST_DIR="dist"

echo "Building Uncloud release v${VERSION}..."
mkdir -p "${DIST_DIR}"

# Build uc CLI for macOS (native - for your local use)
echo "Building uc CLI for macOS (native)..."
go build -o "${DIST_DIR}/uc" ./cmd/uncloud
tar -czf "${DIST_DIR}/uc_darwin_arm64.tar.gz" -C "${DIST_DIR}" uc
rm "${DIST_DIR}/uc"

# Build uc CLI for Linux (for CI/production)
echo "Building uc CLI for Linux..."
GOOS=linux GOARCH=amd64 go build -o "${DIST_DIR}/uc" ./cmd/uncloud
tar -czf "${DIST_DIR}/uc_linux_amd64.tar.gz" -C "${DIST_DIR}" uc
rm "${DIST_DIR}/uc"

GOOS=linux GOARCH=arm64 go build -o "${DIST_DIR}/uc" ./cmd/uncloud
tar -czf "${DIST_DIR}/uc_linux_arm64.tar.gz" -C "${DIST_DIR}" uc
rm "${DIST_DIR}/uc"

# Build uncloudd daemon for Linux only (what runs on servers)
echo "Building uncloudd daemon for Linux..."
GOOS=linux GOARCH=amd64 go build -o "${DIST_DIR}/uncloudd" ./cmd/uncloudd
tar -czf "${DIST_DIR}/uncloudd_linux_amd64.tar.gz" -C "${DIST_DIR}" uncloudd
rm "${DIST_DIR}/uncloudd"

GOOS=linux GOARCH=arm64 go build -o "${DIST_DIR}/uncloudd" ./cmd/uncloudd
tar -czf "${DIST_DIR}/uncloudd_linux_arm64.tar.gz" -C "${DIST_DIR}" uncloudd
rm "${DIST_DIR}/uncloudd"

echo ""
echo "✓ Build complete! Release files in ${DIST_DIR}/"
ls -lh "${DIST_DIR}/"
