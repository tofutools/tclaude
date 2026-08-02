import { h, render } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

const statusLabel = (value) => ({
  ready: 'ready', draining: 'draining', withdrawn: 'withdrawn',
  'publisher-lost': 'publisher lost', open: 'open', closed: 'closed',
  pending: 'pending', refused: 'refused', current: 'current', offline: 'offline',
  'restart-needed': 'restart needed', stale: 'stale', 'wrong-group': 'wrong group',
  hidden: 'hidden boundary',
}[String(value || '')] || String(value || 'unknown'));

function routeStatusClass(value) {
  return `route-status route-status-${String(value || 'unknown').replace(/[^a-z0-9-]/gi, '-')}`;
}

function Boundary({ value }) {
  if (!value || value === 'in-group') return null;
  return html`<span class="route-boundary" title="The route authority no longer exposes this endpoint as a current member of the selected group">${statusLabel(value)} boundary</span>`;
}

function friendlyEndpoint(name, boundary) {
  return boundary && boundary !== 'in-group' ? statusLabel(boundary) + ' boundary' : (name || 'hidden endpoint');
}

function routeGroups(snapshot) {
  return (snapshot?.groups || []).filter((group) => !group.virtual);
}

function routesForGroup(routeMap, groupName) {
  const routes = routeMap?.routes || [];
  return groupName ? routes.filter((route) => route.group === groupName) : routes;
}

function edgeRows(routes) {
  const rows = [];
  for (const route of routes) {
    for (const consumer of route.consumers || []) {
      rows.push({ route, consumer, key: `${route.id}:${consumer.id}` });
    }
  }
  return rows;
}

function GraphNode({ node, position, focused }) {
  return html`<g class=${`route-map-node${focused ? ' is-focused' : ''}`} transform=${`translate(${position.x} ${position.y})`}>
    <rect x="-78" y="-26" width="156" height="52" rx="8" />
    <text class="route-map-node-title" x="0" y="-4" text-anchor="middle">${node.name}</text>
    <text class="route-map-node-meta" x="0" y="14" text-anchor="middle">${node.boundary ? statusLabel(node.boundary) : statusLabel(node.health)}</text>
  </g>`;
}

function RouteGraph({ routes, members, selected, onSelect }) {
  const edges = edgeRows(routes);
  const nodes = [];
  const byID = new Map();
  for (const member of members || []) {
    const id = String(member.agent_id || member.conv_id || '');
    if (!id || byID.has(id)) continue;
    const node = { id, name: member.title || 'unnamed agent', health: member.online ? 'current' : 'offline' };
    byID.set(id, node);
    nodes.push(node);
  }
  for (const { route, consumer } of edges) {
    const publisherID = String(route.publisher_agent_id || '');
    const consumerID = String(consumer.consumer_agent_id || '');
    for (const [id, name, boundary, health] of [
      [publisherID, route.publisher_name, route.publisher_boundary, route.publisher_health],
      [consumerID, consumer.consumer_name, consumer.boundary, consumer.consumer_health],
    ]) {
      if (!id || byID.has(id)) continue;
      const node = { id, name: friendlyEndpoint(name, boundary), health, boundary };
      byID.set(id, node);
      nodes.push(node);
    }
  }
  const positions = new Map(nodes.map((node, index) => [node.id, {
    x: 120 + (index % 3) * 260,
    y: 76 + Math.floor(index / 3) * 105,
  }]));
  const width = 900;
  const height = Math.max(260, 152 + Math.ceil(nodes.length / 3) * 105);
  return html`<div class="route-map-graph-frame">
    <svg class="route-map-graph" viewBox=${`0 0 ${width} ${height}`} role="img" aria-label="Named route graph">
      <defs><marker id="route-map-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" /></marker></defs>
      ${edges.map(({ route, consumer, key }) => {
        const from = positions.get(String(route.publisher_agent_id || ''));
        const to = positions.get(String(consumer.consumer_agent_id || ''));
        if (!from || !to) return null;
        const focused = selected === route.id;
        return html`<g key=${key} class=${`route-map-edge${focused ? ' is-focused' : ''}`} onClick=${() => onSelect(route.id)} tabindex="0" role="button" aria-label=${`${route.name} from ${route.publisher_name || 'hidden publisher'} to ${consumer.consumer_name || 'hidden consumer'}`}>
          <line x1=${from.x} y1=${from.y} x2=${to.x} y2=${to.y} marker-end="url(#route-map-arrow)" />
          <text x=${(from.x + to.x) / 2} y=${(from.y + to.y) / 2 - 8} text-anchor="middle">${route.name}</text>
        </g>`;
      })}
      ${nodes.map((node) => html`<${GraphNode} key=${node.id} node=${node} position=${positions.get(node.id)} focused=${edges.some(({ route, consumer }) => selected === route.id && (route.publisher_agent_id === node.id || consumer.consumer_agent_id === node.id))} />`)}
    </svg>
    ${!edges.length && html`<p class="route-map-empty-graph">No open leases yet. Published routes appear in the exact list below without implying an ambient connection.</p>`}
  </div>`;
}

function RouteDetail({ route, onClose }) {
  if (!route) return html`<aside class="route-map-detail route-map-detail-empty"><strong>Select a route</strong><span>Choose an edge or exact-list row to inspect its safe authority status.</span></aside>`;
  return html`<aside class="route-map-detail" aria-label="Route details">
    <div class="route-map-detail-head"><div><span class="route-map-kicker">Route detail</span><h3>${route.name}</h3></div><button type="button" class="route-map-close" onClick=${onClose} aria-label="Close route detail">×</button></div>
    <dl class="route-map-fields">
      <div><dt>Stable reference</dt><dd><code>${route.stable_reference}</code></dd></div>
      <div><dt>Publisher</dt><dd>${route.publisher_name || friendlyEndpoint('', route.publisher_boundary)} <${Boundary} value=${route.publisher_boundary} /></dd></div>
      <div><dt>Transport</dt><dd>${route.transport || 'unknown'}</dd></div>
      <div><dt>Lifecycle</dt><dd><span class=${routeStatusClass(route.state)}>${statusLabel(route.state)}</span></dd></div>
      <div><dt>Generation</dt><dd><span class=${routeStatusClass(route.generation_health)}>${statusLabel(route.generation_health)}</span></dd></div>
    </dl>
    <h4>Consumers</h4>
    ${route.consumers?.length ? html`<ul class="route-map-consumers">${route.consumers.map((consumer) => html`<li key=${consumer.id}><span>${friendlyEndpoint(consumer.consumer_name, consumer.boundary)}</span><span class=${routeStatusClass(consumer.endpoint_state)}>${statusLabel(consumer.endpoint_state)}</span><span class=${routeStatusClass(consumer.consumer_health)}>${statusLabel(consumer.consumer_health)}</span><${Boundary} value=${consumer.boundary} /></li>`)}</ul>` : html`<p class="route-map-muted">No consumer lease.</p>`}
    <p class="route-map-security-note">Read-only authority view. Endpoint addresses, targets, capabilities, credentials, and payload data are never shown.</p>
  </aside>`;
}

export function GroupsRouteMap({ state }) {
  const snapshot = state.snapshot.value;
  const map = snapshot?.route_map || { routes: [] };
  const groups = routeGroups(snapshot);
  const selectedRouteID = state.routeSelection.value;
  const initialGroup = groups[0]?.name || '';
  const [groupName, setGroupName] = useState(initialGroup);
  const [mode, setMode] = useState('graph');
  useEffect(() => {
    const restore = (event) => {
      const location = event.detail?.location;
      if (location?.tab !== 'groups') return;
      state.setSubview(location.subtab === 'routes' ? 'routes' : 'members', location.selection || '');
    };
    document.addEventListener('tclaude:restore-location', restore);
    return () => document.removeEventListener('tclaude:restore-location', restore);
  }, [state]);
  const selectedRoute = useMemo(() => (map.routes || []).find((route) => route.id === selectedRouteID) || null, [map.routes, selectedRouteID]);
  useEffect(() => {
    if (selectedRoute?.group) setGroupName(selectedRoute.group);
  }, [selectedRoute?.id, selectedRoute?.group]);
  useEffect(() => {
    if (!groupName && groups.length) setGroupName(groups[0].name);
  }, [groupName, groups.length, groups[0]?.name]);
  const routes = routesForGroup(map, groupName);
  const members = groups.find((group) => group.name === groupName)?.members || [];
  const isRoutes = state.subview.value === 'routes';
  useEffect(() => {
    const filterBar = document.querySelector('#groups-filter-root')?.parentElement;
    const list = document.querySelector('#groups-list');
    const hints = document.querySelector('.groups-dnd-hints');
    for (const element of [filterBar, list, hints]) {
      if (element) element.hidden = isRoutes;
    }
    return () => {
      for (const element of [filterBar, list, hints]) {
        if (element) element.hidden = false;
      }
    };
  }, [isRoutes]);
  const navigateRoute = (routeID) => {
    state.setSubview('routes', routeID || '');
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: { tab: 'groups', subtab: 'routes', ...(routeID ? { selection: routeID } : {}) } } }));
  };
  const changeSubview = (next) => {
    state.setSubview(next);
    const location = next === 'routes'
      ? { tab: 'groups', subtab: 'routes' }
      : { tab: 'groups' };
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location } }));
  };
  const selectGroup = (event) => setGroupName(event.currentTarget.value);
  return html`<div class=${`groups-route-map-surface${isRoutes ? ' is-routes' : ''}`}>
    <div class="groups-subnav" role="tablist" aria-label="Groups views">
      <button type="button" role="tab" aria-selected=${state.subview.value === 'members'} class=${state.subview.value === 'members' ? 'active' : ''} onClick=${() => changeSubview('members')}>Members</button>
      <button type="button" role="tab" aria-selected=${state.subview.value === 'routes'} class=${state.subview.value === 'routes' ? 'active' : ''} onClick=${() => changeSubview('routes')}>Route map</button>
    </div>
    ${isRoutes && html`<div class="route-map-toolbar">
      <label>Group <select value=${groupName} onChange=${selectGroup}><option value="">All groups</option>${groups.map((group) => html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}</select></label>
      <div class="route-map-mode" role="group" aria-label="Route map view mode"><button type="button" class=${mode === 'graph' ? 'active' : ''} aria-pressed=${mode === 'graph'} onClick=${() => setMode('graph')}>Graph</button><button type="button" class=${mode === 'list' ? 'active' : ''} aria-pressed=${mode === 'list'} onClick=${() => setMode('list')}>Exact list</button></div>
      <span class="route-map-count" aria-live="polite">${routes.length} route${routes.length === 1 ? '' : 's'}</span>
    </div>`}
    ${isRoutes && map.platform === 'darwin' && html`<div class="route-map-disclosure" role="note">Darwin bounded capacity: ${map.darwin_slots || 'configured'} slots · ${map.darwin_boundary || 'Partial boundary disclosed'}</div>`}
    ${isRoutes && (!routes.length ? html`<div class="route-map-empty"><strong>No named routes in this group.</strong><span>When the route authority has records, this surface will show explicit publisher → consumer leases.</span></div>` : html`<div class=${`route-map-content route-map-mode-${mode}`}>
      ${mode === 'graph' && html`<${RouteGraph} routes=${routes} members=${members} selected=${selectedRouteID} onSelect=${navigateRoute} />`}
      ${mode === 'list' && html`<div class="route-map-table-wrap"><table class="route-map-table"><caption class="sr-only">Exact named route listing</caption><thead><tr><th>Route</th><th>Publisher</th><th>Consumer</th><th>Transport</th><th>Lifecycle</th><th>Health</th></tr></thead><tbody>${routes.map((route) => html`<tr key=${route.id} class=${selectedRouteID === route.id ? 'is-selected' : ''} onClick=${() => navigateRoute(route.id)}><td><button type="button" class="route-map-link">${route.name}</button><code>${route.stable_reference}</code></td><td>${friendlyEndpoint(route.publisher_name, route.publisher_boundary)}</td><td>${route.consumers?.length ? route.consumers.map((consumer) => html`<span class="route-map-consumer-line" key=${consumer.id}>${friendlyEndpoint(consumer.consumer_name, consumer.boundary)} · ${statusLabel(consumer.endpoint_state)}</span>`) : html`<span class="route-map-muted">none</span>`}</td><td>${route.transport || 'unknown'}</td><td><span class=${routeStatusClass(route.state)}>${statusLabel(route.state)}</span></td><td><span class=${routeStatusClass(route.generation_health)}>${statusLabel(route.generation_health)}</span> · ${statusLabel(route.publisher_health)}</td></tr>`)}</tbody></table></div>`}
      <${RouteDetail} route=${selectedRoute} onClose=${() => navigateRoute('')} />
    </div>`)}
  </div>`;
}

export function mountGroupsRouteMap({ host, state, registerCleanup }) {
  render(html`<${GroupsRouteMap} state=${state} />`, host);
  registerCleanup(() => render(null, host));
}
