# Provider API Proxy Auth

Glimmung native agent jobs do not own provider OAuth refresh chains.

The durable shape is one Glimmung-owned proxy deployment per provider in the
native runner namespace:

- `claude-api-proxy` fronts `api.anthropic.com`.
- `codex-api-proxy` fronts `api.openai.com` for current Codex Responses API
  traffic and still accepts `chatgpt.com` for older Codex ChatGPT-backed
  calls.
- Native jobs write placeholder provider credentials with
  `managed-by-glimmung`.
- Native jobs route provider hostnames to the proxy Service ClusterIPs through
  pod `hostAliases`.
- The proxies mount the real OAuth blobs from ExternalSecret-rendered Secrets.
- The proxies inject the current real access token, single-flight refresh on
  upstream `401`, update memory before Key Vault writeback, and expose bounded
  refresh metrics.

The credential source of truth is Azure Key Vault `ng6-glimmung`. External
Secrets mirrors `claude-code-credentials` and `codex-credentials` into the
proxy namespace. Runner jobs must never mount those Secrets. A native job that
contains an agent step fails before Job creation if the proxy Services cannot
be resolved, because starting a provider-backed agent without the central
refresh owner would reintroduce per-pod credential ownership.

TLS is intentionally terminated by the provider proxy with leaf certificates
for the real provider hostnames. The native launcher mounts the proxy CA into
agent jobs and builds a combined CA bundle from the image's system roots plus
the proxy CA, then points `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, and
`GIT_SSL_CAINFO` at that bundle. `NODE_EXTRA_CA_CERTS` points at the proxy CA
directly for Node-based CLIs.

The proxy Envoy listeners load their downstream leaf certificates through
file-based SDS with `watched_directory: /etc/envoy/tls`. cert-manager still
owns and renews the Kubernetes TLS Secrets, and kubelet projects the updated
Secret files into the pod; Envoy watches the directory and updates the active
TLS context without waiting for a Deployment rollout. Static downstream
`tls_certificates` file references are retired for these proxies because they
can keep serving the old leaf until the pod is restarted.

## Operational Contract

- Exactly one authoritative proxy Deployment per provider writes the Glimmung
  provider credential Key Vault secrets.
- Test slots and native jobs do not create provider proxies with write access
  to the same refresh chains.
- `glimmung_api_proxy_token_refresh_total`,
  `glimmung_api_proxy_kv_persist_total`, and
  `glimmung_api_proxy_upstream_401_total` are the primary refresh-health
  counters.
- `/health/claude` and `/health/codex` expose the last refresh attempt
  snapshot for future control-plane projection.

## Migration Guard

The retired path is mounting real provider credential Secrets into native jobs
and copying them into `$HOME/.codex` or `$HOME/.claude`. Tests pin that native
Job manifests do not contain the old `codex-credentials` volume or
`/etc/codex-creds` mount, and the native runner writes placeholder Codex auth
instead of copying a mounted secret.

`scripts/check-provider-api-proxy-envoy-sds.sh` renders the production Helm
chart and fails if `claude-api-proxy-envoy` or `codex-api-proxy-envoy` loses the
file-based SDS Secret or reintroduces static downstream `tls_certificates`.
