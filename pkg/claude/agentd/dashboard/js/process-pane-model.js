// process-pane-model.js -- the pure half of a process template open as a Collab
// strip tab (tcl-tnbtwp): the terminal shell's third pane kind, beside
// terminals and boards. Its seed names the template instead of a socket, and its
// key is derived from the id, so a template has one strip tab at most. DOM-free,
// so the terminal core and the standalone page can read a seed without the
// editor's module graph.

// A template id, by the store's rule: only a stored template leaves the
// Processes tab.
const TEMPLATE_ID = /^[a-z0-9][a-z0-9._-]{0,127}$/;

export const isProcessTemplateId = (id) => typeof id === 'string' && TEMPLATE_ID.test(id);

export const processPaneKey = (id) => `process:${id}`;

export function processPaneSeed(id, label = '') {
  return { process: id, key: processPaneKey(id), label: label || id };
}
