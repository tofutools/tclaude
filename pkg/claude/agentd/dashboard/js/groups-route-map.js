import { h, render } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import { effectiveGroupsFocus, groupsFocusLocation } from './groups-focus.js';
import { isLoopbackDashboard } from './helpers.js';
import { remoteNodeID } from './fleet-model.js';
import { hostRoutePreviewURL } from './groups-route-preview.js';

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
  if (!value || value === 'in-group' || value === 'host') return null;
  return html`<span class="route-boundary" title="The route authority no longer exposes this endpoint as a current member of the selected group">${statusLabel(value)} boundary</span>`;
}

function friendlyEndpoint(name, boundary) {
  return boundary && boundary !== 'in-group' && boundary !== 'host' ? statusLabel(boundary) + ' boundary' : (name || 'hidden endpoint');
}

function routeGroups(snapshot) {
  return (snapshot?.groups || []).filter((group) => !group.virtual);
}

function routesForGroup(routeMap, groupName) {
  const routes = routeMap?.routes || [];
  return routes.filter((route) => route.host_only || (groupName && route.group === groupName));
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
    const node = { id, name: member.title || 'unnamed agent', health: member.route_health || (member.online ? 'current' : 'offline') };
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
          <text x=${(from.x + to.x) / 2} y=${(from.y + to.y) / 2 - 10} text-anchor="middle">${route.name}</text>
          <text class="route-map-edge-status" x=${(from.x + to.x) / 2} y=${(from.y + to.y) / 2 + 7} text-anchor="middle">${statusLabel(route.state)} · ${statusLabel(consumer.endpoint_state)}</text>
        </g>`;
      })}
      ${nodes.map((node) => html`<${GraphNode} key=${node.id} node=${node} position=${positions.get(node.id)} focused=${edges.some(({ route, consumer }) => selected === route.id && (route.publisher_agent_id === node.id || consumer.consumer_agent_id === node.id))} />`)}
    </svg>
    ${!edges.length && html`<p class="route-map-empty-graph">No open leases yet. Published routes appear in the exact list below without implying an ambient connection.</p>`}
  </div>`;
}

function HostEndpointLink({ lease, local }) {
  const url = hostRoutePreviewURL(lease, { local, hostname: window.location?.hostname });
  return url
    ? html`<span><code>${lease.endpoint}</code> · <a class="route-map-preview" href=${url} target="_blank" rel="noopener noreferrer"><code>${url}</code></a></span>`
    : html`<code>${lease.endpoint}</code>`;
}

function HostRouteControls({ route, local }) {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [openedLease, setOpenedLease] = useState(null);
  useEffect(() => {
    if (openedLease && route !== openedLease.snapshot && !(route.consumers || []).some((consumer) => consumer.id === openedLease.id)) {
      setOpenedLease(null);
    }
  }, [route, openedLease]);
  const act = async (leaseID, mode = 'http') => {
    setBusy(true);
    setMessage('');
    setOpenedLease(null);
    try {
      const response = await fetch(leaseID ? `/api/routes/host/${encodeURIComponent(leaseID)}` : `/api/routes/${encodeURIComponent(route.id)}/host`, {
        method: leaseID ? 'DELETE' : 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' }, ...(leaseID ? {} : { body: JSON.stringify({ mode }) }),
      });
      if (!response.ok) throw new Error(await response.text());
      if (!leaseID) {
        const lease = await response.json();
        setOpenedLease({ id: lease.id, endpoint: lease.endpoint, mode: lease.mode, snapshot: route });
      }
    } catch (error) { setMessage(String(error.message || error)); }
    finally { setBusy(false); }
  };
  return html`<div>
    <button type="button" disabled=${busy || route.state !== 'ready' || route.generation_health !== 'current'} onClick=${() => act('')}>Open on host</button>
    <button type="button" disabled=${busy || route.state !== 'ready' || route.generation_health !== 'current'} onClick=${() => act('', 'tcp')}>Open TCP endpoint</button>
    <p class="route-map-muted">Open on host creates an HTTP preview with the Host header set to the published target. Use Open TCP endpoint for other protocols. Both close when withdrawn.</p>
    ${!local && html`<p class="route-map-muted">These endpoints are on the daemon host. Forward the endpoint port with an SSH tunnel to access it from this machine.</p>`}
    ${(route.consumers || []).filter((consumer) => consumer.consumer_kind === 'host').map((consumer) => html`<p key=${consumer.id}><${HostEndpointLink} lease=${consumer} local=${local} /> <button type="button" disabled=${busy} onClick=${() => act(consumer.id)}>Close host endpoint</button></p>`)}
    ${openedLease && !(route.consumers || []).some((consumer) => consumer.id === openedLease.id) && html`<p role="status">Host endpoint: <${HostEndpointLink} lease=${openedLease} local=${local} /></p>`}
    ${message && html`<p role="status">${message}</p>`}
  </div>`;
}

function RouteDetail({ route, onClose, local }) {
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
    <${HostRouteControls} key=${route.id} route=${route} local=${local} />
    <h4>Consumers</h4>
    ${route.consumers?.length ? html`<ul class="route-map-consumers">${route.consumers.map((consumer) => html`<li key=${consumer.id}><span>${friendlyEndpoint(consumer.consumer_name, consumer.boundary)}</span><span class=${routeStatusClass(consumer.endpoint_state)}>${statusLabel(consumer.endpoint_state)}</span><span class=${routeStatusClass(consumer.consumer_health)}>${statusLabel(consumer.consumer_health)}</span><${Boundary} value=${consumer.boundary} /></li>`)}</ul>` : html`<p class="route-map-muted">No consumer lease.</p>`}
    <p class="route-map-security-note">Host endpoints are loopback-only. Publisher targets, helper credentials, and payload data are never shown.</p>
  </aside>`;
}

// routesLocation is a Route map location:
// /groups/routes[/<route-id>][?group=<name>][&view=list].
function routesLocation(routeID, group, view = 'graph') {
  const params = { ...(group ? { group } : {}), ...(view === 'list' ? { view } : {}) };
  return { tab: 'groups', subtab: 'routes', ...(routeID ? { selection: routeID } : {}), ...(Object.keys(params).length ? { params } : {}) };
}

export function GroupsRouteMap({ state }) {
  const snapshot = state.snapshot.value;
  const enabled = snapshot?.groups_route_map_enabled === true;
  const map = enabled ? (snapshot?.route_map || { routes: [] }) : { routes: [] };
  const groups = routeGroups(snapshot);
  const selectedRouteID = state.routeSelection.value;
  // The shown group is in the URL (?group=): the one picked, else the
  // selected route's, else the first.
  const selectedByID = (map.routes || []).find((route) => route.id === selectedRouteID) || null;
  const picked = state.routeGroup.value;
  const groupName = (picked && groups.some((group) => group.name === picked) && picked)
    || selectedByID?.group || groups[0]?.name || '';
  const mode = state.routeView.value;
  const setMode = (next) => {
    if (next === mode) return;
    state.setRouteView(next);
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: routesLocation(state.routeSelection.value, state.routeGroup.value, next) } }));
  };
  useEffect(() => {
    const restore = (event) => {
      const location = event.detail?.location;
      if (location?.tab !== 'groups') return;
      const routes = enabled && location.subtab === 'routes';
      state.setSubview(routes ? 'routes' : 'members', enabled ? (location.selection || '') : '');
      if (routes) { state.setRouteGroup(location.params?.group || ''); state.setRouteView(location.params?.view); }
    };
    document.addEventListener('tclaude:restore-location', restore);
    return () => document.removeEventListener('tclaude:restore-location', restore);
  }, [enabled, state]);
  useEffect(() => {
    if (!enabled && (state.subview.value === 'routes' || state.routeSelection.value)) {
      state.setSubview('members', '');
    }
  }, [enabled, state]);
  const selectedRoute = useMemo(() => (map.routes || []).find((route) => route.id === selectedRouteID && (route.host_only || route.group === groupName)) || null, [map.routes, selectedRouteID, groupName]);
  const routes = routesForGroup(map, groupName);
  const members = groups.find((group) => group.name === groupName)?.members || [];
  const darwinCapacity = map.darwin_capacity?.[groupName] || null;
  const darwinCapacityText = darwinCapacity
    ? `${darwinCapacity.used}/${darwinCapacity.total} in use across ${darwinCapacity.pools} per-launch pools · ${darwinCapacity.available} available`
    : 'Select a group for scoped capacity';
  const isRoutes = enabled && state.subview.value === 'routes';
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
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: routesLocation(routeID, state.routeGroup.value, state.routeView.value) } }));
  };
  const changeSubview = (next) => {
    state.setSubview(next);
    // Members shows whichever Agents | Processes lens is remembered.
    const location = next === 'routes'
      ? routesLocation('', state.routeGroup.value, state.routeView.value)
      : groupsFocusLocation(effectiveGroupsFocus(state));
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location } }));
  };
  const selectGroup = (event) => {
    state.setRouteGroup(event.currentTarget.value);
    state.setSubview('routes', '');
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: routesLocation('', state.routeGroup.value, state.routeView.value) } }));
  };
  if (!enabled) return null;
  return html`<div class=${`groups-route-map-surface${isRoutes ? ' is-routes' : ''}`}>
    <div class="groups-subnav" role="tablist" aria-label="Groups views">
      <button type="button" role="tab" aria-selected=${state.subview.value === 'members'} class=${state.subview.value === 'members' ? 'active' : ''} onClick=${() => changeSubview('members')}>Members</button>
      <button type="button" role="tab" aria-selected=${state.subview.value === 'routes'} class=${state.subview.value === 'routes' ? 'active' : ''} onClick=${() => changeSubview('routes')}>Route map</button>
    </div>
    ${isRoutes && html`<div class="route-map-toolbar">
      <label>Group <select value=${groupName} onChange=${selectGroup}>${groups.map((group) => html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}</select></label>
      <div class="route-map-mode" role="group" aria-label="Route map view mode"><button type="button" class=${mode === 'graph' ? 'active' : ''} aria-pressed=${mode === 'graph'} onClick=${() => setMode('graph')}>Graph</button><button type="button" class=${mode === 'list' ? 'active' : ''} aria-pressed=${mode === 'list'} onClick=${() => setMode('list')}>Exact list</button></div>
      <span class="route-map-count" aria-live="polite">${routes.length} route${routes.length === 1 ? '' : 's'}</span>
    </div>`}
    ${isRoutes && map.platform === 'darwin' && html`<div class="route-map-disclosure" role="note">Darwin bounded capacity: ${darwinCapacityText} · ${map.darwin_boundary || 'Partial boundary disclosed'}</div>`}
    ${isRoutes && (!routes.length ? html`<div class="route-map-empty"><strong>No named routes in this group.</strong><span>When the route authority has records, this surface will show explicit publisher → consumer leases.</span></div>` : html`<div class=${`route-map-content route-map-mode-${mode}`}>
      ${mode === 'graph' && html`<${RouteGraph} routes=${routes} members=${members} selected=${selectedRouteID} onSelect=${navigateRoute} />`}
      ${mode === 'list' && html`<div class="route-map-table-wrap"><table class="route-map-table"><caption class="sr-only">Exact named route listing</caption><thead><tr><th>Route</th><th>Publisher</th><th>Consumer</th><th>Transport</th><th>Lifecycle</th><th>Health</th></tr></thead><tbody>${routes.map((route) => html`<tr key=${route.id} class=${selectedRouteID === route.id ? 'is-selected' : ''} onClick=${() => navigateRoute(route.id)}><td><button type="button" class="route-map-link">${route.name}</button>${route.host_only && html`<span class="route-map-muted">Host only</span>`}<code>${route.stable_reference}</code></td><td>${friendlyEndpoint(route.publisher_name, route.publisher_boundary)}</td><td>${route.consumers?.length ? route.consumers.map((consumer) => html`<span class="route-map-consumer-line" key=${consumer.id}>${friendlyEndpoint(consumer.consumer_name, consumer.boundary)} · ${statusLabel(consumer.endpoint_state)}</span>`) : html`<span class="route-map-muted">none</span>`}</td><td>${route.transport || 'unknown'}</td><td><span class=${routeStatusClass(route.state)}>${statusLabel(route.state)}</span></td><td><span class=${routeStatusClass(route.generation_health)}>${statusLabel(route.generation_health)}</span> · ${statusLabel(route.publisher_health)}</td></tr>`)}</tbody></table></div>`}
      <${RouteDetail} route=${selectedRoute} local=${!remoteNodeID() && !snapshot?.peer_view && isLoopbackDashboard()} onClose=${() => navigateRoute('')} />
    </div>`)}
  </div>`;
}

export function mountGroupsRouteMap({ host, state, registerCleanup }) {
  render(html`<${GroupsRouteMap} state=${state} />`, host);
  registerCleanup(() => render(null, host));
}
