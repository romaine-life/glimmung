import sys
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from glimmung_api_proxy.github_policy import (
    PolicyError,
    enforce_receive_pack_policy,
    forward_request_headers,
    forward_response_headers,
    parse_basic_policy_token,
    repository_from_git_path,
    sign_policy_token,
    validate_policy_token,
)


def pkt(payload: bytes) -> bytes:
    return f"{len(payload) + 4:04x}".encode() + payload


class GitHubPolicyTests(unittest.TestCase):
    def test_policy_token_round_trip(self) -> None:
        payload = {
            "version": 1,
            "repo": "romaine-life/ambience",
            "branch": "glimmung/issue-168/run-1",
            "ref": "refs/heads/glimmung/issue-168/run-1",
            "expires_at": int(time.time()) + 60,
        }
        token = sign_policy_token("secret", payload)

        got = validate_policy_token(token, "secret")

        self.assertEqual(got["repo"], payload["repo"])
        self.assertEqual(got["ref"], payload["ref"])

    def test_policy_token_rejects_wrong_signature(self) -> None:
        token = sign_policy_token("secret", {
            "repo": "owner/repo",
            "branch": "branch",
            "ref": "refs/heads/branch",
            "expires_at": int(time.time()) + 60,
        })

        with self.assertRaises(PolicyError) as cm:
            validate_policy_token(token, "other-secret")

        self.assertEqual(cm.exception.status, 403)

    def test_repository_from_git_path(self) -> None:
        self.assertEqual(repository_from_git_path("/romaine-life/ambience.git/git-receive-pack"), "romaine-life/ambience")

    def test_parse_basic_policy_token(self) -> None:
        import base64

        header = "Basic " + base64.b64encode(b"glimmung-policy:token").decode()
        self.assertEqual(parse_basic_policy_token(header), "token")

    def test_enforce_receive_pack_accepts_allowed_ref(self) -> None:
        body = (
            pkt(
                b"0000000000000000000000000000000000000000 "
                b"1111111111111111111111111111111111111111 "
                b"refs/heads/glimmung/issue-168/run-1\x00 report-status\n"
            )
            + b"0000"
            + b"PACK..."
        )

        commands = enforce_receive_pack_policy(body, "refs/heads/glimmung/issue-168/run-1")

        self.assertEqual(len(commands), 1)

    def test_enforce_receive_pack_rejects_other_ref(self) -> None:
        body = (
            pkt(
                b"0000000000000000000000000000000000000000 "
                b"1111111111111111111111111111111111111111 "
                b"refs/heads/main\x00 report-status\n"
            )
            + b"0000"
        )

        with self.assertRaises(PolicyError) as cm:
            enforce_receive_pack_policy(body, "refs/heads/glimmung/issue-168/run-1")

        self.assertEqual(cm.exception.status, 403)

    def test_enforce_receive_pack_rejects_delete(self) -> None:
        body = (
            pkt(
                b"1111111111111111111111111111111111111111 "
                b"0000000000000000000000000000000000000000 "
                b"refs/heads/glimmung/issue-168/run-1\x00 report-status\n"
            )
            + b"0000"
        )

        with self.assertRaises(PolicyError) as cm:
            enforce_receive_pack_policy(body, "refs/heads/glimmung/issue-168/run-1")

        self.assertEqual(cm.exception.status, 403)


class ForwardHeaderTests(unittest.TestCase):
    def test_request_headers_strip_content_encoding_and_force_host(self) -> None:
        # The protocol-v2 fetch POST that broke clones: git sends a gzip body
        # but aiohttp decompresses it before we forward, so the stale
        # Content-Encoding must not survive (else github 400s the mismatch).
        out = forward_request_headers([
            ("Host", "github.com"),
            ("Content-Length", "1569"),
            ("Content-Encoding", "gzip"),
            ("Content-Type", "application/x-git-upload-pack-request"),
            ("Git-Protocol", "version=2"),
            ("Authorization", "Basic eA=="),
            ("Connection", "keep-alive"),
        ])
        lower = {k.lower() for k in out}
        self.assertNotIn("content-encoding", lower)
        self.assertNotIn("content-length", lower)
        self.assertNotIn("connection", lower)  # hop-by-hop dropped
        self.assertEqual(out["Host"], "github.com")
        self.assertEqual(out["Content-Type"], "application/x-git-upload-pack-request")
        self.assertEqual(out["Git-Protocol"], "version=2")
        self.assertEqual(out["Authorization"], "Basic eA==")

    def test_request_headers_force_host_even_when_absent(self) -> None:
        out = forward_request_headers([("Accept", "*/*")])
        self.assertEqual(out["Host"], "github.com")

    def test_response_headers_strip_content_encoding_and_length(self) -> None:
        # httpx already decompressed resp.content, so a forwarded
        # Content-Encoding would mislabel a plaintext body to the git client.
        out = forward_response_headers([
            ("Content-Encoding", "gzip"),
            ("Content-Length", "5095"),
            ("Content-Type", "application/x-git-upload-pack-result"),
            ("Transfer-Encoding", "chunked"),
        ])
        lower = {k.lower() for k in out}
        self.assertNotIn("content-encoding", lower)
        self.assertNotIn("content-length", lower)
        self.assertNotIn("transfer-encoding", lower)  # hop-by-hop dropped
        self.assertEqual(out["Content-Type"], "application/x-git-upload-pack-result")


if __name__ == "__main__":
    unittest.main()
