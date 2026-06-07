import sys
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from glimmung_api_proxy.github_policy import (
    PolicyError,
    enforce_receive_pack_policy,
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


if __name__ == "__main__":
    unittest.main()
