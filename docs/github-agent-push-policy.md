# GitHub Agent Push & CI Access Policy

Native implementation agents use ordinary Git and GitHub muscle memory:

```sh
git add -A
git commit -m "agent: address <issue>"
git push origin HEAD
# read the draft PR's CI:
curl -H "Authorization: Bearer $TOKEN" https://api.github.com/repos/<owner>/<repo>/commits/<sha>/check-runs
```

The control plane makes that safe without the agent knowing any of it is
happening. The agent gets one repo-scoped GitHub token and behaves normally;
branch-scoped push enforcement and egress confinement are transparent.

## Token

The implementation agent owns a real **GitHub App installation token**, minted
per run, **repo-scoped and minimally permissioned**:

- `contents: write` — push the implementation branch.
- `metadata: read` — baseline.
- `checks: read` — read the draft PR's CI check-runs (the agent's CI-feedback loop).

> Earlier designs kept the GitHub token at a proxy and handed the agent a signed
> "push-policy token" instead of a real GitHub token. That is **retired**: the
> agent owns the token; the proxy holds no credentials. Do not reintroduce it.

The token is minted by `POST /v1/run-callbacks/{callback_token}/native/github-agent-token`
(`writeNativeGitHubAgentToken` → `RepositoryInstallationToken(repo, permissions)`).
The wrapper mints it and mounts it into the implementation agent Job as a file
(`GITHUB_TOKEN_FILE`, `GITHUB_CREDENTIAL_USERNAME=x-access-token`). The agent
subprocess does **not** receive Glimmung callback URLs/tokens, the broad
installation-token URL, PR merge/touchpoint URLs, or SSH/Tailscale mint URLs
(`agentStepBaseEnv` strips them).

## Branch-scoped push enforcement

There is no GitHub-native token shape for "may push only to branch X", so the
push path is confined by a proxy, transparently:

1. Glimmung pre-creates the issue-scoped branch + draft PR before the agent
   starts (so CI feedback exists during the agent loop).
2. The agent Job routes `github.com` through the `github-git-policy-proxy`
   (a `glimmung-runs` Envoy + policy sidecar) via a `hostAlias` + the
   provider-proxy CA bundle.
3. The agent pushes with its own token (Basic auth, `x-access-token`); the proxy
   forwards that auth unchanged, holds no credentials of its own, and enforces
   the authorized repo/ref on `git-receive-pack` (pkt-line parsing), keyed off
   pod annotations (`glimmung.romaine.life/github-policy-{repo,ref}`).
4. Wrong repo/ref/expired/malformed fails closed with a normal git error; the
   agent corrects the branch and re-runs `git push`.

## CI-checks egress (api.github.com)

The agent reads its draft PR's CI by name (`api.github.com`) while under the
default-deny egress lockdown. OSS Calico cannot match egress by FQDN, so
`api.github.com` is routed, transparently, through a name-based **Envoy Gateway**:

- A glimmung-owned `Gateway` (class `eg`, TLS **passthrough** `:443`) + `TLSRoute`
  (SNI `api.github.com`) + external `Backend` (`api.github.com:443`) — see
  `k8s/templates/agent-egress-gateway.yaml`. Requires Envoy Gateway's Backend
  extension API (`extensionApis.enableBackend`, enabled in infra-bootstrap).
- The agent Job `hostAlias`es `api.github.com` onto the gateway's data-plane
  Service (resolved at runtime by the wrapper). TLS passthrough keeps it
  **end-to-end to GitHub** — the agent validates GitHub's real cert; the gateway
  terminates nothing and holds no credentials. It forwards only the
  `api.github.com` SNI.
- The agent never dispatches CI: the draft PR's `pull_request` event runs it
  automatically; the agent only reads check-runs.

## Egress lockdown

Each implementation agent Job is confined by a per-run `NetworkPolicy`
(default-deny egress; selects pods labeled
`glimmung.romaine.life/github-egress-lockdown`). Allowed egress:

- cluster DNS (kube-dns),
- the `glimmung-runs` namespace (provider proxies + git policy proxy),
- same-namespace (slot/app interactions),
- the `envoy-gateway-system` namespace (the agent-egress gateway data-plane).

The in-cluster allows are **not** port-restricted. Calico (Azure CNI overlay)
enforces egress against the post-DNAT pod targetPort (the proxies listen on
`8443`, the gateway on `10443`), so a Service-port `:443` rule never matches and
silently blocks the agent from its own proxies. The namespace is the trust
boundary. Everything else external is denied; GitHub is reachable only via the
git proxy (push) and the egress gateway (`api.github.com`).

## Non-Goals

- Do not hand the agent a broad installation token, or the broad-token mint URL.
- Do not let the agent call Glimmung callback endpoints directly.
- Do not rely on prompt wording as the safety boundary.
- Do not reintroduce the signed push-policy token or proxy-held GitHub credentials.

## Implementation

- glimmung `internal/server/native_github_token_api.go` — repo-scoped agent token
  mint (`contents:write, metadata:read, checks:read`).
- glimmung `cmd/glimmung-native-runner` — `agentStepBaseEnv` strips callback/token
  URLs from the agent subprocess; installs the git credential.
- glimmung `k8s/templates/agent-egress-gateway.yaml` — the egress Gateway + Backend
  + TLSRoute; `k8s/templates/provider-api-proxy.yaml` — the git policy proxy
  (`glimmung_api_proxy.github_proxy`).
- ambience `scripts/glimmung-native/{implement.sh,agent-ci-feedback.sh,lib.sh}` +
  `mcp/ambience_preview/ops.py` — branch/draft-PR setup, the per-run egress
  NetworkPolicy + hostAliases, and the agent's CI-feedback script.
