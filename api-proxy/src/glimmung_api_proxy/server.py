"""Envoy ext_proc service that injects managed OAuth into outbound requests.

The Envoy listener in front of the provider API calls this service via the
``ExternalProcessor.Process`` bidirectional stream once per HTTP transaction.
We act on two messages per stream:

  - ``request_headers``: if the inbound ``authorization`` header is the
    bootstrap placeholder ``Bearer managed-by-glimmung``, swap it for
    the current real OAuth access token from cache. If it's anything else
    (typically a worker_jwt for the v2 remote-control bridge — see below),
    pass the request through untouched.
  - ``response_headers``: peek ``:status``. On 401 *for a request we
    actually injected into*, kick off a background refresh — but only if
    one isn't already in flight (single-flight guard prevents thundering-
    herd rotation storms when many concurrent requests all see 401).
    Envoy's per-route retry policy resends the request; the retry's
    request_headers callback awaits the in-flight refresh task before
    injecting, so the retry always sees a fresh token.

Why two distinct credentials in claude-code:
  * OAuth access token: long-lived (8h), pod-resident only as a placeholder,
    real value lives in this proxy's cache. Used for /v1/messages,
    /v1/code/sessions, /v1/code/sessions/{id}/bridge, /archive, /events,
    /api/oauth/*, /api/claude_code/*, etc.
  * worker_jwt: short-lived, returned by the /bridge POST response, kept
    only in claude-code memory. Used for the v2 remote-control bridge
    endpoints: GET /v1/code/sessions/{id}/worker, PUT same, GET
    /v1/code/sessions/{id}/worker/events/stream. These endpoints reject
    the OAuth Bearer outright with 401, so it's load-bearing that the
    proxy NOT clobber the Authorization on these calls.

State:
  - ``_cached_access`` / ``_cached_refresh``: in-memory copy of the latest
    tokens. Initialized lazily from the mounted credentials.json the first
    time we need them, refreshed in place by ``_refresh()``.
  - ``_refresh_task``: most recent refresh task. Its presence + not done()
    is the single-flight token; ``_get_access_token`` awaits it when
    ``_access_invalidated`` is set.
  - ``_lock``: serializes the actual rotation HTTP call so concurrent waiters
    all see the same fresh token (and so a stale-file reload in ``_refresh``
    is atomic with the rotate).

KV write failure is non-fatal: see the comment on ``_persist_to_kv``.

Anthropic-side gating gotcha (burned us during rollout verification):
api.anthropic.com's /v1/messages endpoint refuses subscription OAuth tokens
unless TWO request headers are set, and the error messages are misleading:

  - Without ``anthropic-beta: oauth-2025-04-20`` →
    401 "OAuth authentication is currently not supported" (it IS supported,
    just gated behind the beta opt-in).
  - Without ``anthropic-dangerous-direct-browser-access: true`` →
    401 "Invalid authentication credentials" even on a freshly-minted token.

claude-code itself sends both headers natively, so traffic from session
pods works without this proxy adding them. We do NOT inject these here:
doing so would mask a future claude-code header change rather than
surfacing it. If you're synthetically curling api.anthropic.com to test
this proxy, set both headers; if a real claude-code session 401s when
synthetic curls succeed, the gate has moved and claude-code needs a bump.
The beta string above is hardcoded in claude-code's bundled JS as of
April 2026 — Anthropic rotating it would silently break direct callers
but not claude-code, which ships the matching value.

Codex uses the same proxy primitive with a different token authority and a
different pod-side auth shape. Session pods write a synthetic
``~/.codex/auth.json`` with ``auth_mode=chatgptAuthTokens`` and
``access_token=managed-by-glimmung``. In current Codex, that mode does
not proactively refresh from auth.json; it simply emits the bearer in API
headers. This proxy swaps the placeholder for the real ChatGPT access token,
overwrites ``ChatGPT-Account-ID`` from the centrally mounted auth.json, and
single-flight-refreshes against auth.openai.com on upstream 401.
"""
from __future__ import annotations

import asyncio
import base64
from dataclasses import dataclass
from datetime import datetime, timezone
import json
import logging
import os
import time
from typing import Any, AsyncIterator

import grpc
import httpx
from azure.identity.aio import DefaultAzureCredential
from azure.keyvault.secrets.aio import SecretClient

from envoy.service.ext_proc.v3 import external_processor_pb2 as ext_proc_pb2
from envoy.service.ext_proc.v3 import external_processor_pb2_grpc as ext_proc_grpc
from envoy.config.core.v3 import base_pb2
from envoy.type.v3 import http_status_pb2

from .metrics import (
    record_ext_proc_request,
    record_kv_persist,
    record_refresh,
    record_single_flight_wait,
    record_upstream_status,
)

log = logging.getLogger(__name__)

# Hardcoded into Claude Code's bundled JS. Two distinct client_ids ship in
# the bundle: 22422756-... is paired with the older console.anthropic.com
# endpoint, 9d1c250a-... with platform.claude.com (our token URL). Tied
# here by the MANUAL_REDIRECT_URL/TOKEN_URL pairing in cli.js. The token
# URL is intentionally NOT routed through the proxy itself — the proxy
# fronts api.anthropic.com, not platform.claude.com.
ANTHROPIC_CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
ANTHROPIC_TOKEN_URL = "https://platform.claude.com/v1/oauth/token"

# Codex ChatGPT OAuth constants from openai/codex's login crate:
# codex-rs/login/src/auth/manager.rs.
CODEX_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
CODEX_TOKEN_URL = "https://auth.openai.com/oauth/token"

# Gemini OAuth constants
GEMINI_CLIENT_ID = ""
GEMINI_CLIENT_SECRET = ""
GEMINI_TOKEN_URL = "https://oauth2.googleapis.com/token"

# The session launchers write this placeholder into
# ~/.claude/.credentials.json's accessToken (and matching refreshToken).
# Used as the discriminator for "this is a request that wants OAuth-
# Bearer injection" — anything else (worker_jwt, missing, future
# unknowns) passes through with its Authorization untouched.
PLACEHOLDER_BEARER = os.environ.get(
    "PROXY_PLACEHOLDER_BEARER",
    "Bearer managed-by-glimmung",
)

@dataclass(frozen=True)
class ProxyConfig:
    provider: str
    credentials_file: str
    token_url: str
    client_id: str
    kv_secret_name: str
    client_secret: str | None = None
    account_header: str | None = None
    fedramp_header: str | None = None
    patch_last_refresh: bool = False


def _config_from_env() -> ProxyConfig:
    provider = os.environ.get("PROXY_PROVIDER", "claude").strip().lower()
    if provider == "codex":
        return ProxyConfig(
            provider="codex",
            credentials_file=os.environ.get(
                "CODEX_CREDENTIALS_FILE", "/etc/codex-credentials/auth.json"
            ),
            token_url=os.environ.get("CODEX_TOKEN_URL", CODEX_TOKEN_URL),
            client_id=os.environ.get("CODEX_CLIENT_ID", CODEX_CLIENT_ID),
            kv_secret_name=os.environ.get("CODEX_CREDENTIALS_KV_KEY", "codex-credentials"),
            account_header="ChatGPT-Account-ID",
            fedramp_header="X-OpenAI-Fedramp",
            patch_last_refresh=True,
        )
    if provider == "gemini":
        return ProxyConfig(
            provider="gemini",
            credentials_file=os.environ.get(
                "GEMINI_CREDENTIALS_FILE", "/etc/gemini-credentials/settings.json"
            ),
            token_url=os.environ.get("GEMINI_TOKEN_URL", GEMINI_TOKEN_URL),
            client_id=os.environ.get("GEMINI_CLIENT_ID", GEMINI_CLIENT_ID),
            client_secret=os.environ.get("GEMINI_CLIENT_SECRET", GEMINI_CLIENT_SECRET),
            kv_secret_name=os.environ.get("GEMINI_CREDENTIALS_KV_KEY", "gemini-credentials"),
        )
    if provider not in ("", "claude"):
        log.warning("unknown PROXY_PROVIDER=%r; falling back to claude", provider)
    return ProxyConfig(
        provider="claude",
        credentials_file=os.environ.get(
            "CLAUDE_CREDENTIALS_FILE", "/etc/claude-credentials/credentials.json"
        ),
        token_url=os.environ.get("CLAUDE_TOKEN_URL", ANTHROPIC_TOKEN_URL),
        client_id=os.environ.get("CLAUDE_CLIENT_ID", ANTHROPIC_CLIENT_ID),
        kv_secret_name=os.environ.get("CLAUDE_CREDENTIALS_KV_KEY", "claude-code-credentials"),
    )


def _walk_for(blob: Any, names: tuple[str, ...]) -> str | None:
    if not isinstance(blob, dict):
        return None
    for k, v in blob.items():
        if k in names and isinstance(v, str):
            return v
        if isinstance(v, dict):
            found = _walk_for(v, names)
            if found:
                return found
    return None


def _patch_blob(
    blob: dict[str, Any],
    new_access: str,
    new_refresh: str,
    expires_in: int,
    *,
    new_id: str | None = None,
    patch_last_refresh: bool = False,
) -> dict[str, Any]:
    expires_at_ms = int((time.time() + expires_in) * 1000)
    last_refresh = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    out = json.loads(json.dumps(blob))

    def walk(node: Any) -> None:
        if not isinstance(node, dict):
            return
        for key in list(node.keys()):
            if key in ("accessToken", "access_token"):
                node[key] = new_access
            elif key in ("refreshToken", "refresh_token"):
                node[key] = new_refresh
            elif new_id is not None and key in ("idToken", "id_token"):
                node[key] = new_id
            elif key in ("expiresAt", "expires_at"):
                node[key] = expires_at_ms
            elif patch_last_refresh and key == "last_refresh":
                node[key] = last_refresh
            elif isinstance(node[key], dict):
                walk(node[key])

    walk(out)
    if patch_last_refresh:
        out["last_refresh"] = last_refresh
    return out


def _jwt_exp_ms(token: str | None) -> int | None:
    claims = _jwt_payload(token)
    exp = claims.get("exp")
    if isinstance(exp, (int, float)):
        return int(exp * 1000)
    return None


def _jwt_payload(token: str | None) -> dict[str, Any]:
    if not token:
        return {}
    parts = token.split(".")
    if len(parts) < 2:
        return {}
    payload = parts[1]
    padding = "=" * (-len(payload) % 4)
    try:
        decoded = base64.urlsafe_b64decode((payload + padding).encode())
        claims = json.loads(decoded)
    except Exception:
        return {}
    return claims if isinstance(claims, dict) else {}


def _iso_ms(value: str | None) -> int | None:
    if not value:
        return None
    try:
        normalized = value.replace("Z", "+00:00")
        dt = datetime.fromisoformat(normalized)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return int(dt.timestamp() * 1000)
    except ValueError:
        return None


# User-facing copy for known OAuth refresh-failure reasons. The /health
# endpoint surfaces these strings directly; the orchestrator's
# session.status:failed banner copies them verbatim into the transcript.
# Keep the strings actionable — every entry should answer "what does the
# user do next" since the action affordance ("Re-sign-in to ...") is
# generic.
_REFRESH_FAILURE_TEXT = {
    "refresh_token_reused": "Sign-in expired. The refresh token has already been used; re-authenticate to restore service.",
    "invalid_grant": "Sign-in expired. Re-authenticate to restore service.",
    "invalid_request": "Sign-in could not be refreshed. Re-authenticate to restore service.",
    "unauthorized_client": "Sign-in is not authorized to refresh. Re-authenticate to restore service.",
}


def _classify_refresh_failure(resp: httpx.Response) -> tuple[str, str]:
    """Extract a (reason, text) tuple from an OAuth /token error response.

    The upstream body shape is the standard OAuth error envelope:
        {"error": {"code": "refresh_token_reused", "message": "..."}}
    For non-JSON bodies (rare; upstream proxies misbehaving), fall back
    to the HTTP status as the reason. The reason field is what feeds
    the orchestrator's metric label and Layer 1 row; text is what shows
    in the transcript banner.
    """
    reason = ""
    text = ""
    try:
        body = resp.json()
    except Exception:
        body = None
    if isinstance(body, dict):
        err = body.get("error")
        if isinstance(err, dict):
            code = err.get("code")
            message = err.get("message")
            if isinstance(code, str) and code:
                reason = code
            if isinstance(message, str) and message:
                text = message
        elif isinstance(err, str) and err:
            reason = err
    if not reason:
        reason = f"http_{resp.status_code}"
    if not text:
        text = _REFRESH_FAILURE_TEXT.get(reason, "Sign-in could not be refreshed. Re-authenticate to restore service.")
    else:
        # If we got an upstream message AND the reason is one we have
        # canonical copy for, prefer the canonical copy — the upstream
        # message is often referrer-style ("Please try signing in again.")
        # and lands awkwardly in the SPA's banner.
        canonical = _REFRESH_FAILURE_TEXT.get(reason)
        if canonical:
            text = canonical
    return reason, text


class AuthInjector(ext_proc_grpc.ExternalProcessorServicer):
    def __init__(self, config: ProxyConfig | None = None) -> None:
        self._config = config or _config_from_env()
        self._cached_access: str | None = None
        self._cached_refresh: str | None = None
        self._cached_account_id: str | None = None
        self._cached_fedramp: bool = False
        self._cached_blob: dict[str, Any] | None = None
        self._lock = asyncio.Lock()
        # Set when an upstream 401 is observed for the current cached
        # token. The next request_headers callback awaits the in-flight
        # refresh task before injecting; once refresh updates _cached_access
        # we clear the flag so subsequent requests don't block.
        self._access_invalidated = False
        # Most recent refresh task, retained as a single-flight handle:
        # _on_response_headers consults `_refresh_task is None or .done()`
        # before scheduling a new one, and _get_access_token awaits it
        # when the cached token is invalidated. Without this dedupe, N
        # concurrent 401s would each schedule their own _refresh(), each
        # successive rotation would single-use-invalidate its predecessor's
        # refresh token, and the proxy logs would show a "rotation storm"
        # of five+ successful rotations in two seconds.
        self._refresh_task: asyncio.Task[None] | None = None
        self._kv_url = os.environ.get("AZURE_KEYVAULT_URL", "")
        # Health snapshot for Glimmung's provider-credential surface.
        # The proxy stays stateless: durable failure projection belongs in
        # Glimmung's control plane, not in this sidecar.
        self._health_last_attempted_at: float | None = None
        self._health_last_succeeded_at: float | None = None
        self._health_last_result: str = "unknown"
        self._health_last_reason: str = ""
        self._health_last_text: str = ""
        self._health_attempt_id: int = 0
        log.info(
            "starting %s auth injector (credentials=%s, kv_secret=%s)",
            self._config.provider,
            self._config.credentials_file,
            self._config.kv_secret_name,
        )

    async def Process(
        self,
        request_iterator: AsyncIterator[ext_proc_pb2.ProcessingRequest],
        context: grpc.aio.ServicerContext,
    ) -> AsyncIterator[ext_proc_pb2.ProcessingResponse]:
        # Per-stream state: did we inject our OAuth Bearer on the request
        # side of this transaction? If not (e.g. the call carried a
        # worker_jwt), we must not interpret a 401 here as "our cached
        # token went stale" — it's about the inbound credential we
        # didn't touch, not ours.
        injected = False
        async for req in request_iterator:
            kind = req.WhichOneof("request")
            if kind == "request_headers":
                response, injected = await self._on_request_headers(req.request_headers)
                yield response
            elif kind == "response_headers":
                yield await self._on_response_headers(req.response_headers, injected)
            else:
                # Body / trailers / unknown: pass through unmodified. We
                # configured the filter to skip body streaming, so the only
                # path that lands here is the trailers message envoy emits
                # at end-of-stream.
                yield ext_proc_pb2.ProcessingResponse()

    async def _on_request_headers(
        self, msg: ext_proc_pb2.HttpHeaders
    ) -> tuple[ext_proc_pb2.ProcessingResponse, bool]:
        inbound = _peek_header(msg, "authorization")
        # Pass-through path: the caller is using a credential we didn't
        # mint and shouldn't touch. claude-code's v2 remote-control bridge
        # uses worker_jwt (sk-ant-si-…) on /v1/code/sessions/{id}/worker*
        # endpoints, returned to it from the prior /bridge POST response.
        # If we overwrite that Authorization with our OAuth Bearer the
        # /worker endpoint 401s — Anthropic rejects the OAuth token there.
        if inbound != PLACEHOLDER_BEARER:
            record_ext_proc_request("passthrough")
            return (
                ext_proc_pb2.ProcessingResponse(
                    request_headers=ext_proc_pb2.HeadersResponse()
                ),
                False,
            )
        token = await self._get_access_token()
        if token == "missing":
            record_ext_proc_request("missing_token")
        else:
            record_ext_proc_request("injected")
        set_headers = [
            base_pb2.HeaderValueOption(
                header=base_pb2.HeaderValue(
                    key="authorization", raw_value=f"Bearer {token}".encode()
                ),
                append_action=base_pb2.HeaderValueOption.OVERWRITE_IF_EXISTS_OR_ADD,
            )
        ]
        if self._config.account_header and self._cached_account_id:
            set_headers.append(
                base_pb2.HeaderValueOption(
                    header=base_pb2.HeaderValue(
                        key=self._config.account_header,
                        raw_value=self._cached_account_id.encode(),
                    ),
                    append_action=base_pb2.HeaderValueOption.OVERWRITE_IF_EXISTS_OR_ADD,
                )
            )
        if self._config.fedramp_header and self._cached_fedramp:
            set_headers.append(
                base_pb2.HeaderValueOption(
                    header=base_pb2.HeaderValue(
                        key=self._config.fedramp_header,
                        raw_value=b"true",
                    ),
                    append_action=base_pb2.HeaderValueOption.OVERWRITE_IF_EXISTS_OR_ADD,
                )
            )
        headers_resp = ext_proc_pb2.HeadersResponse(
            response=ext_proc_pb2.CommonResponse(
                header_mutation=ext_proc_pb2.HeaderMutation(
                    set_headers=set_headers,
                    # Whatever the pod sent for x-api-key would conflict
                    # with our Bearer auth and make the provider 401. Strip.
                    remove_headers=["x-api-key"],
                ),
            ),
        )
        return ext_proc_pb2.ProcessingResponse(request_headers=headers_resp), True

    async def _on_response_headers(
        self, msg: ext_proc_pb2.HttpHeaders, was_injected: bool
    ) -> ext_proc_pb2.ProcessingResponse:
        # Only treat 401 as a refresh trigger for requests we actually
        # injected. A 401 on a request that came in with a worker_jwt
        # we passed through is about that JWT (expired/revoked/etc.),
        # not about our cached OAuth token; rotating in response would
        # spuriously churn tokens and could trigger the storm pattern
        # if /worker endpoints loop on 401.
        if was_injected:
            status = _peek_status(msg)
            record_upstream_status(status)
            if status == 401:
                self._access_invalidated = True
                # Single-flight: only schedule a refresh if one isn't
                # already running. Subsequent 401s in the same burst
                # piggy-back on the in-flight task via
                # _get_access_token.await(self._refresh_task).
                if self._refresh_task is None or self._refresh_task.done():
                    self._refresh_task = asyncio.create_task(self._refresh())
        return ext_proc_pb2.ProcessingResponse(response_headers=ext_proc_pb2.HeadersResponse())

    async def _get_access_token(self) -> str:
        if self._cached_access is not None and not self._access_invalidated:
            return self._cached_access
        # The cache is poisoned (or empty). If a refresh is already in
        # flight, wait for it; awaiting the task guarantees we see the
        # fresh _cached_access on return. The earlier "acquire-and-release
        # the lock" pattern raced: if this coroutine grabbed the lock
        # before the queued _refresh task did, it returned a stale token
        # and the upstream re-401'd, scheduling yet another refresh.
        task = self._refresh_task
        if task is not None and not task.done():
            record_single_flight_wait()
            try:
                await task
            except Exception:
                # _refresh logs and swallows; we just fall through and
                # serve whatever's in cache (worst case: placeholder, and
                # envoy's retry-on-401 will trigger another refresh round).
                pass
        async with self._lock:
            if self._cached_access is None:
                self._reload_from_file()
        # _reload_from_file may have failed to find a token; surface as a
        # placeholder — Envoy will get 401 from upstream and retry, and
        # the retry path will trigger a refresh.
        return self._cached_access or "missing"

    def _blob_freshness_ms(self, blob: dict[str, Any]) -> int | None:
        """Return the best available freshness marker for a credential blob.

        Claude stores expiresAt milliseconds. Codex stores last_refresh in
        newer auth.json files and the access token itself is a JWT with exp.
        We use the greatest comparable marker so a newly re-seeded file can
        replace memory, while an old ESO mount cannot clobber a just-rotated
        in-memory refresh chain.
        """
        if not isinstance(blob, dict):
            return None
        candidates: list[int] = []

        def walk(node: Any) -> None:
            if not isinstance(node, dict):
                return
            for k, v in node.items():
                if k in ("expiresAt", "expires_at") and isinstance(v, int):
                    candidates.append(v)
                elif k == "last_refresh" and isinstance(v, str):
                    parsed = _iso_ms(v)
                    if parsed is not None:
                        candidates.append(parsed)
                elif k in ("accessToken", "access_token") and isinstance(v, str):
                    parsed = _jwt_exp_ms(v)
                    if parsed is not None:
                        candidates.append(parsed)
                elif isinstance(v, dict):
                    walk(v)

        walk(blob)
        return max(candidates) if candidates else None

    def _blob_fedramp(self, blob: dict[str, Any]) -> bool:
        for k, v in blob.items():
            if k == "chatgpt_account_is_fedramp" and isinstance(v, bool):
                return v
            if isinstance(v, dict):
                if self._blob_fedramp(v):
                    return True
            elif k in ("idToken", "id_token") and isinstance(v, str):
                payload = _jwt_payload(v)
                auth = payload.get("https://api.openai.com/auth")
                if isinstance(auth, dict) and auth.get("chatgpt_account_is_fedramp") is True:
                    return True
        return False

    def _blob_account_id(self, blob: dict[str, Any]) -> str | None:
        found = _walk_for(blob, ("account_id", "chatgpt_account_id"))
        if found:
            return found
        for k, v in blob.items():
            if isinstance(v, dict):
                found = self._blob_account_id(v)
                if found:
                    return found
            elif k in ("idToken", "id_token") and isinstance(v, str):
                payload = _jwt_payload(v)
                auth = payload.get("https://api.openai.com/auth")
                if isinstance(auth, dict):
                    account_id = auth.get("chatgpt_account_id")
                    if isinstance(account_id, str) and account_id:
                        return account_id
        return None

    def _cached_freshness_ms(self) -> int | None:
        return self._blob_freshness_ms(self._cached_blob) if self._cached_blob else None

    def _reload_from_file(self) -> None:
        """Pull the on-disk blob into the in-memory cache, but only if the
        file is strictly fresher than memory.

        Skipping a stale-file reload is the load-bearing invariant: if we
        just rotated in-process and KV+ESO haven't propagated back yet,
        the file holds pre-rotation tokens whose refresh has already been
        single-use-invalidated by Anthropic. Clobbering memory with that
        would make the next refresh 400 invalid_grant.
        """
        try:
            with open(self._config.credentials_file, "r", encoding="utf-8") as f:
                blob = json.load(f)
        except FileNotFoundError:
            log.error("credentials file %s not found; serving placeholder", self._config.credentials_file)
            return
        except Exception:
            log.exception("could not read credentials file %s", self._config.credentials_file)
            return
        file_access = _walk_for(blob, ("accessToken", "access_token"))
        file_refresh = _walk_for(blob, ("refreshToken", "refresh_token"))
        file_account_id = self._blob_account_id(blob)
        file_freshness = self._blob_freshness_ms(blob)
        cached_freshness = self._cached_freshness_ms()
        if (
            cached_freshness is not None
            and file_freshness is not None
            and file_freshness <= cached_freshness
        ):
            return  # memory is at least as fresh
        if self._cached_access is not None and file_access == self._cached_access:
            return  # tokens match; nothing to do
        if self._cached_access is not None and file_freshness is None:
            log.warning(
                "skipping %s credential reload with no freshness marker; keeping memory",
                self._config.provider,
            )
            return
        self._cached_blob = blob
        self._cached_access = file_access
        self._cached_refresh = file_refresh
        self._cached_account_id = file_account_id
        self._cached_fedramp = self._blob_fedramp(blob)
        log.info(
            "loaded %s credentials from file (access prefix=%s, account=%s)",
            self._config.provider,
            (file_access or "")[:12],
            file_account_id or "none",
        )

    async def _refresh(self) -> None:
        async with self._lock:
            # Re-read the file under the lock: ESO may have mirrored a
            # newer KV value (e.g. someone re-seeded via "+ config sub")
            # and we should prefer that over rotating against the provider.
            self._reload_from_file()
            if self._cached_refresh is None:
                log.error("no refresh token available; cannot rotate")
                record_refresh("no_refresh_token")
                self._record_health_result("no_refresh_token", "no_refresh_token", "No refresh token available; the OAuth blob is missing or unreadable.")
                return
            log.info("calling %s to rotate %s token", self._config.token_url, self._config.provider)
            refresh_start = time.monotonic()
            self._health_last_attempted_at = time.time()
            self._health_attempt_id += 1
            try:
                async with httpx.AsyncClient(timeout=30.0) as http:
                    payload = {
                        "grant_type": "refresh_token",
                        "refresh_token": self._cached_refresh,
                        "client_id": self._config.client_id,
                    }
                    if self._config.client_secret is not None:
                        payload["client_secret"] = self._config.client_secret
                    resp = await http.post(
                        self._config.token_url,
                        json=payload,
                        headers={"Content-Type": "application/json"},
                    )
            except Exception:
                log.exception("refresh request crashed; keeping existing tokens")
                record_refresh("request_failed", time.monotonic() - refresh_start)
                self._record_health_result("request_failed", "request_failed", "Upstream OAuth token endpoint unreachable.")
                return
            if resp.status_code != 200:
                log.error("refresh failed: status=%s body=%s", resp.status_code, resp.text[:500])
                record_refresh("http_error", time.monotonic() - refresh_start)
                reason, text = _classify_refresh_failure(resp)
                self._record_health_result("http_error", reason, text)
                return
            data = resp.json()
            new_access = data["access_token"]
            new_refresh = data.get("refresh_token") or self._cached_refresh
            new_id = data.get("id_token")
            expires_in = int(data.get("expires_in", 3600))
            # Update in-memory state FIRST so concurrent waiters see the
            # fresh access token without depending on KV+ESO+kubelet.
            self._cached_access = new_access
            self._cached_refresh = new_refresh
            if self._cached_blob is not None:
                self._cached_blob = _patch_blob(
                    self._cached_blob,
                    new_access,
                    new_refresh,
                    expires_in,
                    new_id=new_id,
                    patch_last_refresh=self._config.patch_last_refresh,
                )
                self._cached_account_id = self._blob_account_id(self._cached_blob)
                self._cached_fedramp = self._blob_fedramp(self._cached_blob)
            self._access_invalidated = False
            log.info(
                "rotated %s successfully (access prefix=%s, expires in %ds)",
                self._config.provider,
                new_access[:12],
                expires_in,
            )
            record_refresh("success", time.monotonic() - refresh_start)
            self._health_last_succeeded_at = time.time()
            self._record_health_result("success", "", "")
            await self._persist_to_kv(expires_in)

    def _record_health_result(self, result: str, reason: str, text: str) -> None:
        """Record the outcome of a refresh attempt for the /health endpoint.

        result is the high-level outcome ("success" / "http_error" /
        "request_failed" / "no_refresh_token"); reason is the
        fine-grained label (e.g. "refresh_token_reused"); text is the
        user-facing string the orchestrator copies into a
        session.status:failed banner.
        """
        self._health_last_result = result
        self._health_last_reason = reason
        self._health_last_text = text

    def health_snapshot(self) -> dict[str, Any]:
        """Return the current refresh-health snapshot for /health/<provider>."""
        return {
            "provider": self._config.provider,
            "result": self._health_last_result,
            "reason": self._health_last_reason,
            "text": self._health_last_text,
            "last_attempted_at": self._health_last_attempted_at,
            "last_succeeded_at": self._health_last_succeeded_at,
            "attempt_id": self._health_attempt_id,
        }

    async def _persist_to_kv(self, expires_in: int) -> None:
        """Best-effort write of the rotated blob back to KV.

        Failure mode (KV write errors after a successful provider refresh)
        used to be a chain-killer in the cron design — the provider had
        already invalidated the old refresh token, but KV still held it.
        Here it's tolerable: in-memory state already serves the fresh
        access token to ongoing requests, and a future restart (rare,
        and not concurrent with a refresh storm) re-reads from the
        slightly-stale Secret without losing service. ESO will eventually
        re-mirror after the next successful rotation. No alert needed —
        just log and move on.
        """
        if not self._kv_url or self._cached_blob is None:
            record_kv_persist("skipped")
            return
        try:
            cred = DefaultAzureCredential()
            try:
                async with SecretClient(vault_url=self._kv_url, credential=cred) as kv:
                    await kv.set_secret(self._config.kv_secret_name, json.dumps(self._cached_blob))
                log.info(
                    "wrote rotated blob to %s/%s (expires in %ds)",
                    self._kv_url,
                    self._config.kv_secret_name,
                    expires_in,
                )
                record_kv_persist("success")
            finally:
                await cred.close()
        except Exception:
            log.exception("KV write failed; tokens stay in memory only")
            record_kv_persist("failure")


def _peek_header(msg: ext_proc_pb2.HttpHeaders, name: str) -> str | None:
    name_lower = name.lower()
    for h in msg.headers.headers:
        if h.key.lower() == name_lower:
            value = h.raw_value.decode() if h.raw_value else h.value
            return value
    return None


def _peek_status(msg: ext_proc_pb2.HttpHeaders) -> int | None:
    for h in msg.headers.headers:
        if h.key == ":status":
            value = h.raw_value.decode() if h.raw_value else h.value
            try:
                return int(value)
            except ValueError:
                return None
    return None


async def serve(port: int) -> tuple[grpc.aio.Server, AuthInjector]:
    """Boot the ext_proc grpc server and return both the server and the
    AuthInjector instance. The injector is returned so __main__ can wire
    its health_snapshot() into the metrics-server's /health endpoint.
    """
    config = _config_from_env()
    server = grpc.aio.server()
    injector = AuthInjector(config)
    ext_proc_grpc.add_ExternalProcessorServicer_to_server(injector, server)
    server.add_insecure_port(f"0.0.0.0:{port}")
    await server.start()
    log.info("%s ext_proc listening on 0.0.0.0:%d", config.provider, port)
    return server, injector


# Suppress unused-import warning: the http_status import is kept so that
# downstream protobuf descriptor resolution doesn't require eager loading
# from grpc internals if the module is dlopen'd before the deps register.
_ = http_status_pb2  # noqa: F401
