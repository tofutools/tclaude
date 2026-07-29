import { h } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

function rowScopeLabel(row) {
  if (row.global) return 'global';
  if (row.primary) return 'primary group scope';
  return row.assigned ? 'assigned here' : 'available';
}

export function GroupStandingOrdersDialog({ descriptor, actions }) {
  const [rows, setRows] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [query, setQuery] = useState('');
  const [changing, setChanging] = useState(new Set());
  const group = descriptor.group;

  const load = async () => {
    setLoading(true);
    setError('');
    try {
      const response = await actions.loadStandingOrders(group);
      setRows(Array.isArray(response?.orders) ? response.orders : []);
    } catch (err) {
      setError((err && err.message) || String(err));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [group]);

  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return rows;
    return rows.filter((row) => {
      const order = row.order || {};
      return [order.name, order.summary, order.trigger?.label]
        .some((value) => String(value || '').toLowerCase().includes(needle));
    });
  }, [rows, query]);

  const toggle = async (row, assigned) => {
    const id = row.order?.id;
    if (!id || changing.has(id)) return;
    setChanging((current) => new Set(current).add(id));
    setError('');
    try {
      const updated = await actions.setStandingOrderScope(group, row, assigned);
      setRows((current) => current.map((candidate) =>
        candidate.order?.id === id ? updated : candidate));
    } catch (err) {
      setError((err && err.message) || String(err));
    } finally {
      setChanging((current) => {
        const next = new Set(current);
        next.delete(id);
        return next;
      });
    }
  };

  return html`<${Overlay}
    id="group-standing-orders-modal"
    labelledby="group-standing-orders-title"
    onClose=${actions.closeStandingOrders}
    blocked=${changing.size > 0}
    resizeKey="tclaude.dash.modalSize.group-standing-orders"
  >
    <h3 id="group-standing-orders-title">Standing orders for ${group}</h3>
    <p class="muted">
      Select reusable orders that should also apply to every agent in this group.
      Global and primary group scopes are shown but changed from
      <a href="/automations/standing-orders"> Automations</a>.
    </p>
    <div class="cron-create-row">
      <input
        id="group-standing-orders-filter"
        type="search"
        placeholder="Filter standing orders"
        value=${query}
        onInput=${(event) => setQuery(event.currentTarget.value)}
      />
      <button type="button" onClick=${() => void load()} disabled=${loading || changing.size > 0}>
        refresh
      </button>
    </div>
    ${error && html`<div class="jobs-error" role="alert">${error}</div>`}
    ${loading
      ? html`<div class="empty">Loading standing orders…</div>`
      : rows.length === 0
        ? html`<div class="empty">
            No standing orders exist yet.
            <a href="/automations/standing-orders">Create one in Automations.</a>
          </div>`
        : shown.length === 0
          ? html`<div class="empty">No standing orders match this filter.</div>`
          : html`<div class="group-standing-orders-list">
              ${shown.map((row) => {
                const order = row.order || {};
                const locked = row.global || row.primary;
                const busy = changing.has(order.id);
                return html`<label
                  key=${order.id}
                  class=${`standing-order-choice group-standing-order-choice${order.enabled ? '' : ' disabled'}`}
                  title=${locked
                    ? `${rowScopeLabel(row)}; edit the primary target in Automations`
                    : `Toggle whether ${order.name} applies to ${group}`}
                >
                  <input
                    type="checkbox"
                    checked=${row.global || row.assigned}
                    disabled=${locked || busy}
                    onChange=${(event) => void toggle(row, event.currentTarget.checked)}
                  />
                  <span>
                    <strong>${order.name}</strong>
                    <span class="muted">${order.summary}</span>
                    <span class="muted">
                      ${rowScopeLabel(row)} · ${order.enabled ? 'enabled' : 'master disabled'}
                      · ${order.trigger?.label || ''}
                    </span>
                  </span>
                </label>`;
              })}
            </div>`}
    <div class="modal-buttons">
      <button type="button" onClick=${actions.closeStandingOrders} disabled=${changing.size > 0}>close</button>
    </div>
  <//>`;
}
