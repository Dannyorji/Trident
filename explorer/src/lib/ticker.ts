import { relativeTime, truncate } from './format';
import type { Network } from './types';

/* ------------------------------------------------------------------ *
 * Homepage "Recent Events" ticker.
 *
 * Client-rendered so the page shell paints instantly and the ticker can
 * show a deliberate loading skeleton, an honest empty state, and a clear
 * "indexer unavailable" panel instead of silently going blank.
 * ------------------------------------------------------------------ */

const params = new URLSearchParams(window.location.search);
const network: Network = params.get('network') === 'mainnet' ? 'mainnet' : 'testnet';

const ul = document.getElementById('event-ticker') as HTMLUListElement | null;
const dot = document.getElementById('ticker-dot') as HTMLSpanElement | null;

interface TickerEvent {
  contract_id: string;
  topics: string[];
  event_type: string;
  ledger_sequence: number;
  ledger_timestamp: string;
  id: string;
}

interface TickerResponse {
  status: 'ok' | 'api_unreachable';
  events: TickerEvent[];
  reason?: string;
  message?: string;
}

function setDot(status: 'live' | 'loading' | 'unavailable'): void {
  if (!dot) return;
  dot.className = 'inline-block w-1.5 h-1.5 rounded-full ml-2 align-middle';
  if (status === 'live') {
    dot.classList.add('bg-green-500');
    dot.title = 'Live';
  } else if (status === 'loading') {
    dot.classList.add('bg-amber-400', 'animate-pulse');
    dot.title = 'Loading…';
  } else {
    dot.classList.add('bg-red-500');
    dot.title = 'Live feed unavailable';
  }
}

function esc(v: string): string {
  return v
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function rowHtml(e: TickerEvent): string {
  return `
    <li class="flex items-center gap-4 bg-gray-900 rounded-lg px-4 py-2.5 text-sm hover:bg-gray-800 transition-colors">
      <a href="/contract/${encodeURIComponent(e.contract_id)}?network=${network}"
         class="font-mono text-indigo-300 hover:text-indigo-200 truncate shrink-0 w-36 focus:outline-none focus:ring-2 focus:ring-indigo-500 rounded underline underline-offset-2"
         title="${esc(e.contract_id)}">${esc(truncate(e.contract_id))}</a>
      <span class="text-gray-300 truncate flex-1">${esc(e.topics[0] ?? e.event_type)}</span>
      <span class="text-gray-500 text-xs shrink-0 hidden sm:block" title="${esc(e.ledger_timestamp)}">${esc(relativeTime(e.ledger_timestamp))}</span>
      <a href="/contract/${encodeURIComponent(e.contract_id)}/event/${encodeURIComponent(e.id)}?network=${network}"
         class="text-xs text-gray-500 hover:text-white shrink-0 focus:outline-none focus:ring-2 focus:ring-indigo-500 rounded"
         aria-label="View event ${esc(e.id)} at ledger ${e.ledger_sequence}">#${e.ledger_sequence}</a>
    </li>`.trim();
}

function skeletonHtml(): string {
  return Array.from(
    { length: 5 },
    () => `
    <li class="flex items-center gap-4 bg-gray-900 rounded-lg px-4 py-3">
      <div class="h-3 w-24 rounded bg-gray-800 animate-pulse"></div>
      <div class="h-3 flex-1 rounded bg-gray-800 animate-pulse"></div>
      <div class="h-3 w-14 rounded bg-gray-800 animate-pulse"></div>
    </li>`,
  ).join('');
}

function unavailableHtml(message: string): string {
  return `
    <li class="rounded-lg border border-gray-800 bg-gray-900/60 px-6 py-8 text-center">
      <div class="text-red-300 mx-auto w-8 h-8" aria-hidden="true">
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none"
             stroke="currentColor" stroke-width="1.5">
          <path stroke-linecap="round" stroke-linejoin="round" d="M3 7v6a4 4 0 014 4h10a4 4 0 014-4V7a4 4 0 00-4-4H7a4 4 0 00-4 4z" />
          <path stroke-linecap="round" stroke-linejoin="round" d="M12 11v4m0 0h.01" />
        </svg>
      </div>
      <p class="text-sm text-gray-300 mt-3 max-w-md mx-auto leading-relaxed">${esc(message)}</p>
      <button type="button" id="ticker-retry"
        class="mt-4 px-4 py-2 rounded-lg bg-gray-800 hover:bg-gray-700 text-gray-300 hover:text-white text-sm font-medium transition-colors">
        Retry
      </button>
    </li>`.trim();
}

function emptyHtml(): string {
  return `
    <li class="text-gray-400 text-sm py-4 text-center">
      No recent events on the Stellar ${network} network right now. Check back shortly.
    </li>`.trim();
}

let busy = false;

async function pollTicker(showSkeleton = false): Promise<void> {
  if (busy || !ul) return;
  busy = true;
  if (showSkeleton) {
    ul.innerHTML = skeletonHtml();
    setDot('loading');
  }
  try {
    const res = await fetch(`/api/recent-events.json?network=${network}`);
    const data = (await res.json()) as TickerResponse;
    if (!res.ok) data.status = 'api_unreachable';
    if (data.status === 'ok') {
      setDot('live');
      if (data.events.length === 0) {
        ul.innerHTML = emptyHtml();
      } else {
        ul.innerHTML = data.events.map(rowHtml).join('');
      }
    } else {
      setDot('unavailable');
      ul.innerHTML = unavailableHtml(data.message ?? 'The recent-events feed is unavailable right now.');
    }
  } catch {
    setDot('unavailable');
    ul.innerHTML = unavailableHtml('Could not load recent events. Check your connection.');
  } finally {
    busy = false;
  }
}

function bindTickerRetry(): void {
  ul?.addEventListener('click', (e) => {
    const target = e.target as HTMLElement;
    if (target.closest('#ticker-retry')) void pollTicker(true);
  });
}

bindTickerRetry();
void pollTicker(true);

const id = window.setInterval(() => void pollTicker(), 10000);
document.addEventListener('visibilitychange', () => {
  if (document.hidden) window.clearInterval(id);
});