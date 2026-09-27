// Code generated from JSON Schema using quicktype. DO NOT EDIT.
// To parse and unparse this JSON data, add this code to your project and do:
//
//    openAPIModels, err := UnmarshalOpenAPIModels(bytes)
//    bytes, err = openAPIModels.Marshal()

package openapi

import "time"

import "encoding/json"

func UnmarshalOpenAPIModels(data []byte) (OpenAPIModels, error) {
	var r OpenAPIModels
	err := json.Unmarshal(data, &r)
	return r, err
}

func (r *OpenAPIModels) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

type OpenAPIModels struct {
	AdminKeyUsageResponse          *AdminKeyUsageResponse          `json:"AdminKeyUsageResponse,omitempty"`
	APIKeyResponse                 *APIKeyResponse                 `json:"APIKeyResponse,omitempty"`
	ContractCallRequest            *ContractCallRequest            `json:"ContractCallRequest,omitempty"`
	ContractCallResponse           *ContractCallResponse           `json:"ContractCallResponse,omitempty"`
	ContractEventFieldSchema       *ContractEventFieldSchema       `json:"ContractEventFieldSchema,omitempty"`
	ContractEventSchema            *ContractEventSchema            `json:"ContractEventSchema,omitempty"`
	ContractEventSchemaResponse    *ContractEventSchemaResponse    `json:"ContractEventSchemaResponse,omitempty"`
	ContractRegistrationRequest    *ContractRegistrationRequest    `json:"ContractRegistrationRequest,omitempty"`
	ContractResponse               *ContractResponse               `json:"ContractResponse,omitempty"`
	ContractSpecFunction           *ContractSpecFunction           `json:"ContractSpecFunction,omitempty"`
	ContractSpecResponse           *ContractSpecResponse           `json:"ContractSpecResponse,omitempty"`
	ContractStats                  *ContractStats                  `json:"ContractStats,omitempty"`
	ContractStatsResponse          *ContractStatsResponse          `json:"ContractStatsResponse,omitempty"`
	ContractStorageHistoryResponse *ContractStorageHistoryResponse `json:"ContractStorageHistoryResponse,omitempty"`
	ContractStorageResponse        *ContractStorageResponse        `json:"ContractStorageResponse,omitempty"`
	ContractStorageValue           *ContractStorageValue           `json:"ContractStorageValue,omitempty"`
	EndpointUsage                  *EndpointUsage                  `json:"EndpointUsage,omitempty"`
	ErrorResponse                  *ErrorResponse                  `json:"ErrorResponse,omitempty"`
	EventListResponse              *EventListResponse              `json:"EventListResponse,omitempty"`
	IndexerStatsResponse           *IndexerStatsResponse           `json:"IndexerStatsResponse,omitempty"`
	ListAPIKeysResponse            *ListAPIKeysResponse            `json:"ListAPIKeysResponse,omitempty"`
	ListContractsResponse          *ListContractsResponse          `json:"ListContractsResponse,omitempty"`
	ListWebhooksResponse           *ListWebhooksResponse           `json:"ListWebhooksResponse,omitempty"`
	LivenessResponse               *LivenessResponse               `json:"LivenessResponse,omitempty"`
	ReadyChecks                    *ReadyChecks                    `json:"ReadyChecks,omitempty"`
	ReadyResponse                  *ReadyResponse                  `json:"ReadyResponse,omitempty"`
	SorobanEvent                   *SorobanEvent                   `json:"SorobanEvent,omitempty"`
	TokenMetadataResponse          *TokenMetadataResponse          `json:"TokenMetadataResponse,omitempty"`
	UsageResponse                  *UsageResponse                  `json:"UsageResponse,omitempty"`
	UsageRollupRow                 *UsageRollupRow                 `json:"UsageRollupRow,omitempty"`
	VersionResponse                *VersionResponse                `json:"VersionResponse,omitempty"`
	WebhookCreateRequest           *WebhookCreateRequest           `json:"WebhookCreateRequest,omitempty"`
	WebhookCreateResponse          *WebhookCreateResponse          `json:"WebhookCreateResponse,omitempty"`
	WebhookDelivery                *WebhookDelivery                `json:"WebhookDelivery,omitempty"`
	WebhookReplayResponse          *WebhookReplayResponse          `json:"WebhookReplayResponse,omitempty"`
	WebhookRotateSecretResponse    *WebhookRotateSecretResponse    `json:"WebhookRotateSecretResponse,omitempty"`
	WebhookStatusResponse          *WebhookStatusResponse          `json:"WebhookStatusResponse,omitempty"`
	WebhookSubscription            *WebhookSubscription            `json:"WebhookSubscription,omitempty"`
}

type APIKeyResponse struct {
	CreatedAt                                  time.Time  `json:"created_at"`
	CreatedBy                                  *string    `json:"created_by,omitempty"`
	ID                                         string     `json:"id"`
	// Raw key, returned only at creation time.           
	Key                                        *string    `json:"key,omitempty"`
	KeyPrefix                                  string     `json:"key_prefix"`
	Label                                      string     `json:"label"`
	LastUsedAt                                 time.Time  `json:"last_used_at"`
	Network                                    Network    `json:"network"`
	RateLimitTier                              string     `json:"rate_limit_tier"`
	RequestCount                               int64      `json:"request_count"`
	RevokedAt                                  *time.Time `json:"revoked_at,omitempty"`
}

type AdminKeyUsageResponse struct {
	APIKeyID                                                        string          `json:"api_key_id"`
	// Per-endpoint breakdown; empty when the window has no requests                
	ByEndpoint                                                      []EndpointUsage `json:"by_endpoint"`
	From                                                            time.Time       `json:"from"`
	// Requests with status code < 400                                              
	SuccessfulRequests                                              int64           `json:"successful_requests"`
	To                                                              time.Time       `json:"to"`
	TotalRequests                                                   int64           `json:"total_requests"`
}

type EndpointUsage struct {
	AvgDurationMS float64 `json:"avg_duration_ms"`
	Endpoint      string  `json:"endpoint"`
	Requests      int64   `json:"requests"`
}

type ContractCallRequest struct {
	// Base64-encoded XDR ScVal arguments, in order         
	Args                                           []string `json:"args,omitempty"`
	// Contract function name to invoke                     
	Function                                       string   `json:"function"`
}

type ContractCallResponse struct {
	// Simulation error message; present only when success=false                       
	Error                                                                  *string     `json:"error,omitempty"`
	// Raw base64 XDR of the return value; omitted on failure                          
	RawXdr                                                                 *string     `json:"raw_xdr,omitempty"`
	// Decoded return value; omitted when undecodable or failed                        
	Result                                                                 interface{} `json:"result"`
	// False when the simulation itself reported a failure (still HTTP 200)            
	Success                                                                bool        `json:"success"`
}

type ContractEventFieldSchema struct {
	// Stable field name for this event payload position or property              
	Name                                                                   string `json:"name"`
	// Field type inferred from the contract interface or observed payloads       
	Type                                                                   string `json:"type"`
}

type ContractEventSchema struct {
	// Contract event name (topic_0)                                 
	EventName                             string                     `json:"event_name"`
	// Named fields for this event payload                           
	Fields                                []ContractEventFieldSchema `json:"fields"`
}

type ContractEventSchemaResponse struct {
	// Contract code hash for this schema version                              
	CodeHash                                             string                `json:"code_hash"`
	// Soroban contract address                                                
	ContractID                                           string                `json:"contract_id"`
	// Observed event names and their typed field schemas                      
	Events                                               []ContractEventSchema `json:"events"`
	// Network queried                                                         
	Network                                              Network               `json:"network"`
}

type ContractRegistrationRequest struct {
	// Contract address (C... strkey, 56 characters)             
	ContractID                                           string  `json:"contract_id"`
	// Ledger sequence to start indexing from                    
	IndexFrom                                            *int64  `json:"index_from,omitempty"`
	// Human-readable label                                      
	Label                                                *string `json:"label,omitempty"`
	// Network scope; omitted or empty means all networks        
	Network                                              *string `json:"network,omitempty"`
}

type ContractResponse struct {
	// Stellar contract id (C... strkey).            
	ContractID                             string    `json:"contract_id"`
	CreatedAt                              time.Time `json:"created_at"`
	ID                                     string    `json:"id"`
	// Ledger sequence indexing began from.          
	IndexFrom                              int64     `json:"index_from"`
	Label                                  *string   `json:"label,omitempty"`
	Network                                *string   `json:"network,omitempty"`
}

type ContractSpecFunction struct {
	// Exported function name       
	Name                     string `json:"name"`
}

type ContractSpecResponse struct {
	// Deployed WASM code hash this spec was parsed from                                                       
	CodeHash                                                                            string                 `json:"code_hash"`
	// Soroban contract address                                                                                
	ContractID                                                                          string                 `json:"contract_id"`
	// Primary classification derived from detected interfaces (e.g. token, nft, custom)                       
	ContractType                                                                        string                 `json:"contract_type"`
	// Functions captured from the contract's spec                                                             
	Functions                                                                           []ContractSpecFunction `json:"functions"`
	// Whether an embedded contractspecv0 section was found                                                    
	HasSpec                                                                             bool                   `json:"has_spec"`
	// Every standard interface detected from the contract's spec functions                                    
	Interfaces                                                                          []string               `json:"interfaces"`
	// Network queried                                                                                         
	Network                                                                             Network                `json:"network"`
}

type ContractStats struct {
	AvgCPUInstructions                          float64   `json:"avg_cpu_instructions"`
	AvgFeeCharged                               float64   `json:"avg_fee_charged"`
	AvgReadBytes                                float64   `json:"avg_read_bytes"`
	AvgWriteBytes                               float64   `json:"avg_write_bytes"`
	// Soroban contract address                           
	ContractID                                  string    `json:"contract_id"`
	// Total events for this contract in range            
	EventCount                                  int64     `json:"event_count"`
	InvocationCount                             int64     `json:"invocation_count"`
	// Timestamp of last event for this contract          
	LastSeenAt                                  time.Time `json:"last_seen_at"`
	// Latest ledger sequence for this contract           
	LastSeenLedger                              int64     `json:"last_seen_ledger"`
	TotalFeeCharged                             int64     `json:"total_fee_charged"`
}

type ContractStatsResponse struct {
	// Contracts sorted by event count (descending)                                   
	Contracts                                                         []ContractStats `json:"contracts"`
	// Lower bound of queried ledger range                                            
	FromLedger                                                        int64           `json:"from_ledger"`
	// Timestamp when response was generated                                          
	GeneratedAt                                                       time.Time       `json:"generated_at"`
	// Whether more pages are available                                               
	HasMore                                                           bool            `json:"has_more"`
	// Network queried                                                                
	Network                                                           Network         `json:"network"`
	// Opaque cursor to pass as the cursor parameter for the next page                
	NextCursor                                                        *string         `json:"next_cursor,omitempty"`
	// Upper bound of queried ledger range                                            
	ToLedger                                                          int64           `json:"to_ledger"`
}

type ContractStorageHistoryResponse struct {
	// The contract whose storage history was queried                                        
	ContractID                                                        string                 `json:"contract_id"`
	// Whether more pages are available                                                      
	HasMore                                                           bool                   `json:"has_more"`
	// Network the contract is indexed on                                                    
	Network                                                           string                 `json:"network"`
	// Opaque cursor to pass as the cursor parameter for the next page                       
	NextCursor                                                        *string                `json:"next_cursor,omitempty"`
	// The storage key whose history was queried                                             
	StorageKey                                                        string                 `json:"storage_key"`
	// Storage history entries, oldest first                                                 
	Values                                                            []ContractStorageValue `json:"values"`
}

type ContractStorageValue struct {
	// Human-readable decoded storage key                                          
	Key                                                                interface{} `json:"key"`
	// Ledger sequence at which this value was observed                            
	LedgerSequence                                                     int64       `json:"ledger_sequence"`
	// Timestamp this snapshot row was recorded                                    
	ObservedAt                                                         time.Time   `json:"observed_at"`
	// Base64-encoded XDR LedgerKey this value was read from                       
	StorageKey                                                         string      `json:"storage_key"`
	// Human-readable decoded value (absent when the entry was removed)            
	Value                                                              interface{} `json:"value"`
}

type ContractStorageResponse struct {
	// Soroban contract address                                                                                  
	ContractID                                                                            string                 `json:"contract_id"`
	// Network queried                                                                                           
	Network                                                                               Network                `json:"network"`
	// Storage snapshot values (latest, or full history when queried via /storage/history)                       
	Values                                                                                []ContractStorageValue `json:"values"`
}

type ErrorResponse struct {
	Error Error `json:"error"`
}

type Error struct {
	// Machine-readable error code. Matches httputil.ErrorCode exactly        
	// (services/api/internal/httputil/errors.go).                            
	Code                                                              Code    `json:"code"`
	// Human-readable error message                                           
	Message                                                           string  `json:"message"`
	// Request ID for debugging                                               
	RequestID                                                         *string `json:"request_id,omitempty"`
}

type EventListResponse struct {
	// List of events                                                        
	Events                                                    []SorobanEvent `json:"events"`
	// Whether more results are available                                    
	HasMore                                                   bool           `json:"has_more"`
	// Opaque cursor for next page (null if has_more is false)               
	NextCursor                                                string         `json:"next_cursor"`
}

type SorobanEvent struct {
	// Soroban contract address                     
	ContractID                            string    `json:"contract_id"`
	// Timestamp when event was indexed             
	CreatedAt                             time.Time `json:"created_at"`
	// Event data (XDR-encoded)                     
	Data                                  string    `json:"data"`
	// Event index within transaction               
	EventIndex                            int64     `json:"event_index"`
	// Type of event                                
	EventType                             EventType `json:"event_type"`
	// Unique event identifier                      
	ID                                    string    `json:"id"`
	// Ledger sequence number                       
	LedgerSequence                        int64     `json:"ledger_sequence"`
	// Ledger timestamp in ISO 8601 format          
	LedgerTimestamp                       time.Time `json:"ledger_timestamp"`
	// Event topics (XDR-encoded)                   
	Topics                                []string  `json:"topics"`
	// Transaction hash (XDR-encoded)               
	TransactionHash                       string    `json:"transaction_hash"`
}

type IndexerStatsResponse struct {
	// Average poll duration in milliseconds                                                                             
	AvgPollDurationMS                                                                         int64                      `json:"avg_poll_duration_ms"`
	// Current chain tip ledger (from RPC)                                                                               
	ChainTipLedger                                                                            int64                      `json:"chain_tip_ledger"`
	// Cumulative events indexed                                                                                         
	EventsIndexedTotal                                                                        int64                      `json:"events_indexed_total"`
	// Events processed in last poll                                                                                     
	EventsLastPoll                                                                            int64                      `json:"events_last_poll"`
	// Number of ledgers behind chain tip                                                                                
	LagLedgers                                                                                int64                      `json:"lag_ledgers"`
	// Estimated wall-clock staleness in seconds: lag_ledgers times Stellar's protocol-target                            
	// ledger close time (~5s). Null whenever lag_ledgers is null. See                                                   
	// docs/observability/data-freshness.md for the full freshness contract this field is part                           
	// of.                                                                                                               
	LagSecondsEstimated                                                                       float64                    `json:"lag_seconds_estimated"`
	// Latest indexed ledger sequence                                                                                    
	LastLedgerIndexed                                                                         int64                      `json:"last_ledger_indexed"`
	// Timestamp of last successful poll                                                                                 
	LastPollAt                                                                                time.Time                  `json:"last_poll_at"`
	// Network name from NETWORK environment variable                                                                    
	Network                                                                                   string                     `json:"network"`
	// Indexer health status                                                                                             
	Status                                                                                    IndexerStatsResponseStatus `json:"status"`
}

type ListAPIKeysResponse struct {
	APIKeys                                                        []APIKeyResponse `json:"api_keys"`
	// Whether another page is available.                                           
	HasMore                                                        bool             `json:"has_more"`
	// Opaque cursor for the next page (null if has_more is false).                 
	NextCursor                                                     string           `json:"next_cursor"`
}

type ListContractsResponse struct {
	Contracts                                                      []ContractResponse `json:"contracts"`
	// Whether another page is available.                                             
	HasMore                                                        bool               `json:"has_more"`
	// Opaque cursor for the next page (null if has_more is false).                   
	NextCursor                                                     string             `json:"next_cursor"`
}

type ListWebhooksResponse struct {
	// Whether another page is available.                                                
	HasMore                                                        bool                  `json:"has_more"`
	// Opaque cursor for the next page (null if has_more is false).                      
	NextCursor                                                     string                `json:"next_cursor"`
	Webhooks                                                       []WebhookSubscription `json:"webhooks"`
}

type WebhookSubscription struct {
	// Omitted when empty                                               
	APIKeyID                                                 *string    `json:"apiKeyId,omitempty"`
	ContractID                                               string     `json:"contractId"`
	CreatedAt                                                time.Time  `json:"createdAt"`
	ID                                                       string     `json:"id"`
	Network                                                  string     `json:"network"`
	// Present while deliveries are paused                              
	PausedAt                                                 *time.Time `json:"pausedAt,omitempty"`
	// HMAC signing secret for deliveries; omitted when empty           
	Secret                                                   *string    `json:"secret,omitempty"`
	TargetURL                                                string     `json:"targetUrl"`
	// Topic filter; omitted when unfiltered                            
	Topic0                                                   *string    `json:"topic0,omitempty"`
}

type LivenessResponse struct {
	// Always "ok" while the process is up — no dependency checks.                       
	Status                                                        LivenessResponseStatus `json:"status"`
}

type ReadyChecks struct {
	// "ok" or "error: <message>"       
	GrpcAPI                      string `json:"grpc_api"`
	// "ok" or "error: <message>"       
	Postgres                     string `json:"postgres"`
	// "ok" or "error: <message>"       
	Redis                        string `json:"redis"`
}

type ReadyResponse struct {
	Checks                                                                                  ReadyChecks         `json:"checks"`
	// Ledgers behind chain tip, from system_state. Null when Postgres is unreachable or the                    
	// chain-tip cache hasn't been populated yet.                                                               
	IndexerLag                                                                              int64               `json:"indexer_lag"`
	// "degraded" when any dependency check in `checks` failed.                                                 
	Status                                                                                  ReadyResponseStatus `json:"status"`
}

type TokenMetadataResponse struct {
	// Soroban contract address                                                                          
	ContractID                                                                                string     `json:"contract_id"`
	// Token decimals, from decimals(). Null unless is_token is true.                                    
	Decimals                                                                                  *int64     `json:"decimals,omitempty"`
	// True when the contract was resolved and implements the SEP-41 read interface. False for           
	// both "not yet resolved" and "resolved, not a token".                                              
	IsToken                                                                                   bool       `json:"is_token"`
	// Token name, from name(). Null unless is_token is true.                                            
	Name                                                                                      *string    `json:"name,omitempty"`
	// Network queried                                                                                   
	Network                                                                                   Network    `json:"network"`
	// When this contract was last resolved. Null if never resolved.                                     
	ResolvedAt                                                                                *time.Time `json:"resolved_at,omitempty"`
	// Token symbol, from symbol(). Null unless is_token is true.                                        
	Symbol                                                                                    *string    `json:"symbol,omitempty"`
}

type UsageResponse struct {
	APIKeyID                                                                                    string           `json:"api_key_id"`
	// Daily buckets from the maintained usage_rollup table, oldest first; empty when the window                 
	// has no rollup rows.                                                                                       
	Days                                                                                        []UsageRollupRow `json:"days"`
	From                                                                                        time.Time        `json:"from"`
	To                                                                                          time.Time        `json:"to"`
	TotalErrors                                                                                 int64            `json:"total_errors"`
	TotalRequests                                                                               int64            `json:"total_requests"`
}

type UsageRollupRow struct {
	AvgDurationMS float64   `json:"avg_duration_ms"`
	ErrorCount    int64     `json:"error_count"`
	PeriodEnd     time.Time `json:"period_end"`
	PeriodStart   time.Time `json:"period_start"`
	RequestCount  int64     `json:"request_count"`
}

type VersionResponse struct {
	// RFC 3339 build time, or "unknown" when not injected at build time. Not typed as date-time       
	// because of that sentinel.                                                                       
	BuildTimestamp                                                                              string `json:"build_timestamp"`
	// Full git commit SHA the binary was built from, or "unknown" when not injected at build          
	// time.                                                                                           
	CommitSHA                                                                                   string `json:"commit_sha"`
	// Highest applied migration version from _sqlx_migrations, as a string. Null when no              
	// migrations have been applied yet or when Postgres is unreachable — the endpoint still           
	// returns 200 in that case so build metadata stays available during an outage.                    
	SchemaVersion                                                                               string `json:"schema_version"`
	// Semantic version tag of the running build, or "dev" for a binary built without release          
	// ldflags.                                                                                        
	Version                                                                                     string `json:"version"`
}

type WebhookCreateRequest struct {
	ContractID                                                                    string  `json:"contractId"`
	Network                                                                       *string `json:"network,omitempty"`
	// Delivery target; must be https with a publicly resolvable, non-private host        
	TargetURL                                                                     string  `json:"targetUrl"`
	// Optional topic filter                                                              
	Topic0                                                                        *string `json:"topic0,omitempty"`
}

type WebhookCreateResponse struct {
	ContractID                                            string `json:"contractId"`
	ID                                                    string `json:"id"`
	Network                                               string `json:"network"`
	// HMAC signing secret — shown here and in the listing       
	Secret                                                string `json:"secret"`
	TargetURL                                             string `json:"targetUrl"`
}

type WebhookDelivery struct {
	Attempt                                                           int64     `json:"attempt"`
	Attempts                                                          int64     `json:"attempts"`
	DeliveredAt                                                       time.Time `json:"deliveredAt"`
	EventID                                                           string    `json:"eventId"`
	ID                                                                int64     `json:"id"`
	// Omitted when empty                                                       
	ResponseBody                                                      *string   `json:"responseBody,omitempty"`
	Status                                                            string    `json:"status"`
	// HTTP status of the delivery attempt; omitted when none occurred          
	StatusCode                                                        *int64    `json:"statusCode,omitempty"`
	SubscriptionID                                                    string    `json:"subscriptionId"`
	Success                                                           bool      `json:"success"`
}

type WebhookReplayResponse struct {
	Attempt                            int64                       `json:"attempt"`
	// Truncated to 500 characters                                 
	ResponseBody                       string                      `json:"response_body"`
	Status                             WebhookReplayResponseStatus `json:"status"`
	// 0 when no HTTP response occurred                            
	StatusCode                         int64                       `json:"status_code"`
	Success                            bool                        `json:"success"`
}

type WebhookRotateSecretResponse struct {
	ID                                                                       string `json:"id"`
	// The demoted secret, now serving as secondary during the overlap window       
	PreviousSecret                                                           string `json:"previousSecret"`
	// The new primary signing secret (whsec_ prefixed)                             
	Secret                                                                   string `json:"secret"`
}

type WebhookStatusResponse struct {
	Status WebhookStatusResponseStatus `json:"status"`
}

// Network queried
type Network string

const (
	Mainnet Network = "mainnet"
	Testnet Network = "testnet"
)

// Machine-readable error code. Matches httputil.ErrorCode exactly
// (services/api/internal/httputil/errors.go).
type Code string

const (
	Conflict        Code = "CONFLICT"
	Forbidden       Code = "FORBIDDEN"
	Internal        Code = "INTERNAL"
	InvalidArgument Code = "INVALID_ARGUMENT"
	NotFound        Code = "NOT_FOUND"
	PayloadTooLarge Code = "PAYLOAD_TOO_LARGE"
	RateLimited     Code = "RATE_LIMITED"
	Unauthorized    Code = "UNAUTHORIZED"
	Unavailable     Code = "UNAVAILABLE"
)

// Type of event
type EventType string

const (
	Contract   EventType = "contract"
	Diagnostic EventType = "diagnostic"
	System     EventType = "system"
)

// Indexer health status
type IndexerStatsResponseStatus string

const (
	Healthy IndexerStatsResponseStatus = "healthy"
	Lagging IndexerStatsResponseStatus = "lagging"
	Stalled IndexerStatsResponseStatus = "stalled"
)

// Always "ok" while the process is up — no dependency checks.
type LivenessResponseStatus string

const (
	PurpleOk LivenessResponseStatus = "ok"
)

// "degraded" when any dependency check in `checks` failed.
type ReadyResponseStatus string

const (
	Degraded ReadyResponseStatus = "degraded"
	FluffyOk ReadyResponseStatus = "ok"
)

type WebhookReplayResponseStatus string

const (
	Failed  WebhookReplayResponseStatus = "failed"
	Success WebhookReplayResponseStatus = "success"
)

type WebhookStatusResponseStatus string

const (
	Paused  WebhookStatusResponseStatus = "paused"
	Resumed WebhookStatusResponseStatus = "resumed"
)
