/**
 * Error and empty state definitions for the explorer.
 * Provides honest, actionable messages for every failure and empty path.
 */

export enum ErrorType {
  // Data errors
  NOT_FOUND = 'not_found',
  NO_DATA = 'no_data',
  EMPTY_RESULT = 'empty_result',

  // API errors
  API_ERROR = 'api_error',
  TIMEOUT = 'timeout',
  RATE_LIMITED = 'rate_limited',
  INDEXER_BEHIND = 'indexer_behind',

  // Unknown
  UNKNOWN = 'unknown',
}

export interface ErrorState {
  type: ErrorType;
  title: string;
  message: string;
  actionText?: string;
  actionUrl?: string;
  details?: string;
  retryable: boolean;
}

export function getErrorState(type: ErrorType, context?: Record<string, any>): ErrorState {
  const ctx = context || {};

  switch (type) {
    case ErrorType.NOT_FOUND:
      return {
        type,
        title: 'Contract not found',
        message: 'No contract exists at this address on the selected network.',
        details: `Address: ${ctx.address || 'unknown'}`,
        actionText: 'Try another address',
        actionUrl: '/',
        retryable: false,
      };

    case ErrorType.NO_DATA:
      return {
        type,
        title: 'No events yet',
        message:
          'This contract has not emitted any events yet, or events are still being indexed.',
        details:
          'Indexing typically completes within a few minutes. Check back shortly.',
        actionText: 'Refresh page',
        retryable: true,
      };

    case ErrorType.EMPTY_RESULT:
      return {
        type,
        title: 'No matching events',
        message: `No events match your filter criteria${ctx.filter ? ` (${ctx.filter})` : ''}.`,
        details: 'Try clearing filters or using a different search.',
        actionText: 'Clear filters',
        actionUrl: ctx.clearUrl || '/',
        retryable: false,
      };

    case ErrorType.API_ERROR:
      return {
        type,
        title: 'Failed to load events',
        message: 'The API returned an error. This may be temporary.',
        details: ctx.statusCode ? `HTTP ${ctx.statusCode}` : 'Unknown error',
        actionText: 'Try again',
        retryable: true,
      };

    case ErrorType.TIMEOUT:
      return {
        type,
        title: 'Request timed out',
        message:
          'The API took too long to respond. This may indicate backend degradation or high load.',
        details: `Timeout after ${ctx.timeout || 30}s`,
        actionText: 'Retry',
        retryable: true,
      };

    case ErrorType.RATE_LIMITED:
      return {
        type,
        title: 'Rate limit reached',
        message:
          'Too many requests to the API. Please wait a moment before trying again.',
        details:
          'Free tier: 60 req/min. Get an API key for higher limits.',
        actionText: 'Get an API key',
        actionUrl: 'https://app.trident.dev/signup',
        retryable: false,
      };

    case ErrorType.INDEXER_BEHIND:
      return {
        type,
        title: 'Indexer is catching up',
        message:
          'The indexer is currently behind. Results may be incomplete or delayed.',
        details: `Last indexed: ${ctx.lastIndexedLedger || 'unknown'}. Current ledger: ${ctx.currentLedger || 'unknown'}.`,
        actionText: 'Refresh in a moment',
        retryable: true,
      };

    case ErrorType.UNKNOWN:
    default:
      return {
        type: ErrorType.UNKNOWN,
        title: 'Something went wrong',
        message: 'An unexpected error occurred.',
        details: ctx.error?.message || 'Please try again or contact support.',
        actionText: 'Go home',
        actionUrl: '/',
        retryable: true,
      };
  }
}

export function classifyError(error: Error | string, statusCode?: number): ErrorType {
  const msg = typeof error === 'string' ? error.toLowerCase() : error.message.toLowerCase();

  if (msg.includes('not found') || statusCode === 404) return ErrorType.NOT_FOUND;
  if (msg.includes('timeout') || msg.includes('signal') || statusCode === 504)
    return ErrorType.TIMEOUT;
  if (msg.includes('rate limit') || statusCode === 429) return ErrorType.RATE_LIMITED;
  if (statusCode && statusCode >= 500) return ErrorType.API_ERROR;
  if (statusCode && statusCode >= 400) return ErrorType.API_ERROR;

  return ErrorType.UNKNOWN;
}
