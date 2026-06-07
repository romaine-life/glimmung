# GitHub Agent Push Policy

Native implementation agents should use ordinary Git muscle memory:

```sh
git add -A
git commit -m "agent: address <issue>"
git push origin HEAD
```

The control plane must make that safe. Do not solve this by teaching every
workflow a bespoke push command or by handing the agent a broad GitHub token.

## Required Shape

For native jobs with managed `type: agent` steps:

1. Glimmung creates the implementation branch and draft PR before the agent
   starts. The branch name is issue-scoped, for example
   `glimmung/issue-168/<run-id>`.
2. The native runner installs a managed `post-commit` hook in the checkout.
   The hook reminds the agent to push and inspect draft PR CI after every
   commit.
3. The agent subprocess does not receive Glimmung callback URLs, callback
   tokens, GitHub API tokens, PR merge/touchpoint URLs, SSH cert mint URLs, or
   Tailscale auth-key mint URLs in its environment.
4. The checkout remote stays ordinary:
   `https://github.com/<owner>/<repo>.git`.
5. Native job pods route `github.com` through a Glimmung-managed GitHub policy
   proxy with the same transparent host-alias + CA-bundle pattern used for
   `api.anthropic.com`, `api.openai.com`, and `chatgpt.com`.
6. The agent receives only a branch-scoped Git credential. That credential is
   not a GitHub token. It is a Glimmung push-policy token containing repo,
   allowed branch ref, run id, issue number, and expiry.
7. The GitHub policy proxy validates the policy token, injects the server-held
   GitHub App installation token upstream, and enforces the policy on
   `git-receive-pack` before forwarding to GitHub.
8. Wrong repo, wrong branch, expired policy token, malformed receive-pack, or
   unsupported ref update fails closed with a normal git remote error. The
   agent can then correct the branch and run the same `git push` again.
9. After the agent exits, the deterministic workflow step waits for draft PR
   checks for the pushed commit. Red or timed-out CI aborts before any LLM
   verification work spends tokens.

## Why Header Injection Is Not Enough

Git branch policy is not visible in the request headers. A smart-HTTP push is
a `POST /<owner>/<repo>.git/git-receive-pack`; the updated refs are pkt-lines
inside the request body. A proxy that only rewrites headers can authenticate a
push, but it cannot enforce "only `refs/heads/glimmung/issue-...`". The proxy
must parse the receive-pack command section or delegate to an equivalent
dynamic GitHub branch ruleset. The current architecture assumes receive-pack
parsing because the allowed branch is per run.

## Receive-Pack Enforcement Contract

The proxy must parse the command prelude before packfile bytes:

- Accept only updates whose ref is exactly the policy's allowed
  `refs/heads/<branch>`.
- Reject attempts to update, create, or delete any other ref.
- Reject delete updates to the allowed branch unless final cleanup is using a
  separate server-side cleanup path.
- Reject pushes with zero parseable commands.
- Treat parse errors and oversized unparsed preludes as policy failures, not
  pass-through cases.

The current proxy buffers the request body behind an explicit aiohttp
`client_max_size` cap before forwarding. That is acceptable for the first
production path because failed oversize pushes fail closed. A future streaming
optimization may read only enough pkt-lines to make the policy decision, then
forward the complete request body to GitHub after approval.

## Non-Goals

- Do not let the agent call Glimmung callback endpoints directly.
- Do not expose the GitHub App installation token to the agent.
- Do not rely on prompt wording as the safety boundary.
- Do not make Ambience own the enforcement. Ambience can pre-create branches,
  open draft PRs, and wait for CI, but Glimmung owns native-runner security.

## Implementation

The implemented path has four pieces:

- `nativeJobEnv` mints `GLIMMUNG_GITHUB_PUSH_POLICY_TOKEN` for managed
  `type: agent` jobs with a project repo and GitHub App private key.
- `glimmung-native-runner` installs that token as a local Git credential for
  `https://github.com`, then strips the token from the agent subprocess
  environment.
- Native agent jobs route `github.com` to `github-git-policy-proxy` through
  `hostAliases` and the provider-proxy CA bundle. Verification wrapper jobs do
  not get the `github.com` host alias, so their existing GitHub API/token flows
  are not intercepted.
- `glimmung_api_proxy.github_proxy` validates the token, rejects disallowed
  receive-pack ref updates, mints a server-side GitHub App installation token,
  and forwards allowed Git traffic to real GitHub.
