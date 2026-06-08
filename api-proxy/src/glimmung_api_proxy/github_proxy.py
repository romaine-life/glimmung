from __future__ import annotations

import logging
import os
from typing import Any

from aiohttp import web
import httpx

from .github_policy import (
    PolicyError,
    enforce_receive_pack_policy,
    forward_request_headers,
    forward_response_headers,
    repository_from_git_path,
)

log = logging.getLogger(__name__)

K8S_HOST = os.environ.get("KUBERNETES_SERVICE_HOST", "kubernetes.default.svc")
K8S_PORT = os.environ.get("KUBERNETES_SERVICE_PORT", "443")
K8S_TOKEN_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/token"
K8S_CA_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"


def get_k8s_client() -> httpx.AsyncClient:
    if os.path.exists(K8S_TOKEN_PATH):
        with open(K8S_TOKEN_PATH, "r", encoding="utf-8") as f:
            token = f.read().strip()
    else:
        token = ""

    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/json",
    }
    verify = K8S_CA_PATH if os.path.exists(K8S_CA_PATH) else False
    return httpx.AsyncClient(
        base_url=f"https://{K8S_HOST}:{K8S_PORT}",
        headers=headers,
        verify=verify,
        timeout=10.0,
    )


async def get_pod_annotations(client_ip: str) -> dict[str, str]:
    if not client_ip:
        raise PolicyError(400, "missing client IP")

    async with get_k8s_client() as client:
        # Query pod across all namespaces matching status.podIP
        url = f"/api/v1/pods?fieldSelector=status.podIP={client_ip}"
        resp = await client.get(url)
        if resp.status_code >= 400:
            log.error("Kubernetes API returned %d: %s", resp.status_code, resp.text)
            raise PolicyError(500, "failed to query Kubernetes API for client pod")

        data = resp.json()
        items = data.get("items", [])
        if not items:
            raise PolicyError(403, f"no Kubernetes pod found for client IP {client_ip}")

        pod = items[0]
        metadata = pod.get("metadata", {})
        annotations = metadata.get("annotations", {})
        return annotations


class GitHubPolicyProxy:
    async def handle(self, request: web.Request) -> web.StreamResponse:
        try:
            repo = repository_from_git_path(request.path)

            client_ip = request.headers.get("x-glimmung-source-ip") or request.remote
            if not client_ip:
                raise PolicyError(400, "unable to resolve client IP")

            annotations = await get_pod_annotations(client_ip)
            policy_repo = annotations.get("glimmung.romaine.life/github-policy-repo")
            policy_ref = annotations.get("glimmung.romaine.life/github-policy-ref")

            if not policy_repo or not policy_ref:
                raise PolicyError(403, "client pod is missing github policy annotations")

            if repo.lower() != policy_repo.lower():
                raise PolicyError(403, f"policy repo {policy_repo} does not permit {repo}")

            body = await request.read()
            if request.method.upper() == "POST" and request.path.endswith("/git-receive-pack"):
                enforce_receive_pack_policy(body, policy_ref)

            return await self._forward(request, body)
        except PolicyError as exc:
            log.warning("github policy rejected %s %s: %s", request.method, request.path_qs, exc.message)
            headers = {}
            if exc.status == 401:
                headers["WWW-Authenticate"] = 'Basic realm="Glimmung GitHub policy"'
            return web.Response(status=exc.status, text="remote: " + exc.message + "\n", headers=headers)
        except Exception:
            log.exception("github policy proxy failed for %s %s", request.method, request.path_qs)
            return web.Response(status=502, text="remote: Glimmung GitHub policy proxy failed\n")

    async def _forward(self, request: web.Request, body: bytes) -> web.StreamResponse:
        upstream_url = "https://github.com" + request.path_qs
        headers = forward_request_headers(request.headers.items())

        async with httpx.AsyncClient(timeout=None, follow_redirects=False) as client:
            resp = await client.request(request.method, upstream_url, headers=headers, content=body)

        response_headers = forward_response_headers(resp.headers.items())
        return web.Response(status=resp.status_code, body=resp.content, headers=response_headers)


def create_app() -> web.Application:
    proxy = GitHubPolicyProxy()
    app = web.Application(client_max_size=int(os.environ.get("GITHUB_POLICY_PROXY_MAX_BODY_BYTES", str(512 * 1024 * 1024))))
    app.router.add_get("/healthz", lambda _: web.Response(text="ok\n"))
    app.router.add_get("/metrics", lambda _: web.Response(text="", content_type="text/plain"))
    app.router.add_route("*", "/{tail:.*}", proxy.handle)
    return app


def main() -> None:
    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    port = int(os.environ.get("GITHUB_POLICY_PROXY_PORT", "8080"))
    web.run_app(create_app(), host="0.0.0.0", port=port)


if __name__ == "__main__":
    main()
