#!/usr/bin/env bash
# Preview-lane route-render contract.
#
# A live-preview environment is provisioned by ProvisionPreview
# (internal/server/preview_provision.go) reconciling the slot chart in a warm→hot
# sequence — the SAME sequence the faithful validation activation uses. The two
# phases render disjoint, complementary slices of the slot chart, gated by
# renderMode:
#
#   - WARM  renders the renderWarm-gated route/prereqs (HTTPRoute + testenv
#           ServiceAccount/ConfigMap/ExternalSecret). This is the ONLY phase that
#           materializes the HTTPRoute, so it is what makes the preview URL
#           reachable.
#   - HOT   renders the renderHot-gated workload (Deployment + Service) with the
#           live-preview edge in front of the backend.
#
# A hot-only preview install (the bug this guard pins shut) renders the workload
# + Service but NO HTTPRoute, leaving the preview URL unreachable. This guard
# renders glimmung's own slot chart (k8s/issue) through both the preview and the
# validation warm/hot sequences and asserts:
#
#   - preview warm  -> HTTPRoute for the PREVIEW's host, no workload
#   - preview hot   -> Deployment + Service + edge container + override volume,
#                      no HTTPRoute
#   - validation    -> route in warm, workload in hot, and NO live-preview edge
#                      surface at all (the edge machinery is inert for the
#                      faithful lane: validation renders are unchanged)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${repo_root}/k8s/issue"

# The live-preview-edge library partial is a file:// dependency vendored into
# charts/ at install time (charts/ is gitignored, Chart.lock is committed).
helm dependency build "${chart}" >/dev/null

preview_host="preview-glimmung-s1.glimmung.dev.romaine.life"
slot="preview-glimmung-s1"

preview_common=(
  --set "testEnv.slotName=${slot}"
  --set "hostname=${preview_host}"
  --set "image.tag=app-deadbeefdeadbeef"
  --set "livePreview.enabled=true"
  --set "livePreview.image.repository=acr.io/edge"
  --set "livePreview.image.tag=edge-v1"
  --set "livePreview.authorizedSubject=svc:preview:owner"
  --set "livePreview.upstream.url=http://127.0.0.1:8000"
  --set "livePreview.backendPrefixes[0]=/api"
  --set "livePreview.backendPrefixes[1]=/healthz"
  --set "hotSwapBackend.enabled=false"
)

val_slot="glimmung-slot-1"
val_host="glimmung-slot-1.glimmung.dev.romaine.life"
val_common=(
  --set "testEnv.slotName=${val_slot}"
  --set "hostname=${val_host}"
  --set "image.tag=app-deadbeefdeadbeef"
)

render() {
  helm template "$1" "${chart}" --namespace "$2" "${@:3}"
}

preview_warm="$(render prev-warm "${slot}" --set renderMode=warm "${preview_common[@]}")"
preview_hot="$(render prev-hot "${slot}" --set renderMode=hot "${preview_common[@]}")"
val_warm="$(render val-warm "${val_slot}" --set renderMode=warm "${val_common[@]}")"
val_hot="$(render val-hot "${val_slot}" --set renderMode=hot "${val_common[@]}")"

require_contains() { # haystack needle label
  if ! grep -F -q -e "$2" <<<"$1"; then
    echo "FAIL ${3}: missing '${2}'" >&2
    exit 1
  fi
}

require_absent() { # haystack needle label
  if grep -F -q -e "$2" <<<"$1"; then
    echo "FAIL ${3}: must not contain '${2}'" >&2
    exit 1
  fi
}

require_kind_present() { # haystack kind label
  if ! grep -E -q -e "^kind: ${2}$" <<<"$1"; then
    echo "FAIL ${3}: expected a ${2} in the render" >&2
    exit 1
  fi
}

require_kind_absent() { # haystack kind label
  if grep -E -q -e "^kind: ${2}$" <<<"$1"; then
    echo "FAIL ${3}: a ${2} must NOT render in this phase" >&2
    exit 1
  fi
}

# --- preview WARM: the route (and only the route + prereqs) for the preview host
require_kind_present "${preview_warm}" "HTTPRoute" "preview-warm"
require_contains "${preview_warm}" "${preview_host}" "preview-warm route host"
require_kind_absent "${preview_warm}" "Deployment" "preview-warm"
require_kind_absent "${preview_warm}" "Service" "preview-warm"

# --- preview HOT: the workload + Service + the vendored edge, no route
require_kind_present "${preview_hot}" "Deployment" "preview-hot"
require_kind_present "${preview_hot}" "Service" "preview-hot"
require_contains "${preview_hot}" "name: live-preview-edge" "preview-hot edge container"
require_contains "${preview_hot}" "name: live-preview-override" "preview-hot override emptyDir"
# The backend port is renamed to app-internal because the edge owns "http".
require_contains "${preview_hot}" "name: app-internal" "preview-hot backend internal port"
require_kind_absent "${preview_hot}" "HTTPRoute" "preview-hot"

# --- validation UNCHANGED: route in warm, workload in hot, NO edge surface.
# "Inert" is asserted against the edge's CONTAINER and override-VOLUME markers,
# not a bare "live-preview-edge" substring: deployment.yaml carries a descriptive
# YAML comment mentioning the live-preview-edge.replicas helper that renders in
# every hot manifest (preview and validation alike) and is not edge surface.
require_kind_present "${val_warm}" "HTTPRoute" "validation-warm"
require_absent "${val_warm}" "name: live-preview-edge" "validation-warm edge container must be inert"
require_absent "${val_warm}" "live-preview-override" "validation-warm override volume must be inert"
require_absent "${val_warm}" "app-internal" "validation-warm backend port unchanged"
require_kind_present "${val_hot}" "Deployment" "validation-hot"
require_kind_present "${val_hot}" "Service" "validation-hot"
require_kind_absent "${val_hot}" "HTTPRoute" "validation-hot"
require_absent "${val_hot}" "name: live-preview-edge" "validation-hot edge container must be inert"
require_absent "${val_hot}" "live-preview-override" "validation-hot override volume must be inert"
# The faithful lane keeps the backend's own "http" served port (no edge rename).
require_absent "${val_hot}" "app-internal" "validation-hot backend port unchanged"
require_contains "${val_hot}" "targetPort: http" "validation-hot service targets backend http"

echo "preview warm->hot renders the HTTPRoute (warm) + workload/Service/edge (hot); validation renders unchanged (no edge surface)."
