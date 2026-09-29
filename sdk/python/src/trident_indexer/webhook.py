"""Webhook signature verification helpers for Trident (issue #452).

Receivers call :func:`verify_signature` on every incoming webhook request to
confirm the delivery is authentic and within the replay-protection window.

Signing scheme (for offline validation)::

    import hmac, hashlib
    message  = f"{timestamp}.{body}".encode()
    mac      = hmac.new(secret.encode(), message, hashlib.sha256)
    expected = "sha256=" + mac.hexdigest()

The ``X-Trident-Signature`` header may contain two space-separated signatures
during a secret rotation overlap window.  Pass your current active secret —
the function checks all tokens and returns ``True`` if any one matches.
"""

from __future__ import annotations

import hashlib
import hmac
import time
from typing import Union

__all__ = [
    "DEFAULT_TOLERANCE_SECONDS",
    "WebhookVerificationError",
    "verify_signature",
    "compute_signature",
]

#: Recommended replay-protection window in seconds (5 minutes).
DEFAULT_TOLERANCE_SECONDS: int = 300


class WebhookVerificationError(Exception):
    """Raised by :func:`verify_signature` when verification fails.

    The ``reason`` attribute carries a human-readable explanation for logging.
    Do **not** surface it in HTTP responses to callers.
    """

    def __init__(self, reason: str) -> None:
        super().__init__(f"webhook signature verification failed: {reason}")
        self.reason = reason


def compute_signature(timestamp: int, body: Union[str, bytes], secret: str) -> str:
    """Return the ``sha256=<hex>`` signature for *body* at *timestamp*.

    This is the reference implementation — use it to produce test vectors or
    to sign outgoing requests in your own test server.
    """
    if isinstance(body, str):
        body = body.encode()
    message = f"{timestamp}.".encode() + body
    mac = hmac.new(secret.encode(), message, hashlib.sha256)
    return "sha256=" + mac.hexdigest()


def verify_signature(
    body: Union[str, bytes],
    signature: str,
    timestamp: str,
    secret: str,
    tolerance_seconds: int = DEFAULT_TOLERANCE_SECONDS,
) -> None:
    """Verify an incoming Trident webhook delivery.

    :param body: Raw request body — read it **before** parsing JSON.
    :param signature: Value of the ``X-Trident-Signature`` header.
    :param timestamp: Value of the ``X-Trident-Timestamp`` header (Unix seconds).
    :param secret: Your webhook subscription secret (starts with ``whsec_``).
    :param tolerance_seconds: Maximum delivery age in seconds.  Pass ``0`` to
        disable the timestamp check (not recommended in production).
        Defaults to :data:`DEFAULT_TOLERANCE_SECONDS` (300 s).

    :raises WebhookVerificationError: on any verification failure.
    :raises WebhookVerificationError: if the timestamp is outside the tolerance window.
    """
    # 1. Parse and range-check the timestamp.
    try:
        ts = int(timestamp)
    except (ValueError, TypeError):
        raise WebhookVerificationError(
            "X-Trident-Timestamp is missing or not a valid Unix second"
        )
    if ts <= 0:
        raise WebhookVerificationError(
            "X-Trident-Timestamp must be a positive Unix second"
        )
    if tolerance_seconds > 0:
        age = abs(int(time.time()) - ts)
        if age > tolerance_seconds:
            raise WebhookVerificationError(
                f"timestamp is {age}s old (tolerance {tolerance_seconds}s) — possible replay"
            )

    # 2. Compute the expected signature.
    expected = compute_signature(ts, body, secret)

    # 3. Accept any token in a space-separated signature header.
    for token in signature.split():
        if hmac.compare_digest(token, expected):
            return

    raise WebhookVerificationError("signature does not match")
