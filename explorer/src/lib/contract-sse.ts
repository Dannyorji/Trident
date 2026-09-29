import type { Network, StreamedEvent } from './types';
import { truncate } from './format';

/* ------------------------------------------------------------------ *
 * Contract page live feed (SSE).
 *
 * Works on top of the server-rendered page. Manages the stream status
 * pill ("connecting / live / reconnecting / off"), auto-reconnects with
 * Last-Event-ID so nothing is skipped, and prepends new events to the
 * rendered table.
 * ------------------------------------------------------------------ */

const params = new URLSearchParams(window.location.search);
const network: Network = params.get('network') === 'mainnet' ? 'mainnet' : 'testnet';
const contractId = decodeURIComponent(window.location.pathname.split('/')[2] ?? '');
const topic0 = params.get('topic0') ?? '';

const MAX_RECONNECT_ATTEMPTS = 10;

function esc(v: unknown): string {
  return String(v ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function parseTopics(raw: string): string[] {
  try {
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed)) return parsed.map((t) => String(t));
  } catch {
    /* ignore */
  }
  return [];
}

let source: EventSource | null = null;
let reconnectAttempts = 0;
let caughtUpUntil = 0;
let wasLive = false;

function streamUrl(): string {
  const p = new URLSearchParams({ network, contractId });
  if (topic0) p.set('topic0', topic0);
  return `/api/events/stream?${p.toString()}`;
}

function setLabel(status: 'connecting' | 'open' | 'reconnecting' | 'off'): void {
  const pill = document.getElementById('stream-status');
  if (!pill) return;
  let dot: string;
  let label: string;
  switch (status) {
    case 'connecting':
      dot = 'bg-amber-400 animate-pulse';
      label = 'Connecting to live feed';
      break;
    case 'open':
      dot = 'bg-green-500';
      label = 'Live';
      break;
    case 'reconnecting':
      dot = 'bg-amber-400 animate-pulse';
      label =
        reconnectAttempts > 1
          ? `Reconnecting… (attempt ${reconnectAttempts})`
          : 'Reconnecting…';
      break;
    case 'off':
      dot = 'bg-red-500';
      label = 'Live feed unavailable';
      break;
  }
  const color =
    status === 'open' ? 'text-green-300' : status === 'off' ? 'text-red-300' : 'text-amber-300';
  const action =
    status === 'off'
      ? '<button type="button" id="stream-reconnect" class="ml-2 px-3 py-1 rounded bg-gray-800 hover:bg-gray-700 text-xs text-gray-300 hover:text-white transition-colors">Reconnect</button>'
      : '';
  pill.innerHTML = `
    <span class="inline-flex items-center gap-2 text-xs font-medium ${color}">
      <span class="inline-block w-2 h-2 rounded-full ${dot}"></span>
      ${label}
    </span>
    ${action}`;
  pill.setAttribute(
    'aria-label',
    status === 'open'
      ? 'Live feed connected'
      : status === 'off'
        ? 'Live feed unavailable'
        : label,
  );
  pill.title = label;
}

function showNotice(message: string): void {
  const zone = document.getElementById('stream-notice');
  if (!zone) return;
  zone.innerHTML = `
    <div class="rounded-lg border border-amber-800/60 bg-amber-900/20 px-4 py-3 text-sm text-amber-200 flex items-start gap-3" role="status">
      <span class="inline-block w-2 h-2 rounded-full bg-amber-400 animate-pulse mt-1.5 shrink-0" aria-hidden="true"></span>
      <span>${esc(message)}</span>
    </div>`;
}

function clearNotice(): void {
  const zone = document.getElementById('stream-notice');
  if (zone) zone.innerHTML = '';
}

function liveRowHtml(e: StreamedEvent): string {
  const topics = parseTopics(e.topics);
  const id = e.event_id ?? '';
  const href = `/contract/${encodeURIComponent(e.contract_id || contractId)}/event/${encodeURIComponent(id)}?network=${network}`;
  return `
  <tr data-href="${esc(href)}"
      class="hover:bg-gray-900/60 cursor-pointer transition-colors">
    <td class="px-4 py-3 text-gray-400 whitespace-nowrap text-xs" title="${esc(e.ledger_timestamp)}">just now</td>
    <td class="px-4 py-3 font-mono text-gray-300 text-xs">${esc(e.ledger_sequence)}</td>
    <td class="px-4 py-3"><span class="px-2 py-0.5 rounded-full text-xs bg-indigo-900/50 text-indigo-300 font-medium">${esc(topics[0] ?? e.event_type)}</span></td>
    <td class="px-4 py-3 font-mono text-gray-400 text-xs truncate max-w-[180px] hidden md:table-cell">${topics[1] ? esc(truncate(topics[1], 12, 8)) : '—'}</td>
    <td class="px-4 py-3 hidden lg:table-cell font-mono text-xs text-gray-400">${e.transaction_hash ? esc(truncate(e.transaction_hash, 8, 6)) : '—'}</td>
    <td class="px-4 py-3 hidden xl:table-cell font-mono text-xs text-gray-500 max-w-[200px] truncate" title="${esc(e.data)}">${e.data ? esc(String(e.data).slice(0, 60)) : '—'}</td>
  </tr>`.trim();
}

function startStream(): void {
  stopStream();
  source = new EventSource(streamUrl());
  setLabel('connecting');

  source.addEventListener('open', () => {
    const resumed = wasLive;
    reconnectAttempts = 0;
    wasLive = true;
    setLabel('open');
    clearNotice();
    caughtUpUntil = resumed ? Date.now() + 3000 : 0;
  });

  source.addEventListener('message', (ev: MessageEvent<string>) => {
    let raw: StreamedEvent;
    try {
      raw = JSON.parse(ev.data as string) as StreamedEvent;
    } catch {
      return;
    }
    if (!raw.contract_id || !raw.event_id) return;

    const tbody = document.getElementById('events-tbody');
    if (!tbody) return;

    const seen = Array.from(tbody.querySelectorAll('tr[data-href]')).some((r) =>
      (r as HTMLElement).dataset.href?.includes(encodeURIComponent(raw.event_id ?? '')),
    );
    if (seen) return;

    tbody.insertAdjacentHTML('afterbegin', liveRowHtml(raw));
    const rows = tbody.querySelectorAll('tr[data-href]');
    while (rows.length > 250) rows[rows.length - 1].remove();

    if (Date.now() <= caughtUpUntil) {
      showNotice(
        'Live feed restored — showing the latest events, including anything that arrived while you were disconnected.',
      );
    }
  });

  source.addEventListener('gap', () => {
    showNotice(
      'The live feed could not resume from exactly where it stopped, so refresh to make sure nothing is missing.',
    );
  });

  source.onerror = () => {
    if (source?.readyState === EventSource.CLOSED) return;
    reconnectAttempts += 1;
    setLabel('reconnecting');
    if (reconnectAttempts >= MAX_RECONNECT_ATTEMPTS) {
      stopStream();
      setLabel('off');
    }
  };
}

function stopStream(): void {
  if (source) {
    source.onerror = null;
    source.close();
    source = null;
  }
}

document.addEventListener('click', (e) => {
  const target = e.target as HTMLElement;
  if (target.closest('#stream-reconnect')) {
    startStream();
    return;
  }
  const row = target.closest<HTMLTableRowElement>('tr[data-href]');
  if (row?.dataset.href && !target.closest('a')) {
    window.location.href = row.dataset.href;
  }
});

startStream();
