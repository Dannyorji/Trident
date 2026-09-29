"""trident-indexer — Python client SDK for the Trident Soroban event indexer."""

from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _version

from ._config import TridentConfigError
from .client import TridentClient
from .async_client import AsyncTridentClient
from .errors import TridentApiError
from .retry import DEFAULT_RETRY_CONFIG, RetryConfig
from .types import SorobanEvent, PaginatedEvents, Network
from .openapi_models_gen import OpenAPIModels, SorobanEvent as OpenAPISorobanEvent, EventListResponse, LivenessResponse, ReadyResponse, ReadyChecks, IndexerStatsResponse, ContractStats, ContractStatsResponse, ErrorResponse
from .webhook import (
    DEFAULT_TOLERANCE_SECONDS,
    WebhookVerificationError,
    compute_signature,
    verify_signature,
)

try:
    __version__ = _version("trident-indexer")
except PackageNotFoundError:
    # Package metadata is unavailable when running from a source checkout
    # that was never installed (e.g. `python -c "import trident_indexer"`
    # from the repo root without `pip install -e .`).
    __version__ = "0.0.0+unknown"

__all__ = [
    "TridentClient",
    "AsyncTridentClient",
    "TridentApiError",
    "TridentConfigError",
    "SorobanEvent",
    "PaginatedEvents",
    "Network",
    "OpenAPIModels",
    "OpenAPISorobanEvent",
    "EventListResponse",
    "LivenessResponse",
    "ReadyResponse",
    "ReadyChecks",
    "IndexerStatsResponse",
    "ContractStats",
    "ContractStatsResponse",
    "ErrorResponse",
    "RetryConfig",
    "DEFAULT_RETRY_CONFIG",
    "__version__",
]
