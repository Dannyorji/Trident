"""Tests for trident_indexer.webhook (issue #452)."""

import time

import pytest

from trident_indexer.webhook import (
    DEFAULT_TOLERANCE_SECONDS,
    WebhookVerificationError,
    compute_signature,
    verify_signature,
)


BODY = b'{"id":"evt1","event":{"contractId":"CABC"}}'
SECRET = "whsec_testsecret"


def fresh_sig(body: bytes = BODY, secret: str = SECRET) -> tuple[bytes, str, str]:
    """Return (body, signature, timestamp) for a just-issued delivery."""
    ts = int(time.time())
    sig = compute_signature(ts, body, secret)
    return body, sig, str(ts)


class TestComputeSignature:
    def test_deterministic(self) -> None:
        s1 = compute_signature(1_700_000_000, b"body", "secret")
        s2 = compute_signature(1_700_000_000, b"body", "secret")
        assert s1 == s2

    def test_starts_with_prefix(self) -> None:
        sig = compute_signature(1_700_000_000, b"body", "secret")
        assert sig.startswith("sha256=")

    def test_differs_on_body(self) -> None:
        a = compute_signature(1_700_000_000, b"aaa", "secret")
        b = compute_signature(1_700_000_000, b"bbb", "secret")
        assert a != b

    def test_differs_on_secret(self) -> None:
        a = compute_signature(1_700_000_000, b"body", "secret-a")
        b = compute_signature(1_700_000_000, b"body", "secret-b")
        assert a != b

    def test_differs_on_timestamp(self) -> None:
        a = compute_signature(1_700_000_000, b"body", "secret")
        b = compute_signature(1_700_000_001, b"body", "secret")
        assert a != b

    def test_accepts_str_body(self) -> None:
        a = compute_signature(1_700_000_000, b"body", "secret")
        b = compute_signature(1_700_000_000, "body", "secret")
        assert a == b


class TestVerifySignature:
    def test_valid(self) -> None:
        body, sig, ts = fresh_sig()
        verify_signature(body, sig, ts, SECRET)  # must not raise

    def test_wrong_secret(self) -> None:
        body, sig, ts = fresh_sig()
        with pytest.raises(WebhookVerificationError, match="does not match"):
            verify_signature(body, sig, ts, "whsec_wrong")

    def test_altered_body(self) -> None:
        _, sig, ts = fresh_sig(b"original")
        with pytest.raises(WebhookVerificationError):
            verify_signature(b"altered", sig, ts, SECRET)

    def test_replay_rejected(self) -> None:
        stale_ts = int(time.time()) - 600  # 10 minutes ago
        sig = compute_signature(stale_ts, BODY, SECRET)
        with pytest.raises(WebhookVerificationError, match="replay"):
            verify_signature(BODY, sig, str(stale_ts), SECRET, DEFAULT_TOLERANCE_SECONDS)

    def test_tolerance_disabled(self) -> None:
        stale_ts = int(time.time()) - 9999
        sig = compute_signature(stale_ts, BODY, SECRET)
        verify_signature(BODY, sig, str(stale_ts), SECRET, tolerance_seconds=0)

    def test_invalid_timestamp(self) -> None:
        with pytest.raises(WebhookVerificationError, match="not a valid"):
            verify_signature(BODY, "sha256=abc", "not-a-number", SECRET)

    def test_rotation_overlap_old_secret(self) -> None:
        old, new = "whsec_old", "whsec_new"
        ts = int(time.time())
        body = b'{"id":"evt1"}'
        sig_new = compute_signature(ts, body, new)
        sig_old = compute_signature(ts, body, old)
        combined = sig_new + " " + sig_old
        # Receiver still on old secret must pass.
        verify_signature(body, combined, str(ts), old)

    def test_rotation_overlap_new_secret(self) -> None:
        old, new = "whsec_old", "whsec_new"
        ts = int(time.time())
        body = b'{"id":"evt1"}'
        sig_new = compute_signature(ts, body, new)
        sig_old = compute_signature(ts, body, old)
        combined = sig_new + " " + sig_old
        # Receiver already on new secret must also pass.
        verify_signature(body, combined, str(ts), new)


class TestVectors:
    """Published test vectors — docs/webhook-signature-test-vectors.md."""

    VECTORS = [
        (
            "vector-1-minimal",
            "whsec_0000000000000000000000000000000000000000000000000000000000000001",
            1_700_000_000,
            b'{"id":"evt_test_001"}',
            "sha256=0c5e53bcf4dd4338bc9b91cba384aed76c88fcb31a21ca28cf68ef19bef8797a",
        ),
        (
            "vector-2-empty-body",
            "whsec_0000000000000000000000000000000000000000000000000000000000000001",
            1_700_000_000,
            b"",
            "sha256=e7f81d9f74fad194220a4403cd3d75a2ee450ee8422648b4a0230a4ce77c2f5d",
        ),
    ]

    @pytest.mark.parametrize("name,secret,ts,body,want", VECTORS)
    def test_vector(self, name: str, secret: str, ts: int, body: bytes, want: str) -> None:
        got = compute_signature(ts, body, secret)
        assert got == want, f"{name}: got {got}, want {want}"
