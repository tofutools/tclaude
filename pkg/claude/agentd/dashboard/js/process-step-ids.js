// process-step-ids.js -- a step's id follows its name until the process runs.
//
// A step's id is what `tclaude process decide --node`, the run views and
// agent messages name it by. Made from the kind's placeholder name, ids came
// out as new-step-2 or decide-2. While a process has never run, renaming a
// step moves its id to match the new name (slugged, made unique), and every
// reference to it moves too. Once it has run, ids stay put: runs name them.
//
// Pure functions over a process edit view ({template, edges, layout}).

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

// stepSlug is the id a name makes: the inline editor's own rule
// (process-mini-edit.js uniqueID), before it is made unique.
export function stepSlug(name) {
  return String(name || '').toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[^a-z0-9]+/, '').replace(/-+$/, '');
}

// performersOf lists the performers of a node that can name another step:
// the main one, a gate's deciders, and the plan, check and review stages.
function performersOf(node) {
  return [node.performer, ...(node.deciders || []), node.plan?.performer, node.review?.performer,
    ...(node.checks || []).map((check) => check?.performer)].filter(Boolean);
}

// rekeyStep moves step `id` to `next`, with every reference to it: the
// start, the edges, the layout pin and the sameAs / differentFrom of every
// performer. The nodes keep their order. Returns the new view.
export function rekeyStep(view, id, next) {
  const out = clone({ ...view, template: view.template || {}, edges: view.edges || [], layout: view.layout || {} });
  const nodes = out.template.nodes || {};
  if (!nodes[id] || id === next || nodes[next]) return view;
  out.template.nodes = Object.fromEntries(Object.entries(nodes).map(([key, node]) => [key === id ? next : key, node]));
  if (out.template.start === id) out.template.start = next;
  out.edges = out.edges.map((edge) => ({ ...edge, ...(edge.from === id ? { from: next } : {}), ...(edge.to === id ? { to: next } : {}) }));
  const pins = out.layout.nodes;
  if (pins && Object.hasOwn(pins, id)) {
    out.layout.nodes = Object.fromEntries(Object.entries(pins).map(([key, pin]) => [key === id ? next : key, pin]));
  }
  for (const node of Object.values(out.template.nodes)) {
    for (const performer of performersOf(node)) {
      if (performer.sameAs === id) performer.sameAs = next;
      if (Array.isArray(performer.differentFrom)) performer.differentFrom = performer.differentFrom.map((other) => (other === id ? next : other));
    }
  }
  return out;
}

// followName moves step `id` to the id its name makes, unique among the
// other nodes. Returns {view, id}: unchanged when the name makes no id, or
// the one it already has.
export function followName(view, id) {
  const node = view.template?.nodes?.[id];
  const stem = stepSlug(node?.name);
  if (!node || !stem) return { view, id };
  let next = stem;
  for (let n = 2; next !== id && view.template.nodes[next]; n += 1) next = `${stem}-${n}`;
  if (next === id) return { view, id };
  return { view: rekeyStep(view, id, next), id: next };
}
