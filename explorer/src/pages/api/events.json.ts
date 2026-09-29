import type { APIRoute } from "astro";
import { ApiError, listEvents } from "../../lib/api";
import { isValidContractId } from "../../lib/contracts";
import { probeContractOnChain } from "../../lib/soroban";
import type { ExplorerEventsResponse, Network, UnreachableReason } from "../../lib/types";

const jsonHeaders = {
  "Content-Type": "application/json",
  "Cache-Control": "public, max-age=5, s-maxage=5",
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: jsonHeaders });
}

export const GET: APIRoute = async ({ url }) => {
  const rawNetwork = url.searchParams.get("network");
  const network: Network = rawNetwork === "mainnet" ? "mainnet" : "testnet";
  const contractId = url.searchParams.get("contractId") ?? undefined;
  const topic0 = url.searchParams.get("topic0") ?? undefined;
  const cursor = url.searchParams.get("cursor") ?? undefined;
  const rawFrom = url.searchParams.get("ledgerFrom");
  const rawTo = url.searchParams.get("ledgerTo");
  const filtered = Boolean(topic0 || rawFrom || rawTo);

  // Validate locally (format + strkey checksum) before spending a request on
  // the upstream API, so a typo'd address gets an instant, honest answer.
  if (contractId && !isValidContractId(contractId)) {
    return json({
      status: "invalid_contract",
      events: [],
      has_more: false,
      next_cursor: null,
      message: "That address is not a valid Stellar contract id.",
    } satisfies ExplorerEventsResponse, 400);
  }

  try {
    const result = await listEvents({
      network,
      contractId,
      topic0,
      cursor,
      ledgerFrom: rawFrom ? Number(rawFrom) : undefined,
      ledgerTo: rawTo ? Number(rawTo) : undefined,
      limit: 25,
    });

    const base: ExplorerEventsResponse = { ...result, status: "ok" };

    // Happy path and the tail of a paginated list.
    if (result.events.length > 0 || cursor) return json(base);

    // Empty result for the contract itself: decide between "no events yet"
    // (quiet but known) and "not indexed" (emitting on-chain but Trident has
    // nothing). Only probe when the visitor is browsing the contract without
    // filters, where the distinction actually matters.
    if (contractId && !filtered) {
      const probe = await probeContractOnChain(network, contractId);
      if (probe.status === "has_events") {
        return json({
          ...base,
          status: "not_indexed",
          message:
            "This contract is emitting events on the Stellar network, but Trident has not indexed any of them yet.",
        } satisfies ExplorerEventsResponse);
      }
      if (probe.status === "invalid_contract") {
        return json({
          status: "invalid_contract",
          events: [],
          has_more: false,
          next_cursor: null,
          message: "That address is not a valid Stellar contract id.",
        } satisfies ExplorerEventsResponse, 400);
      }
    }

    return json({
      ...base,
      status: "no_events",
      filtered,
      message: filtered
        ? "No events match the active filters for this contract."
        : "Trident has not recorded any events for this contract yet.",
    } satisfies ExplorerEventsResponse);
  } catch (err) {
    if (!(err instanceof ApiError)) {
      return json({
        status: "api_unreachable",
        reason: "network",
        events: [],
        has_more: false,
        next_cursor: null,
        message: "Could not reach the Trident indexer. Please retry.",
      } satisfies ExplorerEventsResponse, 502);
    }

    const { status, code } = err;
    if (status === 400) {
      return json({
        status: "invalid_contract",
        events: [],
        has_more: false,
        next_cursor: null,
        message: "That address is not a valid Stellar contract id.",
      } satisfies ExplorerEventsResponse, 400);
    }
    if (status === 404) {
      return json({
        status: "not_found",
        events: [],
        has_more: false,
        next_cursor: null,
        message: "We couldn't find anything at that address.",
      } satisfies ExplorerEventsResponse, 404);
    }

    let reason: UnreachableReason = "down";
    if (status === 429 || code === "RATE_LIMITED") reason = "rate_limited";
    else if (status === 401 || status === 403 || code === "UNAUTHORIZED")
      reason = "unauthorized";
    else if (status === 0 || code === "NETWORK") reason = "network";

    const messages: Record<UnreachableReason, string> = {
      rate_limited:
        "You are browsing faster than the explorer is allowed to — rate limiting kicked in. It will recover on its own in a moment.",
      unauthorized:
        "The explorer's server key is not configured. This is on us, not you.",
      network:
        "Could not reach the Trident indexer. Please check your connection and retry.",
      timeout: "The Trident indexer took too long to answer. Please retry.",
      down: "The Trident indexer is temporarily unavailable. Please try again shortly.",
    };

    const httpStatus =
      reason === "rate_limited" ? 429 : reason === "unauthorized" ? status : 502;

    return json(
      {
        status: "api_unreachable",
        reason,
        events: [],
        has_more: false,
        next_cursor: null,
        message: messages[reason],
      } satisfies ExplorerEventsResponse,
      httpStatus,
    );
  }
};
