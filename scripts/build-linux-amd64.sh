#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: ./scripts/build-linux-amd64.sh

Build and verify the Linux/amd64 CPA plugin with Docker.

Environment variables:
  BUILD_IMAGE  Go build image (default: public.ecr.aws/docker/library/golang:1.26-bookworm)
  VERSION      Artifact version without a leading v (default: cmd/plugin/main.go)

Output:
  dist/cpa-plugin-codex-turn-state-v<version>.so
  dist/cpa-plugin-codex-turn-state-v<version>.h
EOF
}

case "${1:-}" in
  -h|--help)
    usage
    exit 0
    ;;
  "") ;;
  *)
    printf 'unknown argument: %s\n\n' "$1" >&2
    usage >&2
    exit 2
    ;;
esac

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required but was not found" >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "docker is installed but the daemon is unavailable" >&2
  exit 1
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
build_image="${BUILD_IMAGE:-public.ecr.aws/docker/library/golang:1.26-bookworm}"
source_version="$(sed -n 's/^var pluginVersion = "\([^"]*\)"$/\1/p' "${repo_root}/cmd/plugin/main.go")"
if [[ -z "${source_version}" ]]; then
  echo "could not determine plugin version from cmd/plugin/main.go" >&2
  exit 1
fi
version="${VERSION:-${source_version}}"

if [[ ! "${version}" =~ ^[0-9A-Za-z._-]+$ ]]; then
  echo "VERSION may contain only letters, numbers, dots, underscores, and dashes" >&2
  exit 2
fi

artifact_rel="dist/cpa-plugin-codex-turn-state-v${version}.so"
header_rel="${artifact_rel%.so}.h"
mkdir -p "${repo_root}/dist"

echo "Running Go verification"
(
  cd "${repo_root}"
  go test ./...
  go vet ./...
)

echo "Building ${artifact_rel} with ${build_image} (linux/amd64)"
docker run --rm \
  --platform linux/amd64 \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp \
  -e GOCACHE=/tmp/go-build \
  -e GOMODCACHE=/tmp/go-mod \
  -e ARTIFACT="${artifact_rel}" \
  -e VERSION="${version}" \
  -v "${repo_root}:/src" \
  -w /src \
  "${build_image}" \
  sh -lc 'CGO_ENABLED=1 GOOS=linux GOARCH=amd64 /usr/local/go/bin/go build -trimpath -buildmode=c-shared -ldflags "-X main.pluginVersion=${VERSION}" -o "$ARTIFACT" ./cmd/plugin'

echo "Verifying Linux ABI"
docker run --rm \
  --platform linux/amd64 \
  -e ARTIFACT="/src/${artifact_rel}" \
  -v "${repo_root}:/src:ro" \
  -w /src \
  "${build_image}" \
  sh -lc '
    set -eu
    gcc -O2 -o /tmp/native-abi-smoke native_abi_smoke.c -ldl
    /tmp/native-abi-smoke "$ARTIFACT"
    readelf -h "$ARTIFACT" | grep -E "Class:|Type:|Machine:"
    nm -D "$ARTIFACT" | grep " T cliproxy_plugin_init$"
    sha256sum "$ARTIFACT"
  '

(
  cd "${repo_root}/dist"
  sha256sum "$(basename "${artifact_rel}")" > "$(basename "${artifact_rel}").sha256"
)

printf 'Built: %s\n' "${repo_root}/${artifact_rel}"
printf 'Header: %s\n' "${repo_root}/${header_rel}"
printf 'Checksum: %s\n' "${repo_root}/${artifact_rel}.sha256"
