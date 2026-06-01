"""Prometheus instrumentation for the api-proxy ext_proc service.

The api-proxy is deployed twice — once for Anthropic (provider="anthropic")
and once for ChatGPT/Codex (provider="chatgpt-codex"). The two deployments
share this code and the ``provider`` label distinguishes their metrics at
scrape time so Grafana can show them on one dashboard.

Cardinality discipline matches the orchestrator's: bounded labels only.
``provider`` has two values; ``result`` is a small enum; ``status_class``
is the 2xx/3xx/4xx/5xx bucket. No per-call labels (request_id, user, etc.).

The HTTP listener is separate from the gRPC ext_proc listener so the
kube-prometheus-stack ServiceMonitor can scrape /metrics independently
without colliding with the ext_proc port.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
from typing import Any, Callable

from aiohttp import web
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest

log = logging.getLogger(__name__)

# Provider label is bound at module import time from PROXY_PROVIDER, the
# same env var server._config_from_env reads to choose credentials and
# token URL. Values match the server: "claude" or "codex". Setting it
# once avoids threading the label through every call site and keeps the
# series partitioned by deployment.
PROVIDER = (os.environ.get("PROXY_PROVIDER") or "claude").strip().lower() or "claude"

ext_proc_requests_total = Counter(
    "glimmung_api_proxy_requests_total",
    "Inbound request_headers messages handled by the ext_proc service.",
    ["provider", "outcome"],
)

upstream_status_total = Counter(
    "glimmung_api_proxy_upstream_status_total",
    "Upstream :status responses observed via response_headers.",
    ["provider", "status_class"],
)

upstream_401_total = Counter(
    "glimmung_api_proxy_upstream_401_total",
    "Upstream 401 responses on injected requests (the refresh-storm signature).",
    ["provider"],
)

token_refresh_total = Counter(
    "glimmung_api_proxy_token_refresh_total",
    "Token refresh attempts against the upstream OAuth endpoint.",
    ["provider", "result"],
)

refresh_duration_seconds = Histogram(
    "glimmung_api_proxy_refresh_duration_seconds",
    "Wall-clock duration of a token refresh attempt.",
    ["provider", "result"],
    buckets=(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30),
)

kv_persist_total = Counter(
    "glimmung_api_proxy_kv_persist_total",
    "Attempts to write a rotated blob back to Key Vault.",
    ["provider", "result"],
)

single_flight_waits_total = Counter(
    "glimmung_api_proxy_single_flight_waits_total",
    "Callers that awaited an already-in-flight refresh task instead of "
    "starting a new one (the single-flight dedupe success path).",
    ["provider"],
)


def record_ext_proc_request(outcome: str) -> None:
    """outcome is one of: injected, passthrough, missing_token."""
    ext_proc_requests_total.labels(provider=PROVIDER, outcome=outcome).inc()


def record_upstream_status(status: int | None) -> None:
    if status is None:
        return
    bucket = _status_class(status)
    upstream_status_total.labels(provider=PROVIDER, status_class=bucket).inc()
    if status == 401:
        upstream_401_total.labels(provider=PROVIDER).inc()


def record_refresh(result: str, duration_seconds: float | None = None) -> None:
    """result is one of: success, http_error, request_failed, no_refresh_token."""
    token_refresh_total.labels(provider=PROVIDER, result=result).inc()
    if duration_seconds is not None:
        refresh_duration_seconds.labels(provider=PROVIDER, result=result).observe(duration_seconds)


def record_kv_persist(result: str) -> None:
    """result is one of: success, failure, skipped."""
    kv_persist_total.labels(provider=PROVIDER, result=result).inc()


def record_single_flight_wait() -> None:
    single_flight_waits_total.labels(provider=PROVIDER).inc()


def _status_class(status: int) -> str:
    if 200 <= status < 300:
        return "2xx"
    if 300 <= status < 400:
        return "3xx"
    if 400 <= status < 500:
        return "4xx"
    if 500 <= status < 600:
        return "5xx"
    return "unknown"


async def _handle_metrics(_: web.Request) -> web.Response:
    return web.Response(body=generate_latest(), content_type=CONTENT_TYPE_LATEST.split(";")[0], charset="utf-8")


async def _handle_healthz(_: web.Request) -> web.Response:
    return web.Response(text="ok")


HealthSnapshotProvider = Callable[[], dict[str, Any]]


def _make_health_handler(snapshot: HealthSnapshotProvider) -> Callable[[web.Request], web.Response]:
    """Wrap a health-snapshot callable as an aiohttp handler.

    Schema:
        {
          "provider": "codex" | "claude",
          "result": "success" | "http_error" | "request_failed" | ...,
          "reason": "refresh_token_reused" | "" | ...,
          "text": "Sign-in expired. ...",
          "last_attempted_at": <unix-seconds float | null>,
          "last_succeeded_at": <unix-seconds float | null>,
          "attempt_id": <int>
        }
    Snapshot exceptions degrade to 503 so a future control-plane poller can
    distinguish "proxy unavailable" from a provider refresh failure.
    """
    def handler(_: web.Request) -> web.Response:
        try:
            payload = snapshot()
        except Exception:
            log.exception("health snapshot failed")
            return web.Response(status=503, text="snapshot unavailable")
        return web.Response(
            body=json.dumps(payload),
            content_type="application/json",
        )
    return handler


async def start_metrics_server(
    port: int,
    health_snapshot: HealthSnapshotProvider | None = None,
) -> web.AppRunner:
    app = web.Application()
    app.router.add_get("/metrics", _handle_metrics)
    app.router.add_get("/healthz", _handle_healthz)
    if health_snapshot is not None:
        # Route is /health/<provider> so a Grafana dashboard or a
        # multi-provider orchestrator can disambiguate which deployment
        # answered without parsing the body.
        app.router.add_get(f"/health/{PROVIDER}", _make_health_handler(health_snapshot))
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, host="0.0.0.0", port=port)
    await site.start()
    log.info("api-proxy metrics listening on 0.0.0.0:%d (provider=%s)", port, PROVIDER)
    return runner


__all__ = [
    "PROVIDER",
    "record_ext_proc_request",
    "record_kv_persist",
    "record_refresh",
    "record_single_flight_wait",
    "record_upstream_status",
    "start_metrics_server",
]
