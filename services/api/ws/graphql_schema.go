package ws

// GraphQLSchema is the SDL for the operations this transport supports
// (issue #223). It is the published contract for the GraphQL surface, and it
// is snapshot-tested against the operations the resolver actually dispatches,
// so the two cannot drift apart silently.
//
// The transport does not execute arbitrary documents against this schema —
// "arbitrary ad-hoc GraphQL over the whole schema" is explicitly out of scope
// for #223 — so this describes what is supported rather than what a general
// executor would accept. Field names here are the camelCase names the
// resolvers emit and the subscription payloads already used.
const GraphQLSchema = `"""
A Soroban contract event. Field names match the REST /v1/events payload with
snake_case remapped to camelCase.
"""
type Event {
  id: ID!
  contractId: String!
  ledgerSequence: Int!
  ledgerTimestamp: String!
  txHash: String!
  eventIndex: Int!
  eventType: String!
  topics: [String!]!
  data: String
  createdAt: String
}

"""
One page of events. Mirrors the REST envelope: nextCursor is null — never
absent — on the last page, so an auto-pager stops on one rule across both
transports.
"""
type EventPage {
  events: [Event!]!
  hasMore: Boolean!
  nextCursor: String
}

"""Per-contract event statistics, mirroring GET /v1/stats/contracts."""
type ContractStat {
  contractId: String!
  eventCount: Int!
  firstLedger: Int
  lastLedger: Int
  firstSeen: String
  lastSeen: String
}

type Query {
  """
  List events. Accepts the same filters and opaque cursor pagination as
  GET /v1/events. The network is taken from the authenticated API key, not
  from arguments.
  """
  events(
    contractId: String
    topic0: String
    topic1: String
    eventType: String
    ledgerFrom: Int
    ledgerTo: Int
    cursor: String
    limit: Int = 50
  ): EventPage!

  """Fetch one event by id. Null when no such event exists."""
  event(id: ID!): Event

  """Per-contract statistics, mirroring GET /v1/stats/contracts."""
  contractStats(
    fromLedger: Int
    toLedger: Int
    limit: Int = 50
  ): [ContractStat!]!
}

type Subscription {
  """
  Live events for one contract, optionally filtered to a single topic0.
  """
  contractEvents(contractId: String!, topic0: String): Event!
}
`

// gqlSupportedQueries is the set of root query fields gqlResolveQuery
// dispatches. Kept next to the schema so the snapshot test can assert the
// two agree — a resolver added without a schema entry, or a schema field with
// no resolver, is a drift the test fails on rather than something a client
// discovers at runtime.
var gqlSupportedQueries = []string{"events", "event", "contractStats"}

// gqlSupportedSubscriptions is the same for subscription root fields.
var gqlSupportedSubscriptions = []string{"contractEvents"}
