// process-scope-ui.js -- subprocess drill-in chrome for the process editor
// (E1, tcl-6rek5d): the breadcrumb bar, the parent-graph minimap, the open
// scope's port panel, and the outline tree. The controller
// (process-editor.js) and the model's scopes (process-edit-model.js) own the
// state; these components only render snapshot.scope and call back.
//
// Node names and ids are template content: rendered as text only.

import { h } from 'preact';
import htm from 'htm';
import { NodeField, NodeSelect } from './process-node-fields.js';

const html = htm.bind(h);

const TYPE_GLYPH = { start: '○', end: '◎', task: '▭', decision: '◆', parallel: '⫴', wait: '⏱', subprocess: '▣' };

export function ScopeBar({ controller, view }) {
  const scope = view.scope;
  if (!scope?.depth) return null;
  const busy = view.pending.save || view.pending.externalDecision || view.pending.externalReload || view.diff?.open;
  return html`<div class="process-scope-bar" role="navigation" aria-label="Subprocess path" data-scope-bar>
    <ol class="pscope-trail">
      ${scope.trail.map((step, index) => {
        const current = index === scope.trail.length - 1;
        return html`<li key=${`${index}:${step.id}`}>
          ${index > 0 && html`<span class="pscope-sep" aria-hidden="true">›</span>`}
          ${current
            ? html`<span class="pscope-crumb is-current" aria-current="location">${index > 0 ? '▣ ' : ''}${step.label}</span>`
            : html`<button type="button" class="pscope-crumb" data-scope-depth=${index}
                onClick=${() => controller.goToScopeDepth(index)}>${index > 0 ? '▣ ' : ''}${step.label}</button>`}
        </li>`;
      })}
    </ol>
    <span class="spacer"></span>
    <button type="button" class="process-action" data-scope-action="ports" aria-pressed=${scope.portsOpen ? 'true' : 'false'}
      title="The subgraph's contract with its parent: in-port bindings and out-port mapping" onClick=${() => controller.toggleScopePorts()}>⇄ ports</button>
    <button type="button" class="process-action" data-scope-action="exit" disabled=${busy}
      title="Back to the parent graph (Esc, Alt+↑)" onClick=${() => controller.exitSubprocess(1)}>↑ exit subgraph</button>
    <button type="button" class="process-action" data-scope-action="extract" disabled=${busy}
      title="Save this subgraph as its own template and reference it from here" onClick=${() => controller.extractSubprocess()}>⇪ extract as template</button>
  </div>`;
}

function bounds(nodes) {
  const xs = nodes.flatMap((node) => [node.x - node.width / 2, node.x + node.width / 2]);
  const ys = nodes.flatMap((node) => [node.y - node.height / 2, node.y + node.height / 2]);
  const minX = Math.min(...xs); const minY = Math.min(...ys);
  return { x: minX - 20, y: minY - 20, width: Math.max(...xs) - minX + 40, height: Math.max(...ys) - minY + 40 };
}

export function ScopeMinimap({ controller, view }) {
  const scope = view.scope;
  const map = scope?.minimap;
  if (!scope?.depth || !map?.nodes?.length) return null;
  const box = bounds(map.nodes);
  const parent = scope.trail[scope.trail.length - 2]?.label || 'parent';
  return html`<button type="button" class="process-scope-minimap" data-scope-minimap
      title=${`Back to ${parent}`} aria-label=${`Parent graph ${parent}; activate to exit the subgraph`}
      onClick=${() => controller.exitSubprocess(1)}>
    <span class="pscope-minimap-label">parent · ${parent}</span>
    <svg viewBox=${`${box.x} ${box.y} ${box.width} ${box.height}`} preserveAspectRatio="xMidYMid meet" aria-hidden="true">
      ${map.edges.map((edge, index) => html`<polyline key=${index} class="pscope-mini-edge"
        points=${edge.points.map((point) => `${point.x},${point.y}`).join(' ')} />`)}
      ${map.nodes.map((node) => html`<rect key=${node.id} class=${`pscope-mini-node${node.current ? ' is-current' : ''}`}
        x=${node.x - node.width / 2} y=${node.y - node.height / 2} width=${node.width} height=${node.height}
        rx=${node.type === 'task' || node.type === 'subprocess' ? 10 : node.width / 2} />`)}
    </svg>
  </button>`;
}

export function ScopePorts({ controller, view }) {
  const scope = view.scope;
  const ports = scope?.ports;
  if (!scope?.portsOpen || !ports) return null;
  const readOnly = view.pending.save || view.diff?.open;
  return html`<aside class="process-scope-ports process-node-detail process-scroll-surface" aria-label="Subgraph ports" data-scope-ports>
    <div class="pni-head-row">
      <strong class="pscope-ports-title">⇄ Ports · ${scope.trail.at(-1).label}</strong>
      <span class="spacer"></span>
      <button type="button" class="pni-close" aria-label="Close ports" onClick=${() => controller.toggleScopePorts(false)}>✕</button>
    </div>
    <p class="pscope-ports-note">The contract with the parent graph. Body params are declared with params…; bind each from a literal or the parent's <code>{{ params.x }}</code>.</p>
    <span class="process-node-field-label">in-ports · bind body params</span>
    ${ports.params.length ? ports.params.map((param) => html`<${NodeField} key=${param} fieldKey=${`scope.inputs.${param}`}
        label=${`${param} ←`} placeholder=${ports.parentParams[0] ? `{{ params.${ports.parentParams[0]} }}` : 'literal value'}
        value=${ports.inputs[param] || ''} readOnly=${readOnly}
        apply=${(key, raw) => (controller.setScopeInput(param, raw) ? { accepted: true } : { blocked: true })} />`)
      : html`<p class="process-node-empty">The body declares no params.</p>`}
    <span class="process-node-field-label">out-ports · body end → parent outcome</span>
    ${ports.ends.length ? ports.ends.map((end) => html`<${NodeSelect} key=${end} fieldKey=${`scope.outputs.${end}`} label=${`${end} →`}
        options=${ports.outcomes} value=${ports.outputs[end] || ''} readOnly=${readOnly}
        blankLabel=${ports.outcomes.length === 1 ? `default (${ports.outcomes[0]})` : 'unmapped'}
        apply=${(key, raw) => controller.setScopeOutput(end, raw)} />`)
      : html`<p class="process-node-empty">The body has no end nodes yet.</p>`}
    ${!ports.outcomes.length && html`<p class="process-node-empty">The ▣ node has no outgoing edges in the parent graph yet.</p>`}
  </aside>`;
}

function OutlineItems({ controller, items, path, scopePath, depth }) {
  return html`<ul class="pscope-outline-list" role=${depth ? 'group' : 'tree'} aria-label=${depth ? undefined : 'Process outline'}>
    ${items.map((item) => {
      const inScope = path.join('/') === scopePath.join('/');
      return html`<li key=${item.id} role="treeitem" aria-expanded=${item.children ? 'true' : undefined}>
        <button type="button" class=${`pscope-outline-item is-${item.type}${inScope ? ' is-in-scope' : ''}`} data-outline-node=${[...path, item.id].join('/')}
          title=${item.ref ? `references template ${item.ref}` : item.id}
          onClick=${() => controller.revealOutlineNode(path, item.id)}>
          <span class="pscope-outline-glyph" aria-hidden="true">${TYPE_GLYPH[item.type] || '▭'}</span>${item.name}${item.ref ? html` <span class="pscope-outline-ref">→ ${item.ref}</span>` : ''}
        </button>
        ${item.children && html`<${OutlineItems} controller=${controller} items=${item.children} path=${[...path, item.id]} scopePath=${scopePath} depth=${depth + 1} />`}
      </li>`;
    })}
  </ul>`;
}

export function ScopeOutline({ controller, view }) {
  const outline = view.scope?.outline || [];
  if (!outline.some((item) => item.type === 'subprocess')) return null;
  const scopePath = view.scope.trail.slice(1).map((step) => step.id);
  return html`<div class="process-scope-outline" data-scope-outline>
    <div class="process-palette-section">Outline</div>
    <${OutlineItems} controller=${controller} items=${outline} path=${[]} scopePath=${scopePath} depth=${0} />
  </div>`;
}
