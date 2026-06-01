import base64
import json
import sys
import tempfile
import types
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))


def install_proto_stubs() -> None:
    grpc = sys.modules.setdefault("grpc", types.ModuleType("grpc"))
    grpc.aio = types.SimpleNamespace(server=lambda *args, **kwargs: object())

    httpx = sys.modules.setdefault("httpx", types.ModuleType("httpx"))
    httpx.Response = object

    azure = sys.modules.setdefault("azure", types.ModuleType("azure"))
    azure.__path__ = []
    identity = sys.modules.setdefault("azure.identity", types.ModuleType("azure.identity"))
    identity.__path__ = []
    identity_aio = sys.modules.setdefault("azure.identity.aio", types.ModuleType("azure.identity.aio"))

    class DefaultAzureCredential:
        async def close(self) -> None:
            pass

    identity_aio.DefaultAzureCredential = DefaultAzureCredential
    keyvault = sys.modules.setdefault("azure.keyvault", types.ModuleType("azure.keyvault"))
    keyvault.__path__ = []
    secrets = sys.modules.setdefault("azure.keyvault.secrets", types.ModuleType("azure.keyvault.secrets"))
    secrets.__path__ = []
    secrets_aio = sys.modules.setdefault("azure.keyvault.secrets.aio", types.ModuleType("azure.keyvault.secrets.aio"))

    class SecretClient:
        def __init__(self, *args, **kwargs) -> None:
            pass

    secrets_aio.SecretClient = SecretClient

    aiohttp = sys.modules.setdefault("aiohttp", types.ModuleType("aiohttp"))
    web = types.ModuleType("aiohttp.web")
    web.Request = object
    web.Response = lambda *args, **kwargs: types.SimpleNamespace(args=args, kwargs=kwargs)
    web.Application = lambda: types.SimpleNamespace(router=types.SimpleNamespace(add_get=lambda *args, **kwargs: None))
    web.AppRunner = lambda app: types.SimpleNamespace(setup=lambda: None, cleanup=lambda: None)
    web.TCPSite = lambda *args, **kwargs: types.SimpleNamespace(start=lambda: None)
    aiohttp.web = web
    sys.modules["aiohttp.web"] = web

    prometheus = sys.modules.setdefault("prometheus_client", types.ModuleType("prometheus_client"))

    class _Metric:
        def __init__(self, *args, **kwargs) -> None:
            pass

        def labels(self, *args, **kwargs):
            return self

        def inc(self) -> None:
            pass

        def observe(self, _value: float) -> None:
            pass

    prometheus.CONTENT_TYPE_LATEST = "text/plain; version=0.0.4"
    prometheus.Counter = _Metric
    prometheus.Histogram = _Metric
    prometheus.generate_latest = lambda: b""

    for name in (
        "envoy",
        "envoy.service",
        "envoy.service.ext_proc",
        "envoy.service.ext_proc.v3",
        "envoy.config",
        "envoy.config.core",
        "envoy.config.core.v3",
        "envoy.type",
        "envoy.type.v3",
    ):
        pkg = sys.modules.setdefault(name, types.ModuleType(name))
        pkg.__path__ = []

    ext_proc_pb2_grpc = types.ModuleType(
        "envoy.service.ext_proc.v3.external_processor_pb2_grpc"
    )

    class ExternalProcessorServicer:
        pass

    ext_proc_pb2_grpc.ExternalProcessorServicer = ExternalProcessorServicer
    ext_proc_pb2_grpc.add_ExternalProcessorServicer_to_server = (
        lambda *args, **kwargs: None
    )
    sys.modules["envoy.service.ext_proc.v3.external_processor_pb2_grpc"] = (
        ext_proc_pb2_grpc
    )
    sys.modules["envoy.service.ext_proc.v3.external_processor_pb2"] = types.ModuleType(
        "envoy.service.ext_proc.v3.external_processor_pb2"
    )
    sys.modules["envoy.config.core.v3.base_pb2"] = types.ModuleType(
        "envoy.config.core.v3.base_pb2"
    )
    sys.modules["envoy.type.v3.http_status_pb2"] = types.ModuleType(
        "envoy.type.v3.http_status_pb2"
    )


install_proto_stubs()

from glimmung_api_proxy.server import (
    AuthInjector,
    ProxyConfig,
    _classify_refresh_failure,
    _patch_blob,
)


def jwt_with_claims(claims: dict) -> str:
    def encode(obj: dict) -> str:
        raw = json.dumps(obj, separators=(",", ":")).encode()
        return base64.urlsafe_b64encode(raw).decode().rstrip("=")

    return f"{encode({'alg': 'none', 'typ': 'JWT'})}.{encode(claims)}.signature"


def codex_config(credentials_file: str) -> ProxyConfig:
    return ProxyConfig(
        provider="codex",
        credentials_file=credentials_file,
        token_url="https://auth.openai.com/oauth/token",
        client_id="app_EMoamEEZ73f0CkXaXp7hrann",
        kv_secret_name="codex-credentials",
        account_header="ChatGPT-Account-ID",
        fedramp_header="X-OpenAI-Fedramp",
        patch_last_refresh=True,
    )


class ServerTests(unittest.TestCase):
    def test_patch_blob_updates_codex_tokens(self) -> None:
        blob = {
            "auth_mode": "chatgptAuthTokens",
            "tokens": {
                "id_token": "old-id",
                "access_token": "old-access",
                "refresh_token": "old-refresh",
                "account_id": "acct",
            },
            "last_refresh": "2026-01-01T00:00:00Z",
        }

        patched = _patch_blob(
            blob,
            "new-access",
            "new-refresh",
            3600,
            new_id="new-id",
            patch_last_refresh=True,
        )

        self.assertEqual(patched["tokens"]["id_token"], "new-id")
        self.assertEqual(patched["tokens"]["access_token"], "new-access")
        self.assertEqual(patched["tokens"]["refresh_token"], "new-refresh")
        self.assertNotEqual(patched["last_refresh"], blob["last_refresh"])

    def test_reload_extracts_codex_account_headers_from_id_token(self) -> None:
        id_token = jwt_with_claims(
            {
                "https://api.openai.com/auth": {
                    "chatgpt_account_id": "acct_123",
                    "chatgpt_account_is_fedramp": True,
                }
            }
        )
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "auth.json"
            path.write_text(
                json.dumps(
                    {
                        "tokens": {
                            "id_token": id_token,
                            "access_token": "access",
                            "refresh_token": "refresh",
                        },
                        "last_refresh": "2026-05-12T00:00:00Z",
                    }
                ),
                encoding="utf-8",
            )
            injector = AuthInjector(codex_config(str(path)))

            injector._reload_from_file()

            self.assertEqual(injector._cached_access, "access")
            self.assertEqual(injector._cached_refresh, "refresh")
            self.assertEqual(injector._cached_account_id, "acct_123")
            self.assertTrue(injector._cached_fedramp)

    def test_reload_does_not_clobber_fresher_memory_with_stale_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "auth.json"
            path.write_text(
                json.dumps(
                    {
                        "tokens": {
                            "access_token": "old-access",
                            "refresh_token": "old-refresh",
                        },
                        "last_refresh": "2026-05-12T00:00:00Z",
                    }
                ),
                encoding="utf-8",
            )
            injector = AuthInjector(codex_config(str(path)))
            injector._cached_blob = {
                "tokens": {
                    "access_token": "new-access",
                    "refresh_token": "new-refresh",
                },
                "last_refresh": "2026-05-13T00:00:00Z",
            }
            injector._cached_access = "new-access"
            injector._cached_refresh = "new-refresh"

            injector._reload_from_file()

            self.assertEqual(injector._cached_access, "new-access")
            self.assertEqual(injector._cached_refresh, "new-refresh")


class HealthSnapshotTests(unittest.TestCase):
    """Pins the provider-health snapshot shape exposed by the proxy."""

    def _fresh_injector(self) -> AuthInjector:
        with tempfile.TemporaryDirectory() as tmp:
            return AuthInjector(codex_config(str(Path(tmp) / "auth.json")))

    def test_snapshot_default_state_is_unknown(self) -> None:
        # Before any refresh attempt, the snapshot's result is "unknown"
        # — the orchestrator treats this as "no data yet, do not flip
        # Layer 1." The proxy intentionally does not infer healthy from
        # the bare absence of a failure; the cached blob may still be
        # serving a long-lived access token whose refresh has never been
        # exercised.
        injector = self._fresh_injector()
        snapshot = injector.health_snapshot()
        self.assertEqual(snapshot["provider"], "codex")
        self.assertEqual(snapshot["result"], "unknown")
        self.assertEqual(snapshot["reason"], "")
        self.assertEqual(snapshot["text"], "")
        self.assertIsNone(snapshot["last_attempted_at"])
        self.assertIsNone(snapshot["last_succeeded_at"])
        self.assertEqual(snapshot["attempt_id"], 0)

    def test_snapshot_after_success_records_success_state(self) -> None:
        injector = self._fresh_injector()
        injector._record_health_result("success", "", "")
        injector._health_attempt_id = 1
        injector._health_last_succeeded_at = 1700000000.0
        injector._health_last_attempted_at = 1700000000.0
        snapshot = injector.health_snapshot()
        self.assertEqual(snapshot["result"], "success")
        self.assertEqual(snapshot["last_succeeded_at"], 1700000000.0)

    def test_snapshot_after_failure_carries_reason_and_text(self) -> None:
        injector = self._fresh_injector()
        injector._record_health_result(
            "http_error",
            "refresh_token_reused",
            "Sign-in expired. The refresh token has already been used; re-authenticate to restore service.",
        )
        injector._health_attempt_id = 2
        injector._health_last_attempted_at = 1700000100.0
        snapshot = injector.health_snapshot()
        self.assertEqual(snapshot["result"], "http_error")
        self.assertEqual(snapshot["reason"], "refresh_token_reused")
        self.assertIn("re-authenticate", snapshot["text"].lower())
        # last_succeeded_at must remain None — a later failure does not
        # invalidate a never-observed success.
        self.assertIsNone(snapshot["last_succeeded_at"])


class ClassifyRefreshFailureTests(unittest.TestCase):
    """Pins how upstream OAuth /token error bodies become (reason, text).

    The refresh_token_reused incident that motivated the transcript
    banner: upstream returns {"error":{"code":"refresh_token_reused",
    "message":"Your refresh token has already been used..."}} and the
    SPA sees a banner explaining what to do. If this classifier drifts,
    the banner becomes content-free again.
    """

    class _StubResponse:
        def __init__(self, status_code: int, body: object) -> None:
            self.status_code = status_code
            self._body = body
            self.text = json.dumps(body) if isinstance(body, dict) else str(body)

        def json(self) -> object:
            if isinstance(self._body, dict):
                return self._body
            raise ValueError("non-json body")

    def test_classify_refresh_token_reused_returns_canonical_text(self) -> None:
        resp = self._StubResponse(
            401,
            {
                "error": {
                    "code": "refresh_token_reused",
                    "message": "Your refresh token has already been used to generate a new access token. Please try signing in again.",
                }
            },
        )
        reason, text = _classify_refresh_failure(resp)  # type: ignore[arg-type]
        self.assertEqual(reason, "refresh_token_reused")
        # Canonical copy preferred over the upstream message — the
        # upstream "Please try signing in again." reads awkwardly in
        # the SPA banner.
        self.assertIn("re-authenticate", text.lower())

    def test_classify_unknown_code_falls_back_to_status(self) -> None:
        resp = self._StubResponse(401, {"error": "bad_things"})
        reason, _ = _classify_refresh_failure(resp)  # type: ignore[arg-type]
        self.assertEqual(reason, "bad_things")

    def test_classify_non_json_body_uses_http_status(self) -> None:
        resp = self._StubResponse(500, "Internal Server Error")
        reason, text = _classify_refresh_failure(resp)  # type: ignore[arg-type]
        self.assertEqual(reason, "http_500")
        self.assertTrue(text)


if __name__ == "__main__":
    unittest.main()
