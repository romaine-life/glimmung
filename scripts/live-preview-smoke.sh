#!/usr/bin/env bash
# live-preview-smoke.sh — the RETAINED, re-runnable cross-app cutover smoke for
# Glimmung's live frontend preview lane (live-preview-plan.md Stage 4c; modeled
# on test-slot-deploy-plan.md Stage 6 "per-app deploy smoke").
#
# It provisions a REAL preview for an onboarded app and proves the five
# load-bearing properties OBSERVED FROM OUTSIDE (observed, never mocked), then
# deprovisions to a clean terminal. A unique per-run sentinel guarantees a stale
# image/serve cannot false-pass. This is the gate: it is GREEN for an app only
# when all five properties are observed green.
#
#   1. fresh-preview passthrough — before any push, the preview URL serves the
#      STABLE app's own frontend (the edge fresh-passthroughs to the backend).
#   2. observed-serve (load-bearing) — push a dist carrying a unique build id;
#      the edge GET /__live-preview/status reports that exact build AND the
#      served page contains the sentinel AND Glimmung's durable
#      preview_environment.state becomes `live` with observed_build_id == pushed.
#   3. replace-not-install — push build A then B; the preview moves A -> B.
#   4. clear-revert — DELETE the override; the URL reverts to the stable backend.
#   5. negative path — an UNAUTHORIZED push is REJECTED by the edge, and Glimmung
#      is not falsely marked live.
#   + backend prefixes stay backend-proxied through the edge regardless of override.
#
# It speaks the same contracts as the Stage 3 sender
# (k8s/session-config/live-preview-push.sh): it exchanges the projected
# auth.romaine.life token for a service-principal JWT, PUTs gzip(tar(dist/)) to
# the edge with X-Live-Preview-Build, POSTs the Glimmung push receipt, and reads
# the durable row back. It must run somewhere with the projected
# auth.romaine.life token (a Tank session pod, or a Glimmung-managed Job with the
# token projected — see docs/live-preview-smoke.md). It is the live-preview lane
# (scratch, for seeing); it never touches the faithful image-deploy validation
# lane and shares no vocabulary with the retired hot-swap path.
#
# Usage:
#   live-preview-smoke.sh --project P --name N [options]
#   live-preview-smoke.sh --all            # run every in-scope app sequentially
#
#   --project P --name N   the app + preview env name to smoke
#   --all                  smoke kill-me, chess-tactics, ambience, glimmung, tank-operator
#   --no-provision         reuse an existing ready preview (skip POST /v1/previews)
#   --keep                 skip the final deprovision (leave the preview up)
#   --glimmung-url URL     Glimmung base (default $GLIMMUNG_INTERNAL_URL or in-cluster)
#   --evidence-dir DIR     where to write observed evidence (default /tmp/live-preview-smoke)
#   --provision-timeout S  seconds to wait for provision->ready (default 480)
#   -h | --help
#
# Exit: 0 all observed properties green · 1 one or more red · 2 usage/setup.
set -uo pipefail

AUTH_TOKEN_PATH="${AUTH_ROMAINE_TOKEN_PATH:-/var/run/secrets/auth.romaine.life/token}"
AUTH_EXCHANGE_URL="${AUTH_ROMAINE_EXCHANGE_URL:-https://auth.romaine.life/api/auth/exchange/k8s}"
GLIMMUNG_URL="${GLIMMUNG_INTERNAL_URL:-http://glimmung.glimmung.svc.cluster.local}"
EVIDENCE_DIR="${LIVE_PREVIEW_SMOKE_EVIDENCE_DIR:-/tmp/live-preview-smoke}"
PROVISION_TIMEOUT=480
BUILD_HEADER="X-Live-Preview-Build"
DO_PROVISION=true
DO_DEPROVISION=true
PROJECT=""; NAME=""; ALL=false

# In-scope apps for --all (live-preview-plan.md Stage 4c). NAME is a DNS-1035
# label derived from the project. tank-operator was onboarded to the v2 lane in
# Stage 5 (it is now a normal preview consumer, no longer the retired v1 path),
# so it is covered here alongside the rest.
ALL_APPS=( "kill-me:smoke-killme" "chess-tactics:smoke-chess" "ambience:smoke-ambience" "glimmung:smoke-glimmung" "tank-operator:smoke-tankop" )

# Backend-proxy probe path override, per app. The backend-proxy property only
# needs ANY backend-proxied prefix to come back NON-override (proving the edge
# proxies it to the backend rather than serving the pushed bundle), so by default
# the probe uses the project's FIRST live_preview.backend_prefix — whatever it is
# (the 4c gate observed /api for kill-me + chess, /snap for ambience, /v1 for
# glimmung; none are health paths, and that's fine). The tank-operator override
# to /healthz is purely a cleaner, deterministic backend health 200 than its own
# first prefix (bare /api, which is backend-proxied and passes, but is a less
# obvious probe). Override only when a clearer probe is wanted.
declare -A BACKEND_PROBE_PATHS=( ["tank-operator"]="/healthz" )

log()  { printf '[smoke] %s\n' "$*" >&2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --project) shift; PROJECT="${1:-}" ;;
    --name) shift; NAME="${1:-}" ;;
    --all) ALL=true ;;
    --no-provision) DO_PROVISION=false ;;
    --keep) DO_DEPROVISION=false ;;
    --glimmung-url) shift; GLIMMUNG_URL="${1:-}" ;;
    --evidence-dir) shift; EVIDENCE_DIR="${1:-}" ;;
    --provision-timeout) shift; PROVISION_TIMEOUT="${1:-480}" ;;
    -h|--help) sed -n '2,46p' "$0"; exit 0 ;;
    *) log "unknown arg: $1"; exit 2 ;;
  esac
  shift
done
GLIMMUNG_URL="${GLIMMUNG_URL%/}"

tok() {
  [ -f "$AUTH_TOKEN_PATH" ] || { log "no projected token at $AUTH_TOKEN_PATH"; return 1; }
  curl -fsS -X POST "$AUTH_EXCHANGE_URL" -H "Authorization: Bearer $(cat "$AUTH_TOKEN_PATH")" \
    -H 'Content-Type: application/json' -d '{}' 2>/dev/null | jq -r '.token // empty'
}
jwt_sub() { # decode sub from a JWT (no verify)
  local p; p="$(printf '%s' "$1" | cut -d. -f2)"
  case $(( ${#p} % 4 )) in 2) p="${p}==";; 3) p="${p}=";; esac
  printf '%s' "$p" | tr '_-' '/+' | base64 -d 2>/dev/null | jq -r '.sub // empty' 2>/dev/null
}

# ---- per-app smoke -----------------------------------------------------------
# Returns 0 if all five properties observed green, 1 otherwise. Appends a
# machine-readable result line per property to $RESULTS_FILE.
smoke_one() {
  local project="$1" name="$2"
  local ev="$EVIDENCE_DIR/$project-$name"; mkdir -p "$ev"
  local run; run="$(cat /proc/sys/kernel/random/uuid 2>/dev/null || echo "$$-$RANDOM")"
  local pass=0 fail=0
  record() { # record PROP STATUS DETAIL
    printf '%s\t%s\t%s\t%s\n' "$project" "$1" "$2" "$3" >> "$RESULTS_FILE"
    if [ "$2" = "PASS" ]; then pass=$((pass+1)); else fail=$((fail+1)); fi
    log "  [$project] $1: $2 — $3"
  }

  local T; T="$(tok)" || { record provision ERROR "auth exchange failed"; return 1; }
  local SUB; SUB="$(jwt_sub "$T")"
  log "[$project/$name] subject=$SUB"

  # ---- provision ----
  local url=""
  if [ "$DO_PROVISION" = true ]; then
    local code
    code="$(curl -sS -o "$ev/provision.json" -w '%{http_code}' -X POST "$GLIMMUNG_URL/v1/previews" \
      -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg p "$project" --arg n "$name" --arg s "$SUB" \
            '{project:$p,name:$n,authorized_subject:$s,session_id:"stage4c-smoke"}')")"
    if [ "$code" != "202" ]; then
      record provision FAIL "POST /v1/previews HTTP $code: $(jq -rc '.detail // .title // .' "$ev/provision.json" 2>/dev/null)"
      record fresh_passthrough SKIP "provision failed"; record observed_serve SKIP "provision failed"
      record replace SKIP "provision failed"; record clear_revert SKIP "provision failed"
      record negative_path SKIP "provision failed"
      return 1
    fi
    # poll to ready/error
    local start=$SECONDS state="" detail=""
    while :; do
      T="$(tok)"; local row; row="$(curl -fsS -H "Authorization: Bearer $T" "$GLIMMUNG_URL/v1/previews/$project/$name" 2>/dev/null || true)"
      state="$(printf '%s' "$row" | jq -r '.state // "?"')"; detail="$(printf '%s' "$row" | jq -r '.detail // ""')"
      url="$(printf '%s' "$row" | jq -r '.url // ""')"
      [ "$state" = "ready" ] && break
      if [ "$state" = "error" ]; then
        printf '%s' "$row" > "$ev/provision-error.json"
        record provision FAIL "state=error: $detail"
        record fresh_passthrough FAIL "backend never became ready (provision error)"
        record observed_serve SKIP "provision error"; record replace SKIP "provision error"
        record clear_revert SKIP "provision error"; record negative_path SKIP "provision error"
        return 1
      fi
      if [ $(( SECONDS - start )) -ge "$PROVISION_TIMEOUT" ]; then
        record provision FAIL "timeout after ${PROVISION_TIMEOUT}s (state=$state)"; return 1
      fi
      sleep 8
    done
    record provision PASS "ready: $url"
  else
    local row; row="$(curl -fsS -H "Authorization: Bearer $T" "$GLIMMUNG_URL/v1/previews/$project/$name" 2>/dev/null || true)"
    url="$(printf '%s' "$row" | jq -r '.url // ""')"
    [ -n "$url" ] && [ "$url" != "null" ] || { record provision ERROR "no existing ready preview"; return 1; }
    log "  [$project] reusing existing preview $url"
  fi

  # resolve the app's backend-proxy probe path: a per-app BACKEND_PROBE_PATHS
  # override if set (tank-operator -> /healthz), else the app's own first
  # live_preview.backend_prefix — whatever it is (observed: /api kill-me+chess,
  # /snap ambience, /v1 glimmung). Any backend-proxied prefix proves the point.
  T="$(tok)"; local rowx; rowx="$(curl -fsS -H "Authorization: Bearer $T" "$GLIMMUNG_URL/v1/previews/$project/$name" 2>/dev/null || true)"
  local bprefix; bprefix="${BACKEND_PROBE_PATHS[$project]:-$(printf '%s' "$rowx" | jq -r '.backend_prefixes[0] // "/healthz"')}"

  # ---- wait for the edge to be EXTERNALLY reachable -------------------------
  # provision->ready means Helm + workload are up, but the preview's public DNS
  # record (ExternalDNS) + TLS can lag a few seconds. The edge's unauthenticated
  # /__live-preview/healthz is the readiness signal; poll it (resolves DNS + TLS
  # + edge-up) before asserting any observed property, so a propagation race
  # cannot read as a lane failure.
  # Require several CONSECUTIVE healthz successes: a freshly-provisioned preview
  # host's public DNS (ExternalDNS) propagates with flap/negative-cache, so a
  # single success can be followed by NXDOMAIN. A sustained streak means DNS has
  # settled, so the property curls below won't intermittently fail to resolve.
  local rstart=$SECONDS hz=000 streak=0
  while :; do
    hz="$(curl -sS -m 8 -o /dev/null -w '%{http_code}' "$url/__live-preview/healthz" 2>/dev/null || echo 000)"
    if [ "$hz" = "200" ]; then streak=$((streak+1)); else streak=0; fi
    [ "$streak" -ge 5 ] && break
    if [ $(( SECONDS - rstart )) -ge 300 ]; then
      record edge_reachable FAIL "edge /__live-preview/healthz not stably reachable after 300s (last HTTP $hz, DNS/TLS/route?)"
      record fresh_passthrough SKIP "edge unreachable"; record observed_serve SKIP "edge unreachable"
      record replace SKIP "edge unreachable"; record negative_path SKIP "edge unreachable"; record clear_revert SKIP "edge unreachable"
      [ "$DO_DEPROVISION" = true ] && { T="$(tok)"; curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $T" "$GLIMMUNG_URL/v1/previews/$project/$name"; }
      return 1
    fi
    sleep 5
  done
  record edge_reachable PASS "edge healthz 200 (DNS+TLS+route live)"

  # ---- property 1: fresh-preview passthrough (before any push) ----
  T="$(tok)"
  local st; st="$(curl -fsS -m 15 -H "Authorization: Bearer $T" "$url/__live-preview/status" 2>/dev/null || echo '{}')"
  printf '%s' "$st" > "$ev/p1-status.json"
  # NOTE: jq's `// empty` treats boolean false as falsy — extract the boolean
  # plainly so override_active=false reads as "false", not "".
  local oa; oa="$(printf '%s' "$st" | jq -r '.override_active')"
  local rc; rc="$(curl -sS -m 15 -o "$ev/p1-root.html" -w '%{http_code}' "$url/" 2>/dev/null || echo 000)"
  # Passthrough works when the edge REACHES the backend: any 2xx/3xx is the
  # backend responding; the edge emits 502 "upstream backend unavailable" only
  # when it cannot reach the backend. (A crashed backend => 502 => fail.)
  if [ "$oa" = "false" ] && printf '%s' "$rc" | grep -qE '^[23]' && ! grep -q "LIVE-PREVIEW-SENTINEL" "$ev/p1-root.html" 2>/dev/null; then
    record fresh_passthrough PASS "override_active=false, root HTTP $rc — edge fresh-passthrough reaches stable backend (no override)"
  else
    record fresh_passthrough FAIL "override_active=$oa, root HTTP $rc (want false + 2xx/3xx backend passthrough, no sentinel; 5xx=edge can't reach backend)"
  fi

  # ---- property 2: observed-serve (push A, read back edge + durable live) ----
  local buildA="b-A-$run"
  mkdir -p "$ev/distA"
  printf '<!doctype html><html><head><title>smoke</title></head><body><h1>LIVE-PREVIEW-SENTINEL-%s</h1></body></html>\n' "$buildA" > "$ev/distA/index.html"
  T="$(tok)"
  local pc; pc="$(tar czf - -C "$ev/distA" . | curl -sS -o "$ev/p2-push.json" -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $T" -H 'Content-Type: application/gzip' -H "$BUILD_HEADER: $buildA" \
    --data-binary @- "$url/__live-preview/push")"
  curl -sS -o /dev/null -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg b "$buildA" '{build:$b}')" "$GLIMMUNG_URL/v1/previews/$project/$name/push-receipt"
  T="$(tok)"; st="$(curl -fsS -m15 -H "Authorization: Bearer $T" "$url/__live-preview/status" 2>/dev/null || echo '{}')"
  printf '%s' "$st" > "$ev/p2-status.json"
  local sb; sb="$(printf '%s' "$st" | jq -r '.build // empty')"
  curl -sS -m15 -o "$ev/p2-served.html" "$url/" 2>/dev/null
  local served_ok=1; grep -q "LIVE-PREVIEW-SENTINEL-$buildA" "$ev/p2-served.html" 2>/dev/null && served_ok=0
  # durable live + observed_build_id
  local dstate="" dobs="" dstart=$SECONDS
  while :; do
    T="$(tok)"; local row; row="$(curl -fsS -H "Authorization: Bearer $T" "$GLIMMUNG_URL/v1/previews/$project/$name" 2>/dev/null || true)"
    dstate="$(printf '%s' "$row" | jq -r '.state // "?"')"; dobs="$(printf '%s' "$row" | jq -r '.observed_build_id // ""')"
    { [ "$dstate" = "live" ] && [ "$dobs" = "$buildA" ]; } && { printf '%s' "$row" > "$ev/p2-durable.json"; break; }
    [ "$dstate" = "stale" ] && { printf '%s' "$row" > "$ev/p2-durable.json"; break; }
    [ $(( SECONDS - dstart )) -ge 90 ] && break
    sleep 6
  done
  if [ "$pc" = "200" ] && [ "$sb" = "$buildA" ] && [ "$served_ok" = "0" ] && [ "$dstate" = "live" ] && [ "$dobs" = "$buildA" ]; then
    record observed_serve PASS "edge status build=$buildA, served page carries sentinel, durable state=live observed_build_id=$buildA"
  else
    record observed_serve FAIL "push HTTP $pc, status.build=$sb, served_sentinel=$([ $served_ok = 0 ] && echo yes || echo no), durable state=$dstate observed=$dobs (want all: 200/$buildA/yes/live/$buildA)"
  fi

  # ---- backend prefixes stay backend-proxied during override ----
  T="$(tok)"
  local hp; hp="$(curl -sS -m15 -o "$ev/p2-health.out" -w '%{http_code}' "$url$bprefix" 2>/dev/null || echo 000)"
  if ! grep -q "LIVE-PREVIEW-SENTINEL" "$ev/p2-health.out" 2>/dev/null; then
    record backend_proxy PASS "backend prefix $bprefix proxied to backend during override (HTTP $hp, not the override)"
  else
    record backend_proxy FAIL "backend prefix $bprefix served the override instead of proxying to backend"
  fi

  # ---- property 3: replace-not-install (push B over A) ----
  local buildB="b-B-$run"
  mkdir -p "$ev/distB"
  printf '<!doctype html><html><head><title>smoke</title></head><body><h1>LIVE-PREVIEW-SENTINEL-%s</h1></body></html>\n' "$buildB" > "$ev/distB/index.html"
  T="$(tok)"
  local pc2; pc2="$(tar czf - -C "$ev/distB" . | curl -sS -o "$ev/p3-push.json" -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $T" -H 'Content-Type: application/gzip' -H "$BUILD_HEADER: $buildB" \
    --data-binary @- "$url/__live-preview/push")"
  curl -sS -o /dev/null -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg b "$buildB" '{build:$b}')" "$GLIMMUNG_URL/v1/previews/$project/$name/push-receipt"
  T="$(tok)"; st="$(curl -fsS -m15 -H "Authorization: Bearer $T" "$url/__live-preview/status" 2>/dev/null || echo '{}')"
  local sb2; sb2="$(printf '%s' "$st" | jq -r '.build // empty')"
  curl -sS -m15 -o "$ev/p3-served.html" "$url/" 2>/dev/null
  if [ "$pc2" = "200" ] && [ "$sb2" = "$buildB" ] && grep -q "LIVE-PREVIEW-SENTINEL-$buildB" "$ev/p3-served.html" 2>/dev/null && ! grep -q "LIVE-PREVIEW-SENTINEL-$buildA" "$ev/p3-served.html" 2>/dev/null; then
    record replace PASS "moved A->B: status.build=$buildB, served page is B not A"
  else
    record replace FAIL "push HTTP $pc2, status.build=$sb2 (want $buildB), served B-not-A check failed"
  fi

  # ---- property 5: negative path (unauthorized push rejected; not falsely live) ----
  T="$(tok)"
  local n1 n2
  n1="$(tar czf - -C "$ev/distA" . | curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    -H 'Content-Type: application/gzip' -H "$BUILD_HEADER: b-EVIL-$run" --data-binary @- "$url/__live-preview/push")"
  n2="$(tar czf - -C "$ev/distA" . | curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer not.a.valid.jwt" -H 'Content-Type: application/gzip' -H "$BUILD_HEADER: b-EVIL-$run" --data-binary @- "$url/__live-preview/push")"
  st="$(curl -fsS -m15 -H "Authorization: Bearer $T" "$url/__live-preview/status" 2>/dev/null || echo '{}')"
  local sb3; sb3="$(printf '%s' "$st" | jq -r '.build // empty')"
  if { [ "$n1" = "401" ] || [ "$n1" = "403" ]; } && { [ "$n2" = "401" ] || [ "$n2" = "403" ]; } && [ "$sb3" = "$buildB" ]; then
    record negative_path PASS "no-auth=$n1, bad-token=$n2 rejected; edge still serves $buildB (not the evil build)"
  else
    record negative_path FAIL "no-auth=$n1, bad-token=$n2 (want 401/403); edge build=$sb3 (want unchanged $buildB)"
  fi

  # ---- property 4: clear-revert (DELETE override -> stable backend passthrough) ----
  T="$(tok)"
  local dc; dc="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $T" "$url/__live-preview/push")"
  st="$(curl -fsS -m15 -H "Authorization: Bearer $T" "$url/__live-preview/status" 2>/dev/null || echo '{}')"
  printf '%s' "$st" > "$ev/p4-status.json"
  local oa2; oa2="$(printf '%s' "$st" | jq -r '.override_active')"
  local rc2; rc2="$(curl -sS -m15 -o "$ev/p4-root.html" -w '%{http_code}' "$url/" 2>/dev/null || echo 000)"
  if [ "$dc" = "200" ] && [ "$oa2" = "false" ] && printf '%s' "$rc2" | grep -qE '^[23]' && ! grep -q "LIVE-PREVIEW-SENTINEL" "$ev/p4-root.html" 2>/dev/null; then
    record clear_revert PASS "DELETE 200, override_active=false, root HTTP $rc2 reverted to stable backend passthrough (no sentinel)"
  else
    record clear_revert FAIL "DELETE HTTP $dc, override_active=$oa2, root HTTP $rc2 (want 200/false + 2xx/3xx passthrough, no sentinel)"
  fi

  # ---- deprovision (clean terminal, no leak) ----
  if [ "$DO_DEPROVISION" = true ]; then
    T="$(tok)"
    local delc; delc="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $T" "$GLIMMUNG_URL/v1/previews/$project/$name")"
    T="$(tok)"; local g; g="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $T" "$GLIMMUNG_URL/v1/previews/$project/$name")"
    if [ "$delc" = "200" ] && [ "$g" = "404" ]; then
      record deprovision PASS "DELETE 200, row gone (GET 404) — clean terminal"
    else
      record deprovision FAIL "DELETE HTTP $delc, subsequent GET HTTP $g (want 200/404)"
    fi
  fi

  log "[$project] result: $pass passed, $fail failed (evidence: $ev)"
  [ "$fail" -eq 0 ]
}

# ---- driver ------------------------------------------------------------------
mkdir -p "$EVIDENCE_DIR"
RESULTS_FILE="$EVIDENCE_DIR/results.tsv"; : > "$RESULTS_FILE"
overall=0
if [ "$ALL" = true ]; then
  for spec in "${ALL_APPS[@]}"; do
    smoke_one "${spec%%:*}" "${spec##*:}" || overall=1
  done
else
  [ -n "$PROJECT" ] && [ -n "$NAME" ] || { log "need --project and --name (or --all)"; exit 2; }
  smoke_one "$PROJECT" "$NAME" || overall=1
fi

echo "" >&2
log "================= OBSERVED RESULTS ================="
awk -F'\t' '{printf "  %-14s %-18s %-5s %s\n",$1,$2,$3,$4}' "$RESULTS_FILE" >&2
log "==================================================="
exit "$overall"
