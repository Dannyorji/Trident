from dataclasses import dataclass
from typing import Any, TypeVar, Callable, Type, cast
from uuid import UUID
from enum import Enum


T = TypeVar("T")
EnumT = TypeVar("EnumT", bound=Enum)


def from_float(x: Any) -> float:
    assert isinstance(x, (float, int)) and not isinstance(x, bool)
    return float(x)


def from_str(x: Any) -> str:
    assert isinstance(x, str)
    return x


def from_int(x: Any) -> int:
    assert isinstance(x, int) and not isinstance(x, bool)
    return x


def to_float(x: Any) -> float:
    assert isinstance(x, (int, float))
    return x


def from_list(f: Callable[[Any], T], x: Any) -> list[T]:
    assert isinstance(x, list)
    return [f(y) for y in x]


def to_class(c: Type[T], x: Any) -> dict:
    assert isinstance(x, c)
    return cast(Any, x).to_dict()


def from_none(x: Any) -> Any:
    assert x is None
    return x


def from_union(fs, x):
    for f in fs:
        try:
            return f(x)
        except:
            pass
    assert False


def to_enum(c: Type[EnumT], x: Any) -> EnumT:
    assert isinstance(x, c)
    return x.value


def from_bool(x: Any) -> bool:
    assert isinstance(x, bool)
    return x


@dataclass
class EndpointUsage:
    avg_duration_ms: float
    endpoint: str
    requests: int

    @staticmethod
    def from_dict(obj: Any) -> 'EndpointUsage':
        assert isinstance(obj, dict)
        avg_duration_ms = from_float(obj.get("avg_duration_ms"))
        endpoint = from_str(obj.get("endpoint"))
        requests = from_int(obj.get("requests"))
        return EndpointUsage(avg_duration_ms, endpoint, requests)

    def to_dict(self) -> dict:
        result: dict = {}
        result["avg_duration_ms"] = to_float(self.avg_duration_ms)
        result["endpoint"] = from_str(self.endpoint)
        result["requests"] = from_int(self.requests)
        return result


@dataclass
class AdminKeyUsageResponse:
    api_key_id: UUID
    by_endpoint: list[EndpointUsage]
    """Per-endpoint breakdown; empty when the window has no requests"""

    admin_key_usage_response_from: str
    successful_requests: int
    """Requests with status code < 400"""

    to: str
    total_requests: int

    @staticmethod
    def from_dict(obj: Any) -> 'AdminKeyUsageResponse':
        assert isinstance(obj, dict)
        api_key_id = UUID(obj.get("api_key_id"))
        by_endpoint = from_list(EndpointUsage.from_dict, obj.get("by_endpoint"))
        admin_key_usage_response_from = from_str(obj.get("from"))
        successful_requests = from_int(obj.get("successful_requests"))
        to = from_str(obj.get("to"))
        total_requests = from_int(obj.get("total_requests"))
        return AdminKeyUsageResponse(api_key_id, by_endpoint, admin_key_usage_response_from, successful_requests, to, total_requests)

    def to_dict(self) -> dict:
        result: dict = {}
        result["api_key_id"] = str(self.api_key_id)
        result["by_endpoint"] = from_list(lambda x: to_class(EndpointUsage, x), self.by_endpoint)
        result["from"] = from_str(self.admin_key_usage_response_from)
        result["successful_requests"] = from_int(self.successful_requests)
        result["to"] = from_str(self.to)
        result["total_requests"] = from_int(self.total_requests)
        return result


class Network(Enum):
    """Network queried"""

    MAINNET = "mainnet"
    TESTNET = "testnet"


@dataclass
class APIKeyResponse:
    created_at: str
    id: UUID
    key_prefix: str
    label: str
    last_used_at: str
    network: Network
    rate_limit_tier: str
    request_count: int
    created_by: str | None = None
    key: str | None = None
    """Raw key, returned only at creation time."""

    revoked_at: str | None = None

    @staticmethod
    def from_dict(obj: Any) -> 'APIKeyResponse':
        assert isinstance(obj, dict)
        created_at = from_str(obj.get("created_at"))
        id = UUID(obj.get("id"))
        key_prefix = from_str(obj.get("key_prefix"))
        label = from_str(obj.get("label"))
        last_used_at = from_str(obj.get("last_used_at"))
        network = Network(obj.get("network"))
        rate_limit_tier = from_str(obj.get("rate_limit_tier"))
        request_count = from_int(obj.get("request_count"))
        created_by = from_union([from_str, from_none], obj.get("created_by"))
        key = from_union([from_str, from_none], obj.get("key"))
        revoked_at = from_union([from_str, from_none], obj.get("revoked_at"))
        return APIKeyResponse(created_at, id, key_prefix, label, last_used_at, network, rate_limit_tier, request_count, created_by, key, revoked_at)

    def to_dict(self) -> dict:
        result: dict = {}
        result["created_at"] = from_str(self.created_at)
        result["id"] = str(self.id)
        result["key_prefix"] = from_str(self.key_prefix)
        result["label"] = from_str(self.label)
        result["last_used_at"] = from_str(self.last_used_at)
        result["network"] = to_enum(Network, self.network)
        result["rate_limit_tier"] = from_str(self.rate_limit_tier)
        result["request_count"] = from_int(self.request_count)
        if self.created_by is not None:
            result["created_by"] = from_union([from_str, from_none], self.created_by)
        if self.key is not None:
            result["key"] = from_union([from_str, from_none], self.key)
        if self.revoked_at is not None:
            result["revoked_at"] = from_union([from_str, from_none], self.revoked_at)
        return result


@dataclass
class ContractCallRequest:
    function: str
    """Contract function name to invoke"""

    args: list[str] | None = None
    """Base64-encoded XDR ScVal arguments, in order"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractCallRequest':
        assert isinstance(obj, dict)
        function = from_str(obj.get("function"))
        args = from_union([lambda x: from_list(from_str, x), from_none], obj.get("args"))
        return ContractCallRequest(function, args)

    def to_dict(self) -> dict:
        result: dict = {}
        result["function"] = from_str(self.function)
        if self.args is not None:
            result["args"] = from_union([lambda x: from_list(from_str, x), from_none], self.args)
        return result


@dataclass
class ContractCallResponse:
    success: bool
    """False when the simulation itself reported a failure (still HTTP 200)"""

    error: str | None = None
    """Simulation error message; present only when success=false"""

    raw_xdr: str | None = None
    """Raw base64 XDR of the return value; omitted on failure"""

    result: Any = None
    """Decoded return value; omitted when undecodable or failed"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractCallResponse':
        assert isinstance(obj, dict)
        success = from_bool(obj.get("success"))
        error = from_union([from_str, from_none], obj.get("error"))
        raw_xdr = from_union([from_str, from_none], obj.get("raw_xdr"))
        result = obj.get("result")
        return ContractCallResponse(success, error, raw_xdr, result)

    def to_dict(self) -> dict:
        result: dict = {}
        result["success"] = from_bool(self.success)
        if self.error is not None:
            result["error"] = from_union([from_str, from_none], self.error)
        if self.raw_xdr is not None:
            result["raw_xdr"] = from_union([from_str, from_none], self.raw_xdr)
        if self.result is not None:
            result["result"] = self.result
        return result


@dataclass
class ContractEventFieldSchema:
    name: str
    """Stable field name for this event payload position or property"""

    type: str
    """Field type inferred from the contract interface or observed payloads"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractEventFieldSchema':
        assert isinstance(obj, dict)
        name = from_str(obj.get("name"))
        type = from_str(obj.get("type"))
        return ContractEventFieldSchema(name, type)

    def to_dict(self) -> dict:
        result: dict = {}
        result["name"] = from_str(self.name)
        result["type"] = from_str(self.type)
        return result


@dataclass
class ContractEventSchema:
    event_name: str
    """Contract event name (topic_0)"""

    fields: list[ContractEventFieldSchema]
    """Named fields for this event payload"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractEventSchema':
        assert isinstance(obj, dict)
        event_name = from_str(obj.get("event_name"))
        fields = from_list(ContractEventFieldSchema.from_dict, obj.get("fields"))
        return ContractEventSchema(event_name, fields)

    def to_dict(self) -> dict:
        result: dict = {}
        result["event_name"] = from_str(self.event_name)
        result["fields"] = from_list(lambda x: to_class(ContractEventFieldSchema, x), self.fields)
        return result


@dataclass
class ContractEventSchemaResponse:
    code_hash: str
    """Contract code hash for this schema version"""

    contract_id: str
    """Soroban contract address"""

    events: list[ContractEventSchema]
    """Observed event names and their typed field schemas"""

    network: Network
    """Network queried"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractEventSchemaResponse':
        assert isinstance(obj, dict)
        code_hash = from_str(obj.get("code_hash"))
        contract_id = from_str(obj.get("contract_id"))
        events = from_list(ContractEventSchema.from_dict, obj.get("events"))
        network = Network(obj.get("network"))
        return ContractEventSchemaResponse(code_hash, contract_id, events, network)

    def to_dict(self) -> dict:
        result: dict = {}
        result["code_hash"] = from_str(self.code_hash)
        result["contract_id"] = from_str(self.contract_id)
        result["events"] = from_list(lambda x: to_class(ContractEventSchema, x), self.events)
        result["network"] = to_enum(Network, self.network)
        return result


@dataclass
class ContractRegistrationRequest:
    contract_id: str
    """Contract address (C... strkey, 56 characters)"""

    index_from: int | None = None
    """Ledger sequence to start indexing from"""

    label: str | None = None
    """Human-readable label"""

    network: str | None = None
    """Network scope; omitted or empty means all networks"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractRegistrationRequest':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contract_id"))
        index_from = from_union([from_int, from_none], obj.get("index_from"))
        label = from_union([from_str, from_none], obj.get("label"))
        network = from_union([from_str, from_none], obj.get("network"))
        return ContractRegistrationRequest(contract_id, index_from, label, network)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contract_id"] = from_str(self.contract_id)
        if self.index_from is not None:
            result["index_from"] = from_union([from_int, from_none], self.index_from)
        if self.label is not None:
            result["label"] = from_union([from_str, from_none], self.label)
        if self.network is not None:
            result["network"] = from_union([from_str, from_none], self.network)
        return result


@dataclass
class ContractResponse:
    contract_id: str
    """Stellar contract id (C... strkey)."""

    created_at: str
    id: UUID
    index_from: int
    """Ledger sequence indexing began from."""

    label: str | None = None
    network: str | None = None

    @staticmethod
    def from_dict(obj: Any) -> 'ContractResponse':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contract_id"))
        created_at = from_str(obj.get("created_at"))
        id = UUID(obj.get("id"))
        index_from = from_int(obj.get("index_from"))
        label = from_union([from_str, from_none], obj.get("label"))
        network = from_union([from_str, from_none], obj.get("network"))
        return ContractResponse(contract_id, created_at, id, index_from, label, network)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contract_id"] = from_str(self.contract_id)
        result["created_at"] = from_str(self.created_at)
        result["id"] = str(self.id)
        result["index_from"] = from_int(self.index_from)
        if self.label is not None:
            result["label"] = from_union([from_str, from_none], self.label)
        if self.network is not None:
            result["network"] = from_union([from_str, from_none], self.network)
        return result


@dataclass
class ContractSpecFunction:
    name: str
    """Exported function name"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractSpecFunction':
        assert isinstance(obj, dict)
        name = from_str(obj.get("name"))
        return ContractSpecFunction(name)

    def to_dict(self) -> dict:
        result: dict = {}
        result["name"] = from_str(self.name)
        return result


@dataclass
class ContractSpecResponse:
    code_hash: str
    """Deployed WASM code hash this spec was parsed from"""

    contract_id: str
    """Soroban contract address"""

    contract_type: str
    """Primary classification derived from detected interfaces (e.g. token, nft, custom)"""

    functions: list[ContractSpecFunction]
    """Functions captured from the contract's spec"""

    has_spec: bool
    """Whether an embedded contractspecv0 section was found"""

    interfaces: list[str]
    """Every standard interface detected from the contract's spec functions"""

    network: Network
    """Network queried"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractSpecResponse':
        assert isinstance(obj, dict)
        code_hash = from_str(obj.get("code_hash"))
        contract_id = from_str(obj.get("contract_id"))
        contract_type = from_str(obj.get("contract_type"))
        functions = from_list(ContractSpecFunction.from_dict, obj.get("functions"))
        has_spec = from_bool(obj.get("has_spec"))
        interfaces = from_list(from_str, obj.get("interfaces"))
        network = Network(obj.get("network"))
        return ContractSpecResponse(code_hash, contract_id, contract_type, functions, has_spec, interfaces, network)

    def to_dict(self) -> dict:
        result: dict = {}
        result["code_hash"] = from_str(self.code_hash)
        result["contract_id"] = from_str(self.contract_id)
        result["contract_type"] = from_str(self.contract_type)
        result["functions"] = from_list(lambda x: to_class(ContractSpecFunction, x), self.functions)
        result["has_spec"] = from_bool(self.has_spec)
        result["interfaces"] = from_list(from_str, self.interfaces)
        result["network"] = to_enum(Network, self.network)
        return result


@dataclass
class ContractStats:
    avg_cpu_instructions: float
    avg_fee_charged: float
    avg_read_bytes: float
    avg_write_bytes: float
    contract_id: str
    """Soroban contract address"""

    event_count: int
    """Total events for this contract in range"""

    invocation_count: int
    last_seen_at: str
    """Timestamp of last event for this contract"""

    last_seen_ledger: int
    """Latest ledger sequence for this contract"""

    total_fee_charged: int

    @staticmethod
    def from_dict(obj: Any) -> 'ContractStats':
        assert isinstance(obj, dict)
        avg_cpu_instructions = from_float(obj.get("avg_cpu_instructions"))
        avg_fee_charged = from_float(obj.get("avg_fee_charged"))
        avg_read_bytes = from_float(obj.get("avg_read_bytes"))
        avg_write_bytes = from_float(obj.get("avg_write_bytes"))
        contract_id = from_str(obj.get("contract_id"))
        event_count = from_int(obj.get("event_count"))
        invocation_count = from_int(obj.get("invocation_count"))
        last_seen_at = from_str(obj.get("last_seen_at"))
        last_seen_ledger = from_int(obj.get("last_seen_ledger"))
        total_fee_charged = from_int(obj.get("total_fee_charged"))
        return ContractStats(avg_cpu_instructions, avg_fee_charged, avg_read_bytes, avg_write_bytes, contract_id, event_count, invocation_count, last_seen_at, last_seen_ledger, total_fee_charged)

    def to_dict(self) -> dict:
        result: dict = {}
        result["avg_cpu_instructions"] = to_float(self.avg_cpu_instructions)
        result["avg_fee_charged"] = to_float(self.avg_fee_charged)
        result["avg_read_bytes"] = to_float(self.avg_read_bytes)
        result["avg_write_bytes"] = to_float(self.avg_write_bytes)
        result["contract_id"] = from_str(self.contract_id)
        result["event_count"] = from_int(self.event_count)
        result["invocation_count"] = from_int(self.invocation_count)
        result["last_seen_at"] = from_str(self.last_seen_at)
        result["last_seen_ledger"] = from_int(self.last_seen_ledger)
        result["total_fee_charged"] = from_int(self.total_fee_charged)
        return result


@dataclass
class ContractStatsResponse:
    contracts: list[ContractStats]
    """Contracts sorted by event count (descending)"""

    from_ledger: int
    """Lower bound of queried ledger range"""

    generated_at: str
    """Timestamp when response was generated"""

    has_more: bool
    """Whether more pages are available"""

    network: Network
    """Network queried"""

    to_ledger: int
    """Upper bound of queried ledger range"""

    next_cursor: str | None = None
    """Opaque cursor to pass as the cursor parameter for the next page"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractStatsResponse':
        assert isinstance(obj, dict)
        contracts = from_list(ContractStats.from_dict, obj.get("contracts"))
        from_ledger = from_int(obj.get("from_ledger"))
        generated_at = from_str(obj.get("generated_at"))
        has_more = from_bool(obj.get("has_more"))
        network = Network(obj.get("network"))
        to_ledger = from_int(obj.get("to_ledger"))
        next_cursor = from_union([from_str, from_none], obj.get("next_cursor"))
        return ContractStatsResponse(contracts, from_ledger, generated_at, has_more, network, to_ledger, next_cursor)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contracts"] = from_list(lambda x: to_class(ContractStats, x), self.contracts)
        result["from_ledger"] = from_int(self.from_ledger)
        result["generated_at"] = from_str(self.generated_at)
        result["has_more"] = from_bool(self.has_more)
        result["network"] = to_enum(Network, self.network)
        result["to_ledger"] = from_int(self.to_ledger)
        if self.next_cursor is not None:
            result["next_cursor"] = from_union([from_str, from_none], self.next_cursor)
        return result


@dataclass
class ContractStorageValue:
    ledger_sequence: int
    """Ledger sequence at which this value was observed"""

    observed_at: str
    """Timestamp this snapshot row was recorded"""

    storage_key: str
    """Base64-encoded XDR LedgerKey this value was read from"""

    key: Any = None
    """Human-readable decoded storage key"""

    value: Any = None
    """Human-readable decoded value (absent when the entry was removed)"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractStorageValue':
        assert isinstance(obj, dict)
        ledger_sequence = from_int(obj.get("ledger_sequence"))
        observed_at = from_str(obj.get("observed_at"))
        storage_key = from_str(obj.get("storage_key"))
        key = obj.get("key")
        value = obj.get("value")
        return ContractStorageValue(ledger_sequence, observed_at, storage_key, key, value)

    def to_dict(self) -> dict:
        result: dict = {}
        result["ledger_sequence"] = from_int(self.ledger_sequence)
        result["observed_at"] = from_str(self.observed_at)
        result["storage_key"] = from_str(self.storage_key)
        result["key"] = self.key
        result["value"] = self.value
        return result


@dataclass
class ContractStorageHistoryResponse:
    contract_id: str
    """The contract whose storage history was queried"""

    has_more: bool
    """Whether more pages are available"""

    network: str
    """Network the contract is indexed on"""

    storage_key: str
    """The storage key whose history was queried"""

    values: list[ContractStorageValue]
    """Storage history entries, oldest first"""

    next_cursor: str | None = None
    """Opaque cursor to pass as the cursor parameter for the next page"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractStorageHistoryResponse':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contract_id"))
        has_more = from_bool(obj.get("has_more"))
        network = from_str(obj.get("network"))
        storage_key = from_str(obj.get("storage_key"))
        values = from_list(ContractStorageValue.from_dict, obj.get("values"))
        next_cursor = from_union([from_str, from_none], obj.get("next_cursor"))
        return ContractStorageHistoryResponse(contract_id, has_more, network, storage_key, values, next_cursor)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contract_id"] = from_str(self.contract_id)
        result["has_more"] = from_bool(self.has_more)
        result["network"] = from_str(self.network)
        result["storage_key"] = from_str(self.storage_key)
        result["values"] = from_list(lambda x: to_class(ContractStorageValue, x), self.values)
        if self.next_cursor is not None:
            result["next_cursor"] = from_union([from_str, from_none], self.next_cursor)
        return result


@dataclass
class ContractStorageResponse:
    contract_id: str
    """Soroban contract address"""

    network: Network
    """Network queried"""

    values: list[ContractStorageValue]
    """Storage snapshot values (latest, or full history when queried via /storage/history)"""

    @staticmethod
    def from_dict(obj: Any) -> 'ContractStorageResponse':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contract_id"))
        network = Network(obj.get("network"))
        values = from_list(ContractStorageValue.from_dict, obj.get("values"))
        return ContractStorageResponse(contract_id, network, values)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contract_id"] = from_str(self.contract_id)
        result["network"] = to_enum(Network, self.network)
        result["values"] = from_list(lambda x: to_class(ContractStorageValue, x), self.values)
        return result


class Code(Enum):
    """Machine-readable error code. Matches httputil.ErrorCode exactly
    (services/api/internal/httputil/errors.go).
    """
    CONFLICT = "CONFLICT"
    FORBIDDEN = "FORBIDDEN"
    INTERNAL = "INTERNAL"
    INVALID_ARGUMENT = "INVALID_ARGUMENT"
    NOT_FOUND = "NOT_FOUND"
    PAYLOAD_TOO_LARGE = "PAYLOAD_TOO_LARGE"
    RATE_LIMITED = "RATE_LIMITED"
    UNAUTHORIZED = "UNAUTHORIZED"
    UNAVAILABLE = "UNAVAILABLE"


@dataclass
class Error:
    code: Code
    """Machine-readable error code. Matches httputil.ErrorCode exactly
    (services/api/internal/httputil/errors.go).
    """
    message: str
    """Human-readable error message"""

    request_id: str | None = None
    """Request ID for debugging"""

    @staticmethod
    def from_dict(obj: Any) -> 'Error':
        assert isinstance(obj, dict)
        code = Code(obj.get("code"))
        message = from_str(obj.get("message"))
        request_id = from_union([from_str, from_none], obj.get("request_id"))
        return Error(code, message, request_id)

    def to_dict(self) -> dict:
        result: dict = {}
        result["code"] = to_enum(Code, self.code)
        result["message"] = from_str(self.message)
        if self.request_id is not None:
            result["request_id"] = from_union([from_str, from_none], self.request_id)
        return result


@dataclass
class ErrorResponse:
    error: Error

    @staticmethod
    def from_dict(obj: Any) -> 'ErrorResponse':
        assert isinstance(obj, dict)
        error = Error.from_dict(obj.get("error"))
        return ErrorResponse(error)

    def to_dict(self) -> dict:
        result: dict = {}
        result["error"] = to_class(Error, self.error)
        return result


class EventType(Enum):
    """Type of event"""

    CONTRACT = "contract"
    DIAGNOSTIC = "diagnostic"
    SYSTEM = "system"


@dataclass
class SorobanEvent:
    contract_id: str
    """Soroban contract address"""

    created_at: str
    """Timestamp when event was indexed"""

    data: str
    """Event data (XDR-encoded)"""

    event_index: int
    """Event index within transaction"""

    event_type: EventType
    """Type of event"""

    id: UUID
    """Unique event identifier"""

    ledger_sequence: int
    """Ledger sequence number"""

    ledger_timestamp: str
    """Ledger timestamp in ISO 8601 format"""

    topics: list[str]
    """Event topics (XDR-encoded)"""

    transaction_hash: str
    """Transaction hash (XDR-encoded)"""

    @staticmethod
    def from_dict(obj: Any) -> 'SorobanEvent':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contract_id"))
        created_at = from_str(obj.get("created_at"))
        data = from_str(obj.get("data"))
        event_index = from_int(obj.get("event_index"))
        event_type = EventType(obj.get("event_type"))
        id = UUID(obj.get("id"))
        ledger_sequence = from_int(obj.get("ledger_sequence"))
        ledger_timestamp = from_str(obj.get("ledger_timestamp"))
        topics = from_list(from_str, obj.get("topics"))
        transaction_hash = from_str(obj.get("transaction_hash"))
        return SorobanEvent(contract_id, created_at, data, event_index, event_type, id, ledger_sequence, ledger_timestamp, topics, transaction_hash)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contract_id"] = from_str(self.contract_id)
        result["created_at"] = from_str(self.created_at)
        result["data"] = from_str(self.data)
        result["event_index"] = from_int(self.event_index)
        result["event_type"] = to_enum(EventType, self.event_type)
        result["id"] = str(self.id)
        result["ledger_sequence"] = from_int(self.ledger_sequence)
        result["ledger_timestamp"] = from_str(self.ledger_timestamp)
        result["topics"] = from_list(from_str, self.topics)
        result["transaction_hash"] = from_str(self.transaction_hash)
        return result


@dataclass
class EventListResponse:
    events: list[SorobanEvent]
    """List of events"""

    has_more: bool
    """Whether more results are available"""

    next_cursor: str
    """Opaque cursor for next page (null if has_more is false)"""

    @staticmethod
    def from_dict(obj: Any) -> 'EventListResponse':
        assert isinstance(obj, dict)
        events = from_list(SorobanEvent.from_dict, obj.get("events"))
        has_more = from_bool(obj.get("has_more"))
        next_cursor = from_str(obj.get("next_cursor"))
        return EventListResponse(events, has_more, next_cursor)

    def to_dict(self) -> dict:
        result: dict = {}
        result["events"] = from_list(lambda x: to_class(SorobanEvent, x), self.events)
        result["has_more"] = from_bool(self.has_more)
        result["next_cursor"] = from_str(self.next_cursor)
        return result


class IndexerStatsResponseStatus(Enum):
    """Indexer health status"""

    HEALTHY = "healthy"
    LAGGING = "lagging"
    STALLED = "stalled"


@dataclass
class IndexerStatsResponse:
    avg_poll_duration_ms: int
    """Average poll duration in milliseconds"""

    chain_tip_ledger: int
    """Current chain tip ledger (from RPC)"""

    events_indexed_total: int
    """Cumulative events indexed"""

    events_last_poll: int
    """Events processed in last poll"""

    lag_ledgers: int
    """Number of ledgers behind chain tip"""

    lag_seconds_estimated: float
    """Estimated wall-clock staleness in seconds: lag_ledgers times Stellar's protocol-target
    ledger close time (~5s). Null whenever lag_ledgers is null. See
    docs/observability/data-freshness.md for the full freshness contract this field is part
    of.
    """
    last_ledger_indexed: int
    """Latest indexed ledger sequence"""

    last_poll_at: str
    """Timestamp of last successful poll"""

    network: str
    """Network name from NETWORK environment variable"""

    status: IndexerStatsResponseStatus
    """Indexer health status"""

    @staticmethod
    def from_dict(obj: Any) -> 'IndexerStatsResponse':
        assert isinstance(obj, dict)
        avg_poll_duration_ms = from_int(obj.get("avg_poll_duration_ms"))
        chain_tip_ledger = from_int(obj.get("chain_tip_ledger"))
        events_indexed_total = from_int(obj.get("events_indexed_total"))
        events_last_poll = from_int(obj.get("events_last_poll"))
        lag_ledgers = from_int(obj.get("lag_ledgers"))
        lag_seconds_estimated = from_float(obj.get("lag_seconds_estimated"))
        last_ledger_indexed = from_int(obj.get("last_ledger_indexed"))
        last_poll_at = from_str(obj.get("last_poll_at"))
        network = from_str(obj.get("network"))
        status = IndexerStatsResponseStatus(obj.get("status"))
        return IndexerStatsResponse(avg_poll_duration_ms, chain_tip_ledger, events_indexed_total, events_last_poll, lag_ledgers, lag_seconds_estimated, last_ledger_indexed, last_poll_at, network, status)

    def to_dict(self) -> dict:
        result: dict = {}
        result["avg_poll_duration_ms"] = from_int(self.avg_poll_duration_ms)
        result["chain_tip_ledger"] = from_int(self.chain_tip_ledger)
        result["events_indexed_total"] = from_int(self.events_indexed_total)
        result["events_last_poll"] = from_int(self.events_last_poll)
        result["lag_ledgers"] = from_int(self.lag_ledgers)
        result["lag_seconds_estimated"] = to_float(self.lag_seconds_estimated)
        result["last_ledger_indexed"] = from_int(self.last_ledger_indexed)
        result["last_poll_at"] = from_str(self.last_poll_at)
        result["network"] = from_str(self.network)
        result["status"] = to_enum(IndexerStatsResponseStatus, self.status)
        return result


@dataclass
class ListAPIKeysResponse:
    api_keys: list[APIKeyResponse]
    has_more: bool
    """Whether another page is available."""

    next_cursor: str
    """Opaque cursor for the next page (null if has_more is false)."""

    @staticmethod
    def from_dict(obj: Any) -> 'ListAPIKeysResponse':
        assert isinstance(obj, dict)
        api_keys = from_list(APIKeyResponse.from_dict, obj.get("api_keys"))
        has_more = from_bool(obj.get("has_more"))
        next_cursor = from_str(obj.get("next_cursor"))
        return ListAPIKeysResponse(api_keys, has_more, next_cursor)

    def to_dict(self) -> dict:
        result: dict = {}
        result["api_keys"] = from_list(lambda x: to_class(APIKeyResponse, x), self.api_keys)
        result["has_more"] = from_bool(self.has_more)
        result["next_cursor"] = from_str(self.next_cursor)
        return result


@dataclass
class ListContractsResponse:
    contracts: list[ContractResponse]
    has_more: bool
    """Whether another page is available."""

    next_cursor: str
    """Opaque cursor for the next page (null if has_more is false)."""

    @staticmethod
    def from_dict(obj: Any) -> 'ListContractsResponse':
        assert isinstance(obj, dict)
        contracts = from_list(ContractResponse.from_dict, obj.get("contracts"))
        has_more = from_bool(obj.get("has_more"))
        next_cursor = from_str(obj.get("next_cursor"))
        return ListContractsResponse(contracts, has_more, next_cursor)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contracts"] = from_list(lambda x: to_class(ContractResponse, x), self.contracts)
        result["has_more"] = from_bool(self.has_more)
        result["next_cursor"] = from_str(self.next_cursor)
        return result


@dataclass
class WebhookSubscription:
    contract_id: str
    created_at: str
    id: UUID
    network: str
    target_url: str
    api_key_id: str | None = None
    """Omitted when empty"""

    paused_at: str | None = None
    """Present while deliveries are paused"""

    secret: str | None = None
    """HMAC signing secret for deliveries; omitted when empty"""

    topic0: str | None = None
    """Topic filter; omitted when unfiltered"""

    @staticmethod
    def from_dict(obj: Any) -> 'WebhookSubscription':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contractId"))
        created_at = from_str(obj.get("createdAt"))
        id = UUID(obj.get("id"))
        network = from_str(obj.get("network"))
        target_url = from_str(obj.get("targetUrl"))
        api_key_id = from_union([from_str, from_none], obj.get("apiKeyId"))
        paused_at = from_union([from_str, from_none], obj.get("pausedAt"))
        secret = from_union([from_str, from_none], obj.get("secret"))
        topic0 = from_union([from_str, from_none], obj.get("topic0"))
        return WebhookSubscription(contract_id, created_at, id, network, target_url, api_key_id, paused_at, secret, topic0)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contractId"] = from_str(self.contract_id)
        result["createdAt"] = from_str(self.created_at)
        result["id"] = str(self.id)
        result["network"] = from_str(self.network)
        result["targetUrl"] = from_str(self.target_url)
        if self.api_key_id is not None:
            result["apiKeyId"] = from_union([from_str, from_none], self.api_key_id)
        if self.paused_at is not None:
            result["pausedAt"] = from_union([from_str, from_none], self.paused_at)
        if self.secret is not None:
            result["secret"] = from_union([from_str, from_none], self.secret)
        if self.topic0 is not None:
            result["topic0"] = from_union([from_str, from_none], self.topic0)
        return result


@dataclass
class ListWebhooksResponse:
    has_more: bool
    """Whether another page is available."""

    next_cursor: str
    """Opaque cursor for the next page (null if has_more is false)."""

    webhooks: list[WebhookSubscription]

    @staticmethod
    def from_dict(obj: Any) -> 'ListWebhooksResponse':
        assert isinstance(obj, dict)
        has_more = from_bool(obj.get("has_more"))
        next_cursor = from_str(obj.get("next_cursor"))
        webhooks = from_list(WebhookSubscription.from_dict, obj.get("webhooks"))
        return ListWebhooksResponse(has_more, next_cursor, webhooks)

    def to_dict(self) -> dict:
        result: dict = {}
        result["has_more"] = from_bool(self.has_more)
        result["next_cursor"] = from_str(self.next_cursor)
        result["webhooks"] = from_list(lambda x: to_class(WebhookSubscription, x), self.webhooks)
        return result


class LivenessResponseStatus(Enum):
    """Always "ok" while the process is up — no dependency checks."""

    OK = "ok"


@dataclass
class LivenessResponse:
    status: LivenessResponseStatus
    """Always "ok" while the process is up — no dependency checks."""

    @staticmethod
    def from_dict(obj: Any) -> 'LivenessResponse':
        assert isinstance(obj, dict)
        status = LivenessResponseStatus(obj.get("status"))
        return LivenessResponse(status)

    def to_dict(self) -> dict:
        result: dict = {}
        result["status"] = to_enum(LivenessResponseStatus, self.status)
        return result


@dataclass
class ReadyChecks:
    grpc_api: str
    """"ok" or "error: <message>\""""

    postgres: str
    """"ok" or "error: <message>\""""

    redis: str
    """"ok" or "error: <message>\""""

    @staticmethod
    def from_dict(obj: Any) -> 'ReadyChecks':
        assert isinstance(obj, dict)
        grpc_api = from_str(obj.get("grpc_api"))
        postgres = from_str(obj.get("postgres"))
        redis = from_str(obj.get("redis"))
        return ReadyChecks(grpc_api, postgres, redis)

    def to_dict(self) -> dict:
        result: dict = {}
        result["grpc_api"] = from_str(self.grpc_api)
        result["postgres"] = from_str(self.postgres)
        result["redis"] = from_str(self.redis)
        return result


class ReadyResponseStatus(Enum):
    """"degraded" when any dependency check in `checks` failed."""

    DEGRADED = "degraded"
    OK = "ok"


@dataclass
class ReadyResponse:
    checks: ReadyChecks
    indexer_lag: int
    """Ledgers behind chain tip, from system_state. Null when Postgres is unreachable or the
    chain-tip cache hasn't been populated yet.
    """
    status: ReadyResponseStatus
    """"degraded" when any dependency check in `checks` failed."""

    @staticmethod
    def from_dict(obj: Any) -> 'ReadyResponse':
        assert isinstance(obj, dict)
        checks = ReadyChecks.from_dict(obj.get("checks"))
        indexer_lag = from_int(obj.get("indexer_lag"))
        status = ReadyResponseStatus(obj.get("status"))
        return ReadyResponse(checks, indexer_lag, status)

    def to_dict(self) -> dict:
        result: dict = {}
        result["checks"] = to_class(ReadyChecks, self.checks)
        result["indexer_lag"] = from_int(self.indexer_lag)
        result["status"] = to_enum(ReadyResponseStatus, self.status)
        return result


@dataclass
class TokenMetadataResponse:
    contract_id: str
    """Soroban contract address"""

    is_token: bool
    """True when the contract was resolved and implements the SEP-41 read interface. False for
    both "not yet resolved" and "resolved, not a token".
    """
    network: Network
    """Network queried"""

    decimals: int | None = None
    """Token decimals, from decimals(). Null unless is_token is true."""

    name: str | None = None
    """Token name, from name(). Null unless is_token is true."""

    resolved_at: str | None = None
    """When this contract was last resolved. Null if never resolved."""

    symbol: str | None = None
    """Token symbol, from symbol(). Null unless is_token is true."""

    @staticmethod
    def from_dict(obj: Any) -> 'TokenMetadataResponse':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contract_id"))
        is_token = from_bool(obj.get("is_token"))
        network = Network(obj.get("network"))
        decimals = from_union([from_int, from_none], obj.get("decimals"))
        name = from_union([from_str, from_none], obj.get("name"))
        resolved_at = from_union([from_str, from_none], obj.get("resolved_at"))
        symbol = from_union([from_str, from_none], obj.get("symbol"))
        return TokenMetadataResponse(contract_id, is_token, network, decimals, name, resolved_at, symbol)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contract_id"] = from_str(self.contract_id)
        result["is_token"] = from_bool(self.is_token)
        result["network"] = to_enum(Network, self.network)
        if self.decimals is not None:
            result["decimals"] = from_union([from_int, from_none], self.decimals)
        if self.name is not None:
            result["name"] = from_union([from_str, from_none], self.name)
        if self.resolved_at is not None:
            result["resolved_at"] = from_union([from_str, from_none], self.resolved_at)
        if self.symbol is not None:
            result["symbol"] = from_union([from_str, from_none], self.symbol)
        return result


@dataclass
class UsageRollupRow:
    avg_duration_ms: float
    error_count: int
    period_end: str
    period_start: str
    request_count: int

    @staticmethod
    def from_dict(obj: Any) -> 'UsageRollupRow':
        assert isinstance(obj, dict)
        avg_duration_ms = from_float(obj.get("avg_duration_ms"))
        error_count = from_int(obj.get("error_count"))
        period_end = from_str(obj.get("period_end"))
        period_start = from_str(obj.get("period_start"))
        request_count = from_int(obj.get("request_count"))
        return UsageRollupRow(avg_duration_ms, error_count, period_end, period_start, request_count)

    def to_dict(self) -> dict:
        result: dict = {}
        result["avg_duration_ms"] = to_float(self.avg_duration_ms)
        result["error_count"] = from_int(self.error_count)
        result["period_end"] = from_str(self.period_end)
        result["period_start"] = from_str(self.period_start)
        result["request_count"] = from_int(self.request_count)
        return result


@dataclass
class UsageResponse:
    api_key_id: UUID
    days: list[UsageRollupRow]
    """Daily buckets from the maintained usage_rollup table, oldest first; empty when the window
    has no rollup rows.
    """
    usage_response_from: str
    to: str
    total_errors: int
    total_requests: int

    @staticmethod
    def from_dict(obj: Any) -> 'UsageResponse':
        assert isinstance(obj, dict)
        api_key_id = UUID(obj.get("api_key_id"))
        days = from_list(UsageRollupRow.from_dict, obj.get("days"))
        usage_response_from = from_str(obj.get("from"))
        to = from_str(obj.get("to"))
        total_errors = from_int(obj.get("total_errors"))
        total_requests = from_int(obj.get("total_requests"))
        return UsageResponse(api_key_id, days, usage_response_from, to, total_errors, total_requests)

    def to_dict(self) -> dict:
        result: dict = {}
        result["api_key_id"] = str(self.api_key_id)
        result["days"] = from_list(lambda x: to_class(UsageRollupRow, x), self.days)
        result["from"] = from_str(self.usage_response_from)
        result["to"] = from_str(self.to)
        result["total_errors"] = from_int(self.total_errors)
        result["total_requests"] = from_int(self.total_requests)
        return result


@dataclass
class VersionResponse:
    build_timestamp: str
    """RFC 3339 build time, or "unknown" when not injected at build time. Not typed as date-time
    because of that sentinel.
    """
    commit_sha: str
    """Full git commit SHA the binary was built from, or "unknown" when not injected at build
    time.
    """
    schema_version: str
    """Highest applied migration version from _sqlx_migrations, as a string. Null when no
    migrations have been applied yet or when Postgres is unreachable — the endpoint still
    returns 200 in that case so build metadata stays available during an outage.
    """
    version: str
    """Semantic version tag of the running build, or "dev" for a binary built without release
    ldflags.
    """

    @staticmethod
    def from_dict(obj: Any) -> 'VersionResponse':
        assert isinstance(obj, dict)
        build_timestamp = from_str(obj.get("build_timestamp"))
        commit_sha = from_str(obj.get("commit_sha"))
        schema_version = from_str(obj.get("schema_version"))
        version = from_str(obj.get("version"))
        return VersionResponse(build_timestamp, commit_sha, schema_version, version)

    def to_dict(self) -> dict:
        result: dict = {}
        result["build_timestamp"] = from_str(self.build_timestamp)
        result["commit_sha"] = from_str(self.commit_sha)
        result["schema_version"] = from_str(self.schema_version)
        result["version"] = from_str(self.version)
        return result


@dataclass
class WebhookCreateRequest:
    contract_id: str
    target_url: str
    """Delivery target; must be https with a publicly resolvable, non-private host"""

    network: str | None = None
    topic0: str | None = None
    """Optional topic filter"""

    @staticmethod
    def from_dict(obj: Any) -> 'WebhookCreateRequest':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contractId"))
        target_url = from_str(obj.get("targetUrl"))
        network = from_union([from_str, from_none], obj.get("network"))
        topic0 = from_union([from_str, from_none], obj.get("topic0"))
        return WebhookCreateRequest(contract_id, target_url, network, topic0)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contractId"] = from_str(self.contract_id)
        result["targetUrl"] = from_str(self.target_url)
        if self.network is not None:
            result["network"] = from_union([from_str, from_none], self.network)
        if self.topic0 is not None:
            result["topic0"] = from_union([from_str, from_none], self.topic0)
        return result


@dataclass
class WebhookCreateResponse:
    contract_id: str
    id: UUID
    network: str
    secret: str
    """HMAC signing secret — shown here and in the listing"""

    target_url: str

    @staticmethod
    def from_dict(obj: Any) -> 'WebhookCreateResponse':
        assert isinstance(obj, dict)
        contract_id = from_str(obj.get("contractId"))
        id = UUID(obj.get("id"))
        network = from_str(obj.get("network"))
        secret = from_str(obj.get("secret"))
        target_url = from_str(obj.get("targetUrl"))
        return WebhookCreateResponse(contract_id, id, network, secret, target_url)

    def to_dict(self) -> dict:
        result: dict = {}
        result["contractId"] = from_str(self.contract_id)
        result["id"] = str(self.id)
        result["network"] = from_str(self.network)
        result["secret"] = from_str(self.secret)
        result["targetUrl"] = from_str(self.target_url)
        return result


@dataclass
class WebhookDelivery:
    attempt: int
    attempts: int
    delivered_at: str
    event_id: str
    id: int
    status: str
    subscription_id: UUID
    success: bool
    response_body: str | None = None
    """Omitted when empty"""

    status_code: int | None = None
    """HTTP status of the delivery attempt; omitted when none occurred"""

    @staticmethod
    def from_dict(obj: Any) -> 'WebhookDelivery':
        assert isinstance(obj, dict)
        attempt = from_int(obj.get("attempt"))
        attempts = from_int(obj.get("attempts"))
        delivered_at = from_str(obj.get("deliveredAt"))
        event_id = from_str(obj.get("eventId"))
        id = from_int(obj.get("id"))
        status = from_str(obj.get("status"))
        subscription_id = UUID(obj.get("subscriptionId"))
        success = from_bool(obj.get("success"))
        response_body = from_union([from_str, from_none], obj.get("responseBody"))
        status_code = from_union([from_int, from_none], obj.get("statusCode"))
        return WebhookDelivery(attempt, attempts, delivered_at, event_id, id, status, subscription_id, success, response_body, status_code)

    def to_dict(self) -> dict:
        result: dict = {}
        result["attempt"] = from_int(self.attempt)
        result["attempts"] = from_int(self.attempts)
        result["deliveredAt"] = from_str(self.delivered_at)
        result["eventId"] = from_str(self.event_id)
        result["id"] = from_int(self.id)
        result["status"] = from_str(self.status)
        result["subscriptionId"] = str(self.subscription_id)
        result["success"] = from_bool(self.success)
        if self.response_body is not None:
            result["responseBody"] = from_union([from_str, from_none], self.response_body)
        if self.status_code is not None:
            result["statusCode"] = from_union([from_int, from_none], self.status_code)
        return result


class WebhookReplayResponseStatus(Enum):
    FAILED = "failed"
    SUCCESS = "success"


@dataclass
class WebhookReplayResponse:
    attempt: int
    response_body: str
    """Truncated to 500 characters"""

    status: WebhookReplayResponseStatus
    status_code: int
    """0 when no HTTP response occurred"""

    success: bool

    @staticmethod
    def from_dict(obj: Any) -> 'WebhookReplayResponse':
        assert isinstance(obj, dict)
        attempt = from_int(obj.get("attempt"))
        response_body = from_str(obj.get("response_body"))
        status = WebhookReplayResponseStatus(obj.get("status"))
        status_code = from_int(obj.get("status_code"))
        success = from_bool(obj.get("success"))
        return WebhookReplayResponse(attempt, response_body, status, status_code, success)

    def to_dict(self) -> dict:
        result: dict = {}
        result["attempt"] = from_int(self.attempt)
        result["response_body"] = from_str(self.response_body)
        result["status"] = to_enum(WebhookReplayResponseStatus, self.status)
        result["status_code"] = from_int(self.status_code)
        result["success"] = from_bool(self.success)
        return result


@dataclass
class WebhookRotateSecretResponse:
    id: UUID
    previous_secret: str
    """The demoted secret, now serving as secondary during the overlap window"""

    secret: str
    """The new primary signing secret (whsec_ prefixed)"""

    @staticmethod
    def from_dict(obj: Any) -> 'WebhookRotateSecretResponse':
        assert isinstance(obj, dict)
        id = UUID(obj.get("id"))
        previous_secret = from_str(obj.get("previousSecret"))
        secret = from_str(obj.get("secret"))
        return WebhookRotateSecretResponse(id, previous_secret, secret)

    def to_dict(self) -> dict:
        result: dict = {}
        result["id"] = str(self.id)
        result["previousSecret"] = from_str(self.previous_secret)
        result["secret"] = from_str(self.secret)
        return result


class WebhookStatusResponseStatus(Enum):
    PAUSED = "paused"
    RESUMED = "resumed"


@dataclass
class WebhookStatusResponse:
    status: WebhookStatusResponseStatus

    @staticmethod
    def from_dict(obj: Any) -> 'WebhookStatusResponse':
        assert isinstance(obj, dict)
        status = WebhookStatusResponseStatus(obj.get("status"))
        return WebhookStatusResponse(status)

    def to_dict(self) -> dict:
        result: dict = {}
        result["status"] = to_enum(WebhookStatusResponseStatus, self.status)
        return result


@dataclass
class OpenAPIModels:
    admin_key_usage_response: AdminKeyUsageResponse | None = None
    api_key_response: APIKeyResponse | None = None
    contract_call_request: ContractCallRequest | None = None
    contract_call_response: ContractCallResponse | None = None
    contract_event_field_schema: ContractEventFieldSchema | None = None
    contract_event_schema: ContractEventSchema | None = None
    contract_event_schema_response: ContractEventSchemaResponse | None = None
    contract_registration_request: ContractRegistrationRequest | None = None
    contract_response: ContractResponse | None = None
    contract_spec_function: ContractSpecFunction | None = None
    contract_spec_response: ContractSpecResponse | None = None
    contract_stats: ContractStats | None = None
    contract_stats_response: ContractStatsResponse | None = None
    contract_storage_history_response: ContractStorageHistoryResponse | None = None
    contract_storage_response: ContractStorageResponse | None = None
    contract_storage_value: ContractStorageValue | None = None
    endpoint_usage: EndpointUsage | None = None
    error_response: ErrorResponse | None = None
    event_list_response: EventListResponse | None = None
    indexer_stats_response: IndexerStatsResponse | None = None
    list_api_keys_response: ListAPIKeysResponse | None = None
    list_contracts_response: ListContractsResponse | None = None
    list_webhooks_response: ListWebhooksResponse | None = None
    liveness_response: LivenessResponse | None = None
    ready_checks: ReadyChecks | None = None
    ready_response: ReadyResponse | None = None
    soroban_event: SorobanEvent | None = None
    token_metadata_response: TokenMetadataResponse | None = None
    usage_response: UsageResponse | None = None
    usage_rollup_row: UsageRollupRow | None = None
    version_response: VersionResponse | None = None
    webhook_create_request: WebhookCreateRequest | None = None
    webhook_create_response: WebhookCreateResponse | None = None
    webhook_delivery: WebhookDelivery | None = None
    webhook_replay_response: WebhookReplayResponse | None = None
    webhook_rotate_secret_response: WebhookRotateSecretResponse | None = None
    webhook_status_response: WebhookStatusResponse | None = None
    webhook_subscription: WebhookSubscription | None = None

    @staticmethod
    def from_dict(obj: Any) -> 'OpenAPIModels':
        assert isinstance(obj, dict)
        admin_key_usage_response = from_union([AdminKeyUsageResponse.from_dict, from_none], obj.get("AdminKeyUsageResponse"))
        api_key_response = from_union([APIKeyResponse.from_dict, from_none], obj.get("APIKeyResponse"))
        contract_call_request = from_union([ContractCallRequest.from_dict, from_none], obj.get("ContractCallRequest"))
        contract_call_response = from_union([ContractCallResponse.from_dict, from_none], obj.get("ContractCallResponse"))
        contract_event_field_schema = from_union([ContractEventFieldSchema.from_dict, from_none], obj.get("ContractEventFieldSchema"))
        contract_event_schema = from_union([ContractEventSchema.from_dict, from_none], obj.get("ContractEventSchema"))
        contract_event_schema_response = from_union([ContractEventSchemaResponse.from_dict, from_none], obj.get("ContractEventSchemaResponse"))
        contract_registration_request = from_union([ContractRegistrationRequest.from_dict, from_none], obj.get("ContractRegistrationRequest"))
        contract_response = from_union([ContractResponse.from_dict, from_none], obj.get("ContractResponse"))
        contract_spec_function = from_union([ContractSpecFunction.from_dict, from_none], obj.get("ContractSpecFunction"))
        contract_spec_response = from_union([ContractSpecResponse.from_dict, from_none], obj.get("ContractSpecResponse"))
        contract_stats = from_union([ContractStats.from_dict, from_none], obj.get("ContractStats"))
        contract_stats_response = from_union([ContractStatsResponse.from_dict, from_none], obj.get("ContractStatsResponse"))
        contract_storage_history_response = from_union([ContractStorageHistoryResponse.from_dict, from_none], obj.get("ContractStorageHistoryResponse"))
        contract_storage_response = from_union([ContractStorageResponse.from_dict, from_none], obj.get("ContractStorageResponse"))
        contract_storage_value = from_union([ContractStorageValue.from_dict, from_none], obj.get("ContractStorageValue"))
        endpoint_usage = from_union([EndpointUsage.from_dict, from_none], obj.get("EndpointUsage"))
        error_response = from_union([ErrorResponse.from_dict, from_none], obj.get("ErrorResponse"))
        event_list_response = from_union([EventListResponse.from_dict, from_none], obj.get("EventListResponse"))
        indexer_stats_response = from_union([IndexerStatsResponse.from_dict, from_none], obj.get("IndexerStatsResponse"))
        list_api_keys_response = from_union([ListAPIKeysResponse.from_dict, from_none], obj.get("ListAPIKeysResponse"))
        list_contracts_response = from_union([ListContractsResponse.from_dict, from_none], obj.get("ListContractsResponse"))
        list_webhooks_response = from_union([ListWebhooksResponse.from_dict, from_none], obj.get("ListWebhooksResponse"))
        liveness_response = from_union([LivenessResponse.from_dict, from_none], obj.get("LivenessResponse"))
        ready_checks = from_union([ReadyChecks.from_dict, from_none], obj.get("ReadyChecks"))
        ready_response = from_union([ReadyResponse.from_dict, from_none], obj.get("ReadyResponse"))
        soroban_event = from_union([SorobanEvent.from_dict, from_none], obj.get("SorobanEvent"))
        token_metadata_response = from_union([TokenMetadataResponse.from_dict, from_none], obj.get("TokenMetadataResponse"))
        usage_response = from_union([UsageResponse.from_dict, from_none], obj.get("UsageResponse"))
        usage_rollup_row = from_union([UsageRollupRow.from_dict, from_none], obj.get("UsageRollupRow"))
        version_response = from_union([VersionResponse.from_dict, from_none], obj.get("VersionResponse"))
        webhook_create_request = from_union([WebhookCreateRequest.from_dict, from_none], obj.get("WebhookCreateRequest"))
        webhook_create_response = from_union([WebhookCreateResponse.from_dict, from_none], obj.get("WebhookCreateResponse"))
        webhook_delivery = from_union([WebhookDelivery.from_dict, from_none], obj.get("WebhookDelivery"))
        webhook_replay_response = from_union([WebhookReplayResponse.from_dict, from_none], obj.get("WebhookReplayResponse"))
        webhook_rotate_secret_response = from_union([WebhookRotateSecretResponse.from_dict, from_none], obj.get("WebhookRotateSecretResponse"))
        webhook_status_response = from_union([WebhookStatusResponse.from_dict, from_none], obj.get("WebhookStatusResponse"))
        webhook_subscription = from_union([WebhookSubscription.from_dict, from_none], obj.get("WebhookSubscription"))
        return OpenAPIModels(admin_key_usage_response, api_key_response, contract_call_request, contract_call_response, contract_event_field_schema, contract_event_schema, contract_event_schema_response, contract_registration_request, contract_response, contract_spec_function, contract_spec_response, contract_stats, contract_stats_response, contract_storage_history_response, contract_storage_response, contract_storage_value, endpoint_usage, error_response, event_list_response, indexer_stats_response, list_api_keys_response, list_contracts_response, list_webhooks_response, liveness_response, ready_checks, ready_response, soroban_event, token_metadata_response, usage_response, usage_rollup_row, version_response, webhook_create_request, webhook_create_response, webhook_delivery, webhook_replay_response, webhook_rotate_secret_response, webhook_status_response, webhook_subscription)

    def to_dict(self) -> dict:
        result: dict = {}
        if self.admin_key_usage_response is not None:
            result["AdminKeyUsageResponse"] = from_union([lambda x: to_class(AdminKeyUsageResponse, x), from_none], self.admin_key_usage_response)
        if self.api_key_response is not None:
            result["APIKeyResponse"] = from_union([lambda x: to_class(APIKeyResponse, x), from_none], self.api_key_response)
        if self.contract_call_request is not None:
            result["ContractCallRequest"] = from_union([lambda x: to_class(ContractCallRequest, x), from_none], self.contract_call_request)
        if self.contract_call_response is not None:
            result["ContractCallResponse"] = from_union([lambda x: to_class(ContractCallResponse, x), from_none], self.contract_call_response)
        if self.contract_event_field_schema is not None:
            result["ContractEventFieldSchema"] = from_union([lambda x: to_class(ContractEventFieldSchema, x), from_none], self.contract_event_field_schema)
        if self.contract_event_schema is not None:
            result["ContractEventSchema"] = from_union([lambda x: to_class(ContractEventSchema, x), from_none], self.contract_event_schema)
        if self.contract_event_schema_response is not None:
            result["ContractEventSchemaResponse"] = from_union([lambda x: to_class(ContractEventSchemaResponse, x), from_none], self.contract_event_schema_response)
        if self.contract_registration_request is not None:
            result["ContractRegistrationRequest"] = from_union([lambda x: to_class(ContractRegistrationRequest, x), from_none], self.contract_registration_request)
        if self.contract_response is not None:
            result["ContractResponse"] = from_union([lambda x: to_class(ContractResponse, x), from_none], self.contract_response)
        if self.contract_spec_function is not None:
            result["ContractSpecFunction"] = from_union([lambda x: to_class(ContractSpecFunction, x), from_none], self.contract_spec_function)
        if self.contract_spec_response is not None:
            result["ContractSpecResponse"] = from_union([lambda x: to_class(ContractSpecResponse, x), from_none], self.contract_spec_response)
        if self.contract_stats is not None:
            result["ContractStats"] = from_union([lambda x: to_class(ContractStats, x), from_none], self.contract_stats)
        if self.contract_stats_response is not None:
            result["ContractStatsResponse"] = from_union([lambda x: to_class(ContractStatsResponse, x), from_none], self.contract_stats_response)
        if self.contract_storage_history_response is not None:
            result["ContractStorageHistoryResponse"] = from_union([lambda x: to_class(ContractStorageHistoryResponse, x), from_none], self.contract_storage_history_response)
        if self.contract_storage_response is not None:
            result["ContractStorageResponse"] = from_union([lambda x: to_class(ContractStorageResponse, x), from_none], self.contract_storage_response)
        if self.contract_storage_value is not None:
            result["ContractStorageValue"] = from_union([lambda x: to_class(ContractStorageValue, x), from_none], self.contract_storage_value)
        if self.endpoint_usage is not None:
            result["EndpointUsage"] = from_union([lambda x: to_class(EndpointUsage, x), from_none], self.endpoint_usage)
        if self.error_response is not None:
            result["ErrorResponse"] = from_union([lambda x: to_class(ErrorResponse, x), from_none], self.error_response)
        if self.event_list_response is not None:
            result["EventListResponse"] = from_union([lambda x: to_class(EventListResponse, x), from_none], self.event_list_response)
        if self.indexer_stats_response is not None:
            result["IndexerStatsResponse"] = from_union([lambda x: to_class(IndexerStatsResponse, x), from_none], self.indexer_stats_response)
        if self.list_api_keys_response is not None:
            result["ListAPIKeysResponse"] = from_union([lambda x: to_class(ListAPIKeysResponse, x), from_none], self.list_api_keys_response)
        if self.list_contracts_response is not None:
            result["ListContractsResponse"] = from_union([lambda x: to_class(ListContractsResponse, x), from_none], self.list_contracts_response)
        if self.list_webhooks_response is not None:
            result["ListWebhooksResponse"] = from_union([lambda x: to_class(ListWebhooksResponse, x), from_none], self.list_webhooks_response)
        if self.liveness_response is not None:
            result["LivenessResponse"] = from_union([lambda x: to_class(LivenessResponse, x), from_none], self.liveness_response)
        if self.ready_checks is not None:
            result["ReadyChecks"] = from_union([lambda x: to_class(ReadyChecks, x), from_none], self.ready_checks)
        if self.ready_response is not None:
            result["ReadyResponse"] = from_union([lambda x: to_class(ReadyResponse, x), from_none], self.ready_response)
        if self.soroban_event is not None:
            result["SorobanEvent"] = from_union([lambda x: to_class(SorobanEvent, x), from_none], self.soroban_event)
        if self.token_metadata_response is not None:
            result["TokenMetadataResponse"] = from_union([lambda x: to_class(TokenMetadataResponse, x), from_none], self.token_metadata_response)
        if self.usage_response is not None:
            result["UsageResponse"] = from_union([lambda x: to_class(UsageResponse, x), from_none], self.usage_response)
        if self.usage_rollup_row is not None:
            result["UsageRollupRow"] = from_union([lambda x: to_class(UsageRollupRow, x), from_none], self.usage_rollup_row)
        if self.version_response is not None:
            result["VersionResponse"] = from_union([lambda x: to_class(VersionResponse, x), from_none], self.version_response)
        if self.webhook_create_request is not None:
            result["WebhookCreateRequest"] = from_union([lambda x: to_class(WebhookCreateRequest, x), from_none], self.webhook_create_request)
        if self.webhook_create_response is not None:
            result["WebhookCreateResponse"] = from_union([lambda x: to_class(WebhookCreateResponse, x), from_none], self.webhook_create_response)
        if self.webhook_delivery is not None:
            result["WebhookDelivery"] = from_union([lambda x: to_class(WebhookDelivery, x), from_none], self.webhook_delivery)
        if self.webhook_replay_response is not None:
            result["WebhookReplayResponse"] = from_union([lambda x: to_class(WebhookReplayResponse, x), from_none], self.webhook_replay_response)
        if self.webhook_rotate_secret_response is not None:
            result["WebhookRotateSecretResponse"] = from_union([lambda x: to_class(WebhookRotateSecretResponse, x), from_none], self.webhook_rotate_secret_response)
        if self.webhook_status_response is not None:
            result["WebhookStatusResponse"] = from_union([lambda x: to_class(WebhookStatusResponse, x), from_none], self.webhook_status_response)
        if self.webhook_subscription is not None:
            result["WebhookSubscription"] = from_union([lambda x: to_class(WebhookSubscription, x), from_none], self.webhook_subscription)
        return result


def open_api_models_from_dict(s: Any) -> OpenAPIModels:
    return OpenAPIModels.from_dict(s)


def open_api_models_to_dict(x: OpenAPIModels) -> Any:
    return to_class(OpenAPIModels, x)
