#!/usr/bin/env bash
set -euo pipefail

require_platform=false
if [[ "${1:-}" == "--require-platform" ]]; then
  require_platform=true
  shift
fi
if [[ "$#" -ne 0 ]]; then
  echo "usage: $0 [--require-platform]" >&2
  exit 2
fi
if [[ "${require_platform}" == true ]]; then
  for name in SDK_CANARY_INGEST_KEY SDK_CANARY_READ_API_KEY SDK_CANARY_MCP_SERVER_ID SDK_CANARY_PLATFORM_URL; do
    if [[ -z "${!name:-}" ]]; then
      echo "missing live canary configuration: ${name}" >&2
      exit 1
    fi
  done
fi

package_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
consumer="$(mktemp -d)"
trap 'rm -rf "${consumer}"' EXIT

cp "${package_root}"/integration_test/*.go "${consumer}/"
cd "${consumer}"
go mod init armature-sdk-canary.invalid/consumer
go mod edit -require=github.com/armature-tech/mcp-analytics-go@v0.0.0
go mod edit -replace="github.com/armature-tech/mcp-analytics-go=${package_root}"
go mod tidy
go test ./...

resolved="$(go list -m -f '{{.Dir}}' github.com/armature-tech/mcp-analytics-go)"
if [[ "${resolved}" != "${package_root}" ]]; then
  echo "candidate module resolved outside the expected checkout: ${resolved}" >&2
  exit 1
fi
echo "verified external Go consumer against ${resolved}"
