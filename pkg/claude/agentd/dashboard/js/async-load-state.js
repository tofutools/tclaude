import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

// Shared only for the pilot-proven loading/error/retry contract. Feature
// layout, stale content, paging, dialogs, and focus policy remain feature-owned.
export function AsyncLoadState({ label, request, retry, errorClass = 'island-error' }) {
  if (request?.error) {
    const suffix = request.hasLoaded ? ' Showing the last successful page.' : '';
    return html`<div class=${errorClass} role="alert">
      ${label} refresh failed: ${request.error}.${suffix}
      <button onClick=${retry}>Retry</button>
    </div>`;
  }
  if (!request?.hasLoaded) {
    return html`<div class="empty" role="status" aria-live="polite">Loading ${label.toLowerCase()}…</div>`;
  }
  return null;
}
