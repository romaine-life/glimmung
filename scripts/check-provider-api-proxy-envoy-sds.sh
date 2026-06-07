#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(helm template glimmung "${repo_root}/k8s")"

extract_configmap() {
  local name="$1"
  awk -v name="${name}" '
    /^---$/ {
      if (found) {
        printf "%s", doc
        exit
      }
      doc = ""
      found = 0
      next
    }
    {
      doc = doc $0 ORS
      if ($0 == "  name: " name) found = 1
    }
    END {
      if (found) printf "%s", doc
    }
  ' <<<"${rendered}"
}

require_contains() {
  local haystack="$1"
  local needle="$2"
  local label="$3"
  if ! grep -Fq "${needle}" <<<"${haystack}"; then
    echo "${label}: missing ${needle}" >&2
    exit 1
  fi
}

require_absent() {
  local haystack="$1"
  local needle="$2"
  local label="$3"
  if grep -Fq "${needle}" <<<"${haystack}"; then
    echo "${label}: forbidden static TLS field present: ${needle}" >&2
    exit 1
  fi
}

for provider in claude codex; do
  configmap="${provider}-api-proxy-envoy"
  block="$(extract_configmap "${configmap}")"
  if [ -z "${block}" ]; then
    echo "${configmap}: ConfigMap not found in Helm render" >&2
    exit 1
  fi

  require_contains "${block}" "secrets:" "${configmap}"
  require_contains "${block}" "name: provider_api_proxy_leaf" "${configmap}"
  require_contains "${block}" "tls_certificate_sds_secret_configs:" "${configmap}"
  require_contains "${block}" "filename: /etc/envoy/tls/tls.crt" "${configmap}"
  require_contains "${block}" "filename: /etc/envoy/tls/tls.key" "${configmap}"
  require_contains "${block}" "watched_directory:" "${configmap}"
  require_contains "${block}" "path: /etc/envoy/tls" "${configmap}"
  require_absent "${block}" "tls_certificates:" "${configmap}"
done

echo "Provider API proxy Envoy TLS leaves use file-based SDS with watched Kubernetes Secret mounts."
