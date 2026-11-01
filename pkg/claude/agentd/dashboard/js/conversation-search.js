// conversation-search.js — the Groups tab's content search panel.
//
// Each settled query is one request to /api/conversations/search. Nothing here
// rides the poll: the panel owns its results, a newer query aborts the older
// request, which also stops the daemon's scan, and closing the panel aborts
// whatever is in flight.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';

const html = htm.bind(h);

export const CONVERSATION_SEARCH_MIN = 3;
export const CONVERSATION_SEARCH_MAX = 200;
export const CONVERSATION_SEARCH_DEBOUNCE_MS = 400;

const IDLE = Object.freeze({ phase: 'idle' });

async function errorText(response) {
  try {
    const body = await response.json();
    return body?.error || body?.message || `search failed (${response.status})`;
  } catch (_) {
    return `search failed (${response.status})`;
  }
}

export async function searchConversations(query, signal, fetchImpl = globalThis.fetch) {
  const response = await fetchImpl(`/api/conversations/search?q=${encodeURIComponent(query)}`, {
    credentials: 'same-origin', signal,
  });
  if (!response.ok) throw new Error(await errorText(response));
  return response.json();
}

function outcomeText({ results = [], scanned = 0, total = 0, truncated = false }) {
  const found = `${results.length} conversation${results.length === 1 ? '' : 's'}`;
  if (!truncated) return `${found} found, ${total} searched`;
  return `${found} found in the newest ${scanned} of ${total}; older ones were not searched`;
}

function ResultAction({ row, wizard }) {
  const label = row.title || row.conv_id;
  if (!row.agent_id || row.retired) {
    return html`<button class="primary" data-act="promote-agent" data-conv=${row.conv_id} data-label=${label}
      title=${row.retired ? 'Bring this retired agent back' : 'Promote this conversation into an agent'}
    >${wizard ? 'awaken' : (row.retired ? 'reinstate' : 'promote')}</button>`;
  }
  return html`<button data-act="copy-generation-id" data-conv=${row.conv_id} data-label=${label}
    title="Already an agent. Copy its conv-id to find it, or to inspect an older generation."
  >copy id</button>`;
}

function ResultRow({ row, wizard }) {
  return html`<li class="content-search-row" data-conv-result=${row.conv_id}>
    <div class="content-search-head">
      <span class=${row.online ? 'online' : 'offline'} title=${row.online ? 'online' : 'offline'}>${row.online ? '●' : '○'}</span>
      <span class="rowname">${row.title || '(untitled)'}</span>
      ${row.agent_id ? html`<span class="muted">${row.retired ? 'retired agent' : 'agent'}</span>` : null}
      ${row.modified ? html`<span class="last-hook">${relTime(row.modified)}</span>` : null}
      <span class="row-actions"><${ResultAction} row=${row} wizard=${wizard} /></span>
    </div>
    ${row.matches.map((match, index) => html`<div class="content-search-match" key=${index}>${match.text}</div>`)}
  </li>`;
}

export function ConversationSearchPanel({ wizard = false, onClose, search = searchConversations, debounceMs = CONVERSATION_SEARCH_DEBOUNCE_MS }) {
  const [query, setQuery] = useState('');
  const [outcome, setOutcome] = useState(IDLE);
  const inputRef = useRef(null);
  const settled = query.trim();

  useEffect(() => { inputRef.current?.focus(); }, []);

  useEffect(() => {
    if (settled.length < CONVERSATION_SEARCH_MIN) {
      setOutcome(IDLE);
      return undefined;
    }
    const controller = new AbortController();
    const timer = setTimeout(async () => {
      setOutcome((before) => ({ ...before, phase: 'searching' }));
      try {
        const found = await search(settled, controller.signal);
        if (!controller.signal.aborted) setOutcome({ phase: 'done', found });
      } catch (cause) {
        if (!controller.signal.aborted) setOutcome({ phase: 'error', message: cause?.message || String(cause) });
      }
    }, debounceMs);
    return () => { clearTimeout(timer); controller.abort(); };
  }, [settled]);

  const rows = outcome.found?.results || [];
  return html`<div id="conversation-search" class="content-search" role="search">
    <div class="content-search-bar">
      <input ref=${inputRef} id="conversation-search-input" type="text" autocomplete="off" spellcheck=${false}
        maxlength=${CONVERSATION_SEARCH_MAX} value=${query}
        aria-label="Search conversation content"
        placeholder=${wizard ? 'Search the scrolls for exact words' : 'Search conversation content for exact text'}
        onInput=${(event) => setQuery(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Escape') onClose?.(); }} />
      <button id="conversation-search-close" type="button" class="clear-filter" aria-label="Close conversation search" onClick=${() => onClose?.()}>×</button>
    </div>
    <div id="conversation-search-status" class="muted" role="status">${
      outcome.phase === 'searching' ? 'Searching…'
        : outcome.phase === 'error' ? outcome.message
          : outcome.phase === 'done' ? outcomeText(outcome.found)
            : `Type at least ${CONVERSATION_SEARCH_MIN} characters. Titles and transcripts are searched, newest first.`
    }</div>
    ${rows.length ? html`<ul class="content-search-results">${rows.map((row) => html`<${ResultRow} key=${row.conv_id} row=${row} wizard=${wizard} />`)}</ul>` : null}
  </div>`;
}
