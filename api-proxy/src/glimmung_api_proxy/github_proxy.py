from __future__ import annotations

import asyncio
import base64
from datetime import datetime, timedelta, timezone
import logging
import os
import time
from typing import Any

from aiohttp import web
import httpx
import jwt

from .github_policy import (
    PolicyError,
    enforce_receive_pack_policy,
    parse_basic_policy_token,
    repository_from_git_path,
    validate_policy_token,
)

log = logging.getLogger(__name__)

HOP_BY_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}


class InstallationTokenCache:
    def __init__(self, *, app_id: str, installation_id: str, private_key: str) -> None:
        self._app_id = app_id.strip()
        self._installation_id = installation_id.strip()
        self._private_key = private_key
        self._token = ""
        self._expires_at = 0.0
        self._lock = asyncio.Lock()

    async def token(self) -> str:
        async with self._lock:
            if self._token and self._expires_at - time.time() > 300:
                return self._token
            now = datetime.now(timezone.utc)
            app_jwt = jwt.encode(
                {
                    "iat": int((now - timedelta(seconds=60)).timestamp()),
                    "exp": int((now + timedelta(minutes=9)).timestamp()),
                    "iss": self._app_id,
                },
                self._private_key,
                algorithm="RS256",
            )
            url = f"https://api.github.com/app/installations/{self._installation_id}/access_tokens"
            async with httpx.AsyncClient(timeout=30.0) as client:
                resp = await client.post(
                    url,
                    headers={
                        "Authorization": f"Bearer {app_jwt}",
                        "Accept": "application/vnd.github+json",
                        "X-GitHub-Api-Version": "2022-11-28",
                    },
                )
            if resp.status_code >= 400:
                raise RuntimeError(f"GitHub access_tokens returned {resp.status_code}: {resp.text}")
            payload = resp.json()
            token = payload.get("token")
            expires_at = payload.get("expires_at")
            if not isinstance(token, str) or not token:
                raise RuntimeError("GitHub access_tokens response did not include token")
            self._token = token
            self._expires_at = _parse_github_time(expires_at)
            return token


class GitHubPolicyProxy:
    def __init__(self, *, signing_key: str, token_cache: InstallationTokenCache) -> None:
        self._signing_key = signing_key
        self._token_cache = token_cache

    async def handle(self, request: web.Request) -> web.StreamResponse:
        try:
            repo = repository_from_git_path(request.path)
            policy_token = parse_basic_policy_token(request.headers.get("authorization"))
            policy = validate_policy_token(policy_token, self._signing_key)
            if repo.lower() != str(policy["repo"]).lower():
                raise PolicyError(403, f"policy repo {policy['repo']} does not permit {repo}")
            body = await request.read()
            if request.method.upper() == "POST" and request.path.endswith("/git-receive-pack"):
                enforce_receive_pack_policy(body, str(policy["ref"]))
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
        upstream_token = await self._token_cache.token()
        auth = base64.b64encode(f"x-access-token:{upstream_token}".encode()).decode()
        upstream_url = "https://github.com" + request.path_qs
        headers = {}
        for key, value in request.headers.items():
            lower = key.lower()
            if lower in HOP_BY_HOP_HEADERS or lower in {"host", "authorization", "content-length"}:
                continue
            headers[key] = value
        headers["Authorization"] = "Basic " + auth
        headers["Host"] = "github.com"
        async with httpx.AsyncClient(timeout=None, follow_redirects=False) as client:
            resp = await client.request(request.method, upstream_url, headers=headers, content=body)
        response_headers = {}
        for key, value in resp.headers.items():
            lower = key.lower()
            if lower in HOP_BY_HOP_HEADERS or lower == "content-length":
                continue
            response_headers[key] = value
        return web.Response(status=resp.status_code, body=resp.content, headers=response_headers)


def create_app() -> web.Application:
    app_id = _required_env("GITHUB_APP_ID")
    installation_id = _required_env("GITHUB_APP_INSTALLATION_ID")
    private_key = _required_env("GITHUB_APP_PRIVATE_KEY")
    proxy = GitHubPolicyProxy(
        signing_key=private_key,
        token_cache=InstallationTokenCache(
            app_id=app_id,
            installation_id=installation_id,
            private_key=private_key,
        ),
    )
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


def _required_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


def _parse_github_time(value: Any) -> float:
    if isinstance(value, str) and value:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    return time.time() + 3600


if __name__ == "__main__":
    main()
