from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time
from dataclasses import dataclass
from typing import Any, Iterable


class PolicyError(Exception):
    def __init__(self, status: int, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.message = message


# Headers stripped when relaying between the git client and github: hop-by-hop
# per RFC 7230, plus content-length (the forwarder re-derives it from the
# buffered body) and content-encoding.
#
# content-encoding is the load-bearing one. The proxy's HTTP stacks
# auto-decompress bodies — aiohttp on the inbound request (request.read() also
# feeds the receive-pack policy parser, which needs plaintext) and httpx on the
# upstream response — so the body that is re-sent is always already plaintext.
# Forwarding the original `Content-Encoding: gzip` would label that plaintext as
# gzip; github answers the mismatch with an empty-bodied 400. That is exactly
# what broke git protocol-v2 clones: the fetch POST gzips its request body while
# the ls-refs POST does not, so only the fetch failed.
HOP_BY_HOP_HEADERS = frozenset(
    {
        "connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "te",
        "trailer",
        "transfer-encoding",
        "upgrade",
    }
)
_STRIP_REQUEST_HEADERS = HOP_BY_HOP_HEADERS | {"host", "content-length", "content-encoding"}
_STRIP_RESPONSE_HEADERS = HOP_BY_HOP_HEADERS | {"content-length", "content-encoding"}


def forward_request_headers(items: Iterable[tuple[str, str]]) -> dict[str, str]:
    """Header set to send upstream to github, with Host forced to github.com."""
    out = {key: value for key, value in items if key.lower() not in _STRIP_REQUEST_HEADERS}
    out["Host"] = "github.com"
    return out


def forward_response_headers(items: Iterable[tuple[str, str]]) -> dict[str, str]:
    """Header set to relay from github back to the git client."""
    return {key: value for key, value in items if key.lower() not in _STRIP_RESPONSE_HEADERS}


@dataclass(frozen=True)
class ReceivePackCommand:
    old: str
    new: str
    ref: str


def sign_policy_token(signing_key: str, payload: dict[str, Any]) -> str:
    raw = json.dumps(payload, separators=(",", ":"), sort_keys=True).encode()
    body = base64.urlsafe_b64encode(raw).decode().rstrip("=")
    sig = hmac.new(signing_key.encode(), body.encode(), hashlib.sha256).digest()
    encoded_sig = base64.urlsafe_b64encode(sig).decode().rstrip("=")
    return f"{body}.{encoded_sig}"


def validate_policy_token(token: str, signing_key: str, *, now: int | None = None) -> dict[str, Any]:
    if not token or "." not in token:
        raise PolicyError(401, "missing or malformed Glimmung GitHub policy token")
    body, encoded_sig = token.rsplit(".", 1)
    expected = hmac.new(signing_key.encode(), body.encode(), hashlib.sha256).digest()
    try:
        actual = _b64decode(encoded_sig)
    except ValueError as exc:
        raise PolicyError(401, "malformed Glimmung GitHub policy signature") from exc
    if not hmac.compare_digest(expected, actual):
        raise PolicyError(403, "Glimmung GitHub policy signature did not match")
    try:
        payload = json.loads(_b64decode(body))
    except Exception as exc:
        raise PolicyError(401, "malformed Glimmung GitHub policy payload") from exc
    if not isinstance(payload, dict):
        raise PolicyError(401, "Glimmung GitHub policy payload must be an object")
    expires_at = payload.get("expires_at")
    if not isinstance(expires_at, int):
        raise PolicyError(401, "Glimmung GitHub policy token has no expiry")
    if expires_at < (int(time.time()) if now is None else now):
        raise PolicyError(403, "Glimmung GitHub policy token is expired")
    for key in ("repo", "branch", "ref"):
        if not isinstance(payload.get(key), str) or not payload[key].strip():
            raise PolicyError(401, f"Glimmung GitHub policy token missing {key}")
    return payload


def repository_from_git_path(path: str) -> str:
    parts = path.split("/")
    if len(parts) < 3 or parts[0] != "" or not parts[2].endswith(".git"):
        raise PolicyError(404, "unsupported GitHub git path")
    owner = parts[1]
    repo = parts[2][:-4]
    if not owner or not repo:
        raise PolicyError(404, "unsupported GitHub git path")
    return f"{owner}/{repo}"


def parse_basic_policy_token(header: str | None) -> str:
    if not header or not header.lower().startswith("basic "):
        raise PolicyError(401, "GitHub policy proxy requires Basic auth")
    raw = header.split(" ", 1)[1].strip()
    try:
        decoded = base64.b64decode(raw).decode()
    except Exception as exc:
        raise PolicyError(401, "malformed Basic auth") from exc
    username, sep, password = decoded.partition(":")
    if sep != ":" or username != "glimmung-policy" or not password:
        raise PolicyError(401, "GitHub policy proxy requires glimmung-policy credentials")
    return password


def parse_receive_pack_commands(body: bytes, *, max_prelude_bytes: int = 1_048_576) -> list[ReceivePackCommand]:
    commands: list[ReceivePackCommand] = []
    offset = 0
    while True:
        if offset > max_prelude_bytes:
            raise PolicyError(413, "git receive-pack command prelude is too large")
        if offset + 4 > len(body):
            raise PolicyError(400, "git receive-pack command prelude is truncated")
        raw_len = body[offset : offset + 4]
        offset += 4
        try:
            pkt_len = int(raw_len.decode("ascii"), 16)
        except ValueError as exc:
            raise PolicyError(400, "git receive-pack command prelude has invalid pkt-line length") from exc
        if pkt_len == 0:
            if not commands:
                raise PolicyError(400, "git receive-pack command prelude had no ref updates")
            return commands
        if pkt_len < 4:
            raise PolicyError(400, "git receive-pack command prelude has invalid pkt-line length")
        payload_len = pkt_len - 4
        if offset + payload_len > len(body):
            raise PolicyError(400, "git receive-pack command prelude is truncated")
        line = body[offset : offset + payload_len]
        offset += payload_len
        commands.append(_parse_command_line(line, first=len(commands) == 0))


def enforce_receive_pack_policy(body: bytes, allowed_ref: str) -> list[ReceivePackCommand]:
    commands = parse_receive_pack_commands(body)
    for command in commands:
        if command.ref != allowed_ref:
            raise PolicyError(403, f"push to {command.ref} rejected; allowed ref is {allowed_ref}")
        if command.new == "0" * 40:
            raise PolicyError(403, f"delete of {allowed_ref} rejected")
    return commands


def _parse_command_line(line: bytes, *, first: bool) -> ReceivePackCommand:
    if first:
        line = line.split(b"\x00", 1)[0]
    line = line.rstrip(b"\n")
    try:
        text = line.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise PolicyError(400, "git receive-pack command line is not utf-8") from exc
    fields = text.split(" ")
    if len(fields) < 3:
        raise PolicyError(400, "git receive-pack command line is malformed")
    old, new, ref = fields[0], fields[1], fields[2]
    if len(old) != 40 or len(new) != 40 or not ref.startswith("refs/"):
        raise PolicyError(400, "git receive-pack command line has malformed refs")
    return ReceivePackCommand(old=old, new=new, ref=ref)


def _b64decode(value: str) -> bytes:
    return base64.urlsafe_b64decode(value + ("=" * (-len(value) % 4)))
