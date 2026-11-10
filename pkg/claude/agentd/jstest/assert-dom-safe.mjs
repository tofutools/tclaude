// A failing assert.equal on a DOM node formats its message with util.inspect,
// custom inspectors off and depth 1000. On a LinkeDOM node that walks the
// whole linked document, and on a dashboard-sized one it never finishes: the
// suite allocates until the machine runs out, and a CI runner dies without
// naming the test (tcl-ha2ux1). Hundreds of assertions compare a
// querySelector result with null, so the equality checks are guarded here,
// where every DOM suite gets them, rather than at each call site.
//
// When either side is a node, the comparison is made here and a failure names
// the node in a few characters. Everything else goes to the real assertion.

import assert from 'node:assert';

const isNode = (value) => value !== null && typeof value === 'object'
  && typeof value.nodeType === 'number' && 'ownerDocument' in value;

export function describeValue(value) {
  if (!isNode(value)) return typeof value === 'string' ? JSON.stringify(value) : String(value);
  const tag = String(value.localName || value.nodeName || 'node').toLowerCase();
  const id = typeof value.id === 'string' && value.id ? `#${value.id}` : '';
  const classes = typeof value.className === 'string' && value.className.trim()
    ? `.${value.className.trim().split(/\s+/).join('.')}` : '';
  return `<${tag}${id}${classes}>`.slice(0, 120);
}

const strict = (a, b) => Object.is(a, b);
// eslint-disable-next-line eqeqeq
const loose = (a, b) => a == b || (Number.isNaN(a) && Number.isNaN(b));

function guard(target, name, same, negate) {
  const original = target[name];
  if (original.domSafe) return;
  const checked = function (actual, expected, message) {
    if (!isNode(actual) && !isNode(expected)) return original.apply(this, arguments);
    if (same(actual, expected) !== negate) return;
    if (message instanceof Error) throw message;
    const shown = [describeValue(actual), describeValue(expected)];
    throw new assert.AssertionError({
      message: message ?? `${shown[0]} ${negate ? 'should not equal' : 'should equal'} ${shown[1]}`,
      actual: shown[0],
      expected: shown[1],
      operator: name,
      stackStartFn: checked,
    });
  };
  checked.domSafe = true;
  target[name] = checked;
}

guard(assert, 'equal', loose, false);
guard(assert, 'notEqual', loose, true);
for (const target of [assert, assert.strict]) {
  guard(target, 'strictEqual', strict, false);
  guard(target, 'notStrictEqual', strict, true);
}
guard(assert.strict, 'equal', strict, false);
guard(assert.strict, 'notEqual', strict, true);
