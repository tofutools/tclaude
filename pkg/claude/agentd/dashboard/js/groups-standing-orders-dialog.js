import { h } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { isWizardActive } from './slop.js';

const html = htm.bind(h);

function useWizardTheme() {
  const [wizard, setWizard] = useState(isWizardActive());
  useEffect(() => {
    const onWizard = (event) => setWizard(
      event.detail?.active == null ? isWizardActive() : Boolean(event.detail.active),
    );
    document.addEventListener('tclaude:wizard', onWizard);
    return () => document.removeEventListener('tclaude:wizard', onWizard);
  }, []);
  return wizard;
}

function scopeLabel(row, wizard) {
  if (row.global) return wizard ? 'realm-wide' : 'global';
  if (row.primary) return wizard ? 'this party' : 'primary group';
  return row.assigned ? (wizard ? 'bound here' : 'added here') : (wizard ? 'unbound' : 'available');
}

function OrderDetails({ row, wizard }) {
  const order = row.order || {};
  return html`<span class="group-standing-order-details">
    <strong>${order.name}</strong>
    <span class="muted">${order.summary}</span>
    <span class="muted">${order.trigger?.label || ''}${order.trigger?.label ? ' · ' : ''}${order.enabled
      ? (wizard ? 'decree enabled' : 'enabled')
      : (wizard ? 'master seal broken' : 'master disabled')}</span>
  </span>`;
}

export function GroupStandingOrdersDialog({ descriptor, actions }) {
  const [rows, setRows] = useState([]);
  const [loading, setLoading] = useState(true);
  const [loadFailed, setLoadFailed] = useState(false);
  const [error, setError] = useState('');
  const [query, setQuery] = useState('');
  const [browsing, setBrowsing] = useState(false);
  const [explaining, setExplaining] = useState(false);
  const [changing, setChanging] = useState(new Set());
  const group = descriptor.group;
  const wizard = useWizardTheme();

  const load = async () => {
    setLoading(true);
    setLoadFailed(false);
    setError('');
    try {
      const response = await actions.loadStandingOrders(group);
      setRows(Array.isArray(response?.orders) ? response.orders : []);
    } catch (err) {
      setError((err && err.message) || String(err));
      setLoadFailed(true);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [group]);

  const scoped = useMemo(() => rows.filter((row) =>
    row.global || row.primary || row.assigned), [rows]);
  const effective = useMemo(() => scoped.filter((row) => row.order?.enabled), [scoped]);
  const paused = useMemo(() => scoped.filter((row) => !row.order?.enabled), [scoped]);
  const available = useMemo(() => rows.filter((row) =>
    !row.global && !row.primary && !row.assigned), [rows]);
  const shownAvailable = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return available;
    return available.filter((row) => {
      const order = row.order || {};
      return [order.name, order.summary, order.trigger?.label]
        .some((value) => String(value || '').toLowerCase().includes(needle));
    });
  }, [available, query]);
  const firstRun = !loading && !loadFailed && rows.length === 0;

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
    <div class="group-standing-orders-heading">
      <div>
        <h3 id="group-standing-orders-title">${wizard ? `✦ Decrees of party ${group}` : `Rules for ${group}`}</h3>
        <p class="muted">${wizard
          ? 'Enduring commandments whispered to every familiar in this party.'
          : 'Durable guidance delivered to every agent in this group.'}</p>
      </div>
      <button type="button" class="group-standing-orders-close"
        aria-label=${wizard ? 'Seal decrees' : 'Close rules'}
        disabled=${changing.size > 0} onClick=${actions.closeStandingOrders}>×</button>
    </div>
    ${error && html`<div class="jobs-error" role="alert">${error}</div>`}
    ${loading
      ? html`<div class="empty">Loading standing orders…</div>`
      : loadFailed
        ? html`<div class="group-standing-orders-load-failed">
            <strong>${wizard ? 'The archive cannot be reached.' : 'Could not load this group’s rules.'}</strong>
            <span class="muted">${wizard
              ? 'The silence is not proof that no decrees are bound. Try the archive again.'
              : 'The rulebook may not be empty. Retry to load its current state.'}</span>
            <button type="button" onClick=${() => void load()}>${wizard ? 'consult again' : 'retry'}</button>
          </div>`
      : rows.length === 0
        ? html`<div class="group-standing-orders-first-run">
            <div class="group-standing-orders-first-icon" aria-hidden="true">${wizard ? '📜' : '◎'}</div>
            <strong>${wizard ? 'The party bears no decrees' : 'No standing orders yet'}</strong>
            <p>${wizard
              ? 'Inscribe the first enduring commandment. Present and future familiars alike shall heed its words.'
              : 'Create the first reusable rule for this group. It will apply to every current member and anyone added later.'}</p>
            <div class="group-standing-orders-first-actions">
              <button type="button" class="primary" onClick=${() => actions.openStandingOrderCreate(group)}>
                ${wizard ? '✦ inscribe first decree' : '+ create first rule'}</button>
              <button type="button" onClick=${() => setExplaining(!explaining)}
                aria-expanded=${explaining ? 'true' : 'false'}>
                ${wizard ? 'consult the lore' : 'what are standing orders?'}</button>
            </div>
            ${explaining && html`<div class="group-standing-orders-explainer">
              ${wizard
                ? 'Decrees are reusable instructions delivered when a chosen omen occurs. Their bindings and omens remain editable in Labours.'
                : 'Standing orders are reusable instructions delivered when a chosen trigger occurs. Their scope and trigger remain editable in Automations.'}
            </div>`}
          </div>
          <div class="group-standing-orders-first-hint">
            <strong>${wizard ? 'Scrolls already bound elsewhere?' : 'Already have rules elsewhere?'}</strong>
            ${wizard
              ? ' When the archive holds decrees, it may be opened from here.'
              : ' Once reusable rules exist, this view offers a library to add them here.'}
          </div>`
      : html`
        <div class="group-standing-orders-summary">
          <strong><span class="group-standing-orders-live" aria-hidden="true"></span>${`${effective.length} ${wizard ? 'decrees' : 'rules'} in force`}</strong>
          <span>${`${effective.filter((row) => row.global).length} ${wizard ? 'realm-wide' : 'global'} · ${effective.filter((row) => !row.global).length} ${wizard ? 'of this party' : 'group-scoped'}`}</span>
        </div>
        <div class="group-standing-orders-section-heading">
          <strong>${wizard ? 'Decrees in force' : 'In force'}</strong>
          <span class="muted">${wizard
            ? 'heeded by present and future familiars'
            : 'applies to all current and future members'}</span>
        </div>
        ${effective.length === 0
          ? html`<div class="group-standing-orders-empty">
              <strong>${wizard ? 'No decrees are bound to this party.' : 'No rules apply to this group yet.'}</strong>
              <span class="muted">${wizard
                ? 'Bind a scroll from the archive, or inscribe the first decree.'
                : 'Add one from the library, or create the first rule.'}</span>
            </div>`
          : html`<div class="group-standing-orders-list group-standing-orders-effective">
              ${effective.map((row) => {
                const order = row.order || {};
                const locked = row.global || row.primary;
                const busy = changing.has(order.id);
                return html`<div
                  key=${order.id}
                  class=${`standing-order-choice group-standing-order-choice${order.enabled ? '' : ' disabled'}`}
                  title=${locked
                    ? `${scopeLabel(row, wizard)}; ${wizard ? 'govern this decree in Labours' : 'manage this scope in Automations'}`
                    : `${wizard ? 'Unbind' : 'Remove'} ${order.name} ${wizard ? 'from this party' : `from ${group}`}`}
                >
                  <span class=${`group-standing-order-origin ${locked ? 'locked' : ''}`} aria-hidden="true">${locked ? '◆' : '✓'}</span>
                  <${OrderDetails} row=${row} wizard=${wizard} />
                  <span class=${`group-standing-order-scope ${row.global ? 'global' : 'group'}`}>${scopeLabel(row, wizard)}</span>
                  ${locked
                    ? html`<span class="group-standing-order-lock" aria-label=${wizard ? 'governed elsewhere' : 'managed elsewhere'}>◇</span>`
                    : html`<button type="button" class="group-standing-order-remove" disabled=${busy}
                        onClick=${() => void toggle(row, false)}>${wizard ? 'unbind' : 'remove'}</button>`}
                </div>`;
              })}</div>`}
        ${paused.length > 0 && html`
          <div class="group-standing-orders-section-heading group-standing-orders-paused-heading">
            <strong>${wizard ? 'Seals broken at the source' : 'Master disabled'}</strong>
            <span class="muted">${wizard ? 'scoped here, but unable to speak' : 'scoped here, but not delivered'}</span>
          </div>
          <div class="group-standing-orders-list group-standing-orders-paused">
            ${paused.map((row) => {
              const order = row.order || {};
              const locked = row.global || row.primary;
              const busy = changing.has(order.id);
              return html`<div key=${order.id} class="standing-order-choice group-standing-order-choice disabled">
                <span class=${`group-standing-order-origin ${locked ? 'locked' : ''}`} aria-hidden="true">○</span>
                <${OrderDetails} row=${row} wizard=${wizard} />
                <span class=${`group-standing-order-scope ${row.global ? 'global' : 'group'}`}>${scopeLabel(row, wizard)}</span>
                ${locked
                  ? html`<span class="group-standing-order-lock" aria-label=${wizard ? 'governed elsewhere' : 'managed elsewhere'}>◇</span>`
                  : html`<button type="button" class="group-standing-order-remove" disabled=${busy}
                      onClick=${() => void toggle(row, false)}>${wizard ? 'unbind' : 'remove'}</button>`}
              </div>`;
            })}
          </div>`}

        <div class="group-standing-orders-section-heading group-standing-orders-add-heading">
          <strong>${wizard ? 'Bind another decree' : 'Add to this group'}</strong>
          <span class="muted">${available.length} ${wizard ? 'scrolls await in the archive' : 'reusable rules available'}</span>
        </div>
        ${browsing && html`<div class="group-standing-orders-library">
          <div class="group-standing-orders-library-toolbar">
            <input id="group-standing-orders-filter" type="search"
              placeholder=${wizard ? 'Scry for a decree…' : 'Filter reusable rules…'}
              aria-label=${wizard ? 'Scry for a decree' : 'Filter reusable rules'}
              value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} />
            <button type="button" onClick=${() => { setBrowsing(false); setQuery(''); }}>hide</button>
          </div>
          ${shownAvailable.length === 0
            ? html`<div class="empty">${available.length
              ? (wizard ? 'No decrees answer that scrying.' : 'No reusable rules match this filter.')
              : (wizard ? 'The archive holds no unbound decrees.' : 'Every reusable rule is already in force.')}</div>`
            : html`<div class="group-standing-orders-list">
              ${shownAvailable.map((row) => {
                const order = row.order || {};
                const busy = changing.has(order.id);
                return html`<div key=${order.id} class=${`standing-order-choice group-standing-order-choice${order.enabled ? '' : ' disabled'}`}>
                  <span class="group-standing-order-origin" aria-hidden="true">＋</span>
                  <${OrderDetails} row=${row} wizard=${wizard} />
                  <button type="button" class="primary" disabled=${busy}
                    onClick=${() => void toggle(row, true)}>${wizard ? 'bind' : 'add'}</button>
                </div>`;
              })}</div>`}
        </div>`}
        <div class="group-standing-orders-library-callout">
          <div><strong>${wizard ? 'Consult the archive of decrees' : 'Choose from the standing-order library'}</strong>
            <span class="muted">${wizard
              ? 'Read, divine, and bind an existing scroll to this party.'
              : 'Browse, preview, and add existing rules without leaving this group.'}</span></div>
          <div class="group-standing-orders-callout-actions">
            <button type="button" onClick=${() => setBrowsing(!browsing)}>${browsing
              ? (wizard ? 'close archive' : 'hide library')
              : (wizard ? 'open archive…' : 'browse library…')}</button>
            <button type="button" class="primary" onClick=${() => actions.openStandingOrderCreate(group)}>
              ${wizard ? '✦ inscribe decree' : '+ create rule'}</button>
          </div>
        </div>`}
    <div class="modal-buttons">
      <span class="group-standing-orders-managed-note">◇ ${wizard
        ? (firstRun ? 'Realm-wide decrees shall reveal themselves here' : 'Realm-wide decrees are governed in Labours')
        : (firstRun ? 'Global rules will appear here automatically' : 'Inherited rules are managed in Automations')}</span>
      <button type="button" onClick=${actions.closeStandingOrders} disabled=${changing.size > 0}>${wizard ? 'seal' : 'done'}</button>
    </div>
  <//>`;
}
