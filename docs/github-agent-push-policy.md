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
   not a GitHub token. It is a Glimmung push-policy token containing at least:
   repo, allowed branch ref, run id, issue number, expiry, and key id.
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

The proxy may buffer the command prelude, but it should not require loading an
unbounded packfile into memory. A streaming implementation should read enough
pkt-lines to make the policy decision, then forward the complete request body
to GitHub after approval.

## Non-Goals

- Do not let the agent call Glimmung callback endpoints directly.
- Do not expose the GitHub App installation token to the agent.
- Do not rely on prompt wording as the safety boundary.
- Do not make Ambience own the enforcement. Ambience can pre-create branches,
  open draft PRs, and wait for CI, but Glimmung owns native-runner security.

## Current Staging

This branch implements the native-runner side of the contract: managed
post-commit hook installation and agent subprocess environment filtering. The
remaining work is the GitHub policy token minting and the `github.com` policy
proxy that validates that token and parses receive-pack.
