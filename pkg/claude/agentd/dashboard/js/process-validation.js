// process-validation.js -- live validation loop for the process template
// editor (TCL-299): after every edit-model mutation a debounce fires the
// current draft at POST /v1/process/validate and the diagnostics come back as
// inline badges on nodes/edges plus a collapsible issues panel. Never a
// separate lint step.
//
// Split of responsibilities:
//   - ValidationScheduler, mapDiagnostics, decorateGraph are PURE (no DOM, no
//     real timers/fetch baked in) so Node's test runner exercises the exact
//     shipped file (jstest/process-validation.test.mjs).
//   - LiveValidation owns scheduling + diagnostic state and publishes immutable
//     snapshots through the editor. Preact owns the issues panel DOM.
//
// Correctness rules from the ticket:
//   - Stale-response guard: every request carries a sequence number; a
//     response that is not the newest issued request is discarded, so an
//     out-of-order network reply can never paint older diagnostics over newer.
//   - A draft that cannot serialize (or that the server rejects as
//     unserializable) skips that validation round; the previous diagnostics
//     stay up and the loop never crashes.
//   - Badges are glyph-coded per severity (never color-only).

import { graphEdgeID } from './process-edit-model.js';

export const VALIDATION_DEBOUNCE_MS = 400;

export function severityGlyph(severity) {
  return severity === 'error' ? '✕' : '⚠';
}

// splitEdgeTarget parses the server's edge targetId ("from:outcome"). Node
// ids cannot contain ':' (model idPattern), so the first ':' is the
// separator; the outcome keeps any further colons verbatim.
export function splitEdgeTarget(targetId) {
  const at = String(targetId || '').indexOf(':');
  if (at < 0) return null;
  return { from: targetId.slice(0, at), outcome: targetId.slice(at + 1) };
}

// A diagnostic's stable editor identity is semantic, never its array index or
// display message. Duplicate identities are intentionally ambiguous: after a
// changed validation result we clear focus rather than guessing which row the
// human meant.
export function diagnosticIdentity(entry) {
  return JSON.stringify([
    String(entry?.code || ''),
    String(entry?.scope || 'template'),
    String(entry?.targetId || ''),
  ]);
}

export function resolveDiagnosticFocus(entries, { identity = '', ambiguous = false } = {}) {
  if (!identity || ambiguous) return -1;
  const matches = [];
  for (let index = 0; index < (entries || []).length; index++) {
    if (diagnosticIdentity(entries[index]) === identity) matches.push(index);
  }
  return matches.length === 1 ? matches[0] : -1;
}

// ValidationScheduler is the debounce + sequence-number core. `run` performs
// one validation round for a built payload and resolves to a diagnostics
// array, or null when the round must be skipped. Timers are injectable so
// tests drive the debounce deterministically.
export class ValidationScheduler {
  constructor({ run, onResult, delayMs = VALIDATION_DEBOUNCE_MS, timers } = {}) {
    this.run = run;
    this.onResult = typeof onResult === 'function' ? onResult : () => {};
    this.delayMs = delayMs;
    this.timers = timers || {
      set: (fn, ms) => setTimeout(fn, ms),
      clear: (handle) => clearTimeout(handle),
    };
    this.seq = 0;
    this.timer = null;
    this.destroyed = false;
  }

  // schedule (re)arms the debounce; only the last call's payload builder runs.
  schedule(buildPayload) {
    if (this.destroyed) return;
    if (this.timer !== null) this.timers.clear(this.timer);
    this.timer = this.timers.set(() => {
      this.timer = null;
      this.fire(buildPayload);
    }, this.delayMs);
  }

  async fire(buildPayload) {
    if (this.destroyed) return;
    let payload = null;
    try {
      payload = buildPayload();
    } catch {
      payload = null;
    }
    // An unserializable intermediate draft skips this round (ticket rule).
    if (payload == null) return;
    const seq = ++this.seq;
    let diagnostics = null;
    try {
      diagnostics = await this.run(payload);
    } catch {
      return;
    }
    // Discard stale responses: a newer request was issued while this one was
    // in flight, so this result may not overwrite the newer one.
    if (this.destroyed || seq !== this.seq) return;
    if (diagnostics == null) return;
    this.onResult(diagnostics);
  }

  flush(buildPayload) {
    if (this.destroyed) return false;
    if (this.timer !== null) this.timers.clear(this.timer);
    this.timer = null;
    void this.fire(buildPayload);
    return true;
  }

  destroy() {
    this.destroyed = true;
    if (this.timer !== null) this.timers.clear(this.timer);
    this.timer = null;
    // Any in-flight response fails the seq check and is dropped.
    this.seq += 1;
  }
}

// mapDiagnostics resolves server diagnostics against the CURRENT edit model:
// entries whose node/edge still exists anchor badges there; an edge whose
// edge is gone falls back to its source node; anything else stays a
// template-scope panel entry. Diagnostics may be one edit behind the model
// (debounce + network), so dangling targets are expected, not an error.
export function mapDiagnostics(diagnostics, model) {
  const nodes = new Map();
  const edges = new Map();
  const entries = [];
  let errorCount = 0;
  let warningCount = 0;
  const bump = (map, key, severity) => {
    const hit = map.get(key) || { error: 0, warning: 0 };
    hit[severity] += 1;
    map.set(key, hit);
  };
  for (const diag of diagnostics || []) {
    const severity = diag.severity === 'error' ? 'error' : 'warning';
    if (severity === 'error') errorCount += 1;
    else warningCount += 1;
    const entry = {
      scope: 'template',
      severity,
      code: String(diag.code || ''),
      message: String(diag.message || ''),
      targetId: String(diag.targetId || ''),
    };
    if (diag.scope === 'edge') {
      const target = splitEdgeTarget(entry.targetId);
      if (target && model?.findEdge?.(target.from, target.outcome)) {
        entry.scope = 'edge';
        entry.edge = target;
        bump(edges, graphEdgeID(target.from, target.outcome), severity);
      } else if (target && model?.node?.(target.from)) {
        entry.scope = 'node';
        entry.node = target.from;
        bump(nodes, target.from, severity);
      }
    } else if (diag.scope === 'node' && model?.node?.(entry.targetId)) {
      entry.scope = 'node';
      entry.node = entry.targetId;
      bump(nodes, entry.targetId, severity);
    }
    entries.push(entry);
  }
  // Errors first; then a stable target/code order so panel rebuilds do not
  // shuffle rows between rounds.
  entries.sort((a, b) => {
    if (a.severity !== b.severity) return a.severity === 'error' ? -1 : 1;
    return `${a.targetId}\x00${a.code}`.localeCompare(`${b.targetId}\x00${b.code}`, 'en');
  });
  return { nodes, edges, entries, errorCount, warningCount };
}

// decorateGraph merges mapped diagnostics onto a model.graph() projection:
// node badges use the graph core's overlay anchor (glyph + severity class +
// count), edge badges ride the edge label anchor. Existing overlay fields are
// preserved so a future run view can combine state and validation.
export function decorateGraph(graph, mapped) {
  if (!mapped) return graph;
  for (const node of graph.nodes || []) {
    const hit = mapped.nodes.get(node.id);
    if (!hit) continue;
    const severity = hit.error > 0 ? 'error' : 'warning';
    const count = hit.error + hit.warning;
    const issues = mapped.entries
      .filter((entry) => entry.scope === 'node' && entry.node === node.id)
      .map((entry) => `${entry.code}: ${entry.message}`);
    node.overlay = { ...node.overlay, glyph: severityGlyph(severity), severity, issues };
    // Only claim the badge slot when there is a count to show; a single
    // diagnostic must not blank a badge some other decorator already set.
    if (count > 1) node.overlay.badge = `×${count}`;
  }
  for (const edge of graph.edges || []) {
    const hit = mapped.edges.get(edge.id);
    if (!hit) continue;
    const severity = hit.error > 0 ? 'error' : 'warning';
    const issues = mapped.entries
      .filter((entry) => entry.scope === 'edge' && entry.edge
        && graphEdgeID(entry.edge.from, entry.edge.outcome) === edge.id)
      .map((entry) => `${entry.code}: ${entry.message}`);
    edge.badge = severityGlyph(severity);
    edge.badgeSeverity = severity;
    edge.issues = issues;
  }
  return graph;
}

// LiveValidation wires the loop into one ProcessTemplateEditor. It repaints
// graph badges by re-setting the decorated graph and exposes panelSnapshot()
// for the Preact shell; it never reads or writes component DOM.
export class LiveValidation {
  constructor(editor, { delayMs, fetchFn } = {}) {
    this.editor = editor;
    this.fetchFn = fetchFn || ((url, options) => fetch(url, options));
    this.diagnostics = editor.model.diagnostics || [];
    this.mapped = null;
    this.panelSignature = '';
    this.panel = { open: false };
    this.focusRequest = 0;
    this.issueCursor = -1;
    this.focusedIssueIdentity = '';
    this.focusedIssueAmbiguous = false;
    this.scheduler = new ValidationScheduler({
      run: (payload) => this.post(payload),
      onResult: (diagnostics) => this.applyDiagnostics(diagnostics),
      delayMs,
    });
    // The edit view ships the stored version's diagnostics: paint them
    // immediately, then confirm against the live draft.
    this.applyDiagnostics(this.diagnostics);
    this.schedule();
  }

  schedule() {
    this.scheduler.schedule(() => this.payload());
  }

  validateNow() {
    return this.scheduler.flush(() => this.payload());
  }

  payload() {
    try {
      return JSON.stringify(this.editor.model.saveBody());
    } catch {
      return null;
    }
  }

  async post(body) {
    const response = await this.fetchFn('/v1/process/validate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
    });
    // 422 = the draft cannot serialize server-side; skip the round and keep
    // the previous diagnostics (never crash the loop on intermediate states).
    if (!response.ok) return null;
    const out = await response.json().catch(() => null);
    return out ? out.diagnostics || [] : null;
  }

  // applyDiagnostics is the response path: remap, repaint badges, refresh the
  // panel. decorate() is the edit path: refresh() hands the fresh graph
  // through here so badges survive re-renders and drop with deleted targets.
  applyDiagnostics(diagnostics) {
    this.diagnostics = diagnostics || [];
    this.editor.graph.setGraph(this.decorate(this.editor.model.graph()));
    this.editor?.publish?.();
  }

  decorate(graph) {
    this.mapped = mapDiagnostics(this.diagnostics, this.editor.model);
    this.renderPanel();
    return decorateGraph(graph, this.mapped);
  }

  // focusEntry selects the diagnostic's target and centers the canvas on it.
  focusEntry(entry) {
    const layout = this.editor.graph.layoutSnapshot();
    if (entry.scope === 'node' && entry.node) {
      this.editor.setSelection({ type: 'node', id: entry.node });
      const laid = layout.nodes.find((node) => node.id === entry.node);
      if (laid) this.editor.graph.centerOn(laid.x, laid.y);
    } else if (entry.scope === 'edge' && entry.edge) {
      this.editor.setSelection({ type: 'edge', from: entry.edge.from, outcome: entry.edge.outcome });
      // Layout edges keep the input's from/outcome fields (spread-through),
      // which sidesteps the layout's own id-minting scheme entirely.
      const laid = layout.edges.find((edge) => edge.outcome === entry.edge.outcome && edge.from === entry.edge.from);
      const anchor = laid?.label || layout.nodes.find((node) => node.id === entry.edge.from);
      if (anchor) this.editor.graph.centerOn(anchor.x, anchor.y);
    }
  }

  focusIssue(delta = 1) {
    const entries = this.mapped?.entries || [];
    if (!entries.length) return false;
    const index = this.issueCursor < 0
      ? (delta < 0 ? entries.length - 1 : 0)
      : (this.issueCursor + delta + entries.length) % entries.length;
    return this.focusIssueAt(index);
  }

  currentIssue() {
    if (this.focusedIssueAmbiguous) return null;
    const entries = this.mapped?.entries || [];
    const entry = this.issueCursor >= 0 && this.issueCursor < entries.length ? entries[this.issueCursor] : null;
    return entry && this.focusedIssueIdentity
      && diagnosticIdentity(entry) === this.focusedIssueIdentity ? entry : null;
  }

  focusIssueAt(index, { focusButton = true } = {}) {
    const entries = this.mapped?.entries || [];
    if (!Number.isInteger(index) || index < 0 || index >= entries.length) return false;
    this.issueCursor = index;
    this.focusedIssueIdentity = diagnosticIdentity(entries[index]);
    this.focusedIssueAmbiguous = entries.filter((entry) => diagnosticIdentity(entry) === this.focusedIssueIdentity).length > 1;
    this.panel.open = true;
    if (focusButton) this.focusRequest += 1;
    this.focusEntry(entries[index]);
    this.editor?.publish?.();
    return true;
  }

  renderPanel() {
    const { entries, errorCount, warningCount } = this.mapped;
    const signature = JSON.stringify(entries);
    // Rebuilding on every poll-ish repaint would drop focus mid-keyboarding;
    // only rebuild when the content actually changed.
    if (signature === this.panelSignature) return;
    this.panelSignature = signature;
    this.issueCursor = resolveDiagnosticFocus(entries, {
      identity: this.focusedIssueIdentity, ambiguous: this.focusedIssueAmbiguous,
    });
    if (this.issueCursor < 0) {
      this.focusedIssueIdentity = '';
      this.focusedIssueAmbiguous = false;
    }
    const bits = [];
    if (errorCount) bits.push(`${errorCount} error${errorCount === 1 ? '' : 's'}`);
    if (warningCount) bits.push(`${warningCount} warning${warningCount === 1 ? '' : 's'}`);
    this.summaryText = `Issues · ${bits.join(' · ') || 'none'}`;
    this.editor.publish?.();
  }

  setPanelOpen(open) {
    this.panel.open = !!open;
    this.editor.publish?.();
  }

  panelSnapshot() {
    return {
      open: !!this.panel.open,
      hidden: !(this.mapped?.entries?.length),
      summary: this.summaryText || 'Issues · none',
      entries: structuredClone(this.mapped?.entries || []),
      issueCursor: this.issueCursor,
      focusRequest: this.focusRequest,
    };
  }

  destroy() {
    this.scheduler.destroy();
  }
}
