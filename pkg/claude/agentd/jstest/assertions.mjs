import assert from 'node:assert/strict';

function bounded(value, limit = 80) {
  const text = String(value ?? '');
  return text.length <= limit ? text : `${text.slice(0, limit)}…`;
}

function describeNode(value) {
  if (value === null) return 'null';
  if (value === undefined) return 'undefined';
  if (typeof value !== 'object') return `${typeof value}(${bounded(value)})`;

  const name = bounded(value.localName || value.nodeName || 'object');
  if (typeof value.getAttribute !== 'function') return name;
  const id = bounded(value.getAttribute('id'));
  const classes = bounded(value.getAttribute('class'));
  const dataKey = bounded(value.getAttribute('data-key'));
  return `${name}${id ? `#${id}` : ''}${classes ? `.${classes}` : ''}`
    + `${dataKey ? `[data-key=${JSON.stringify(dataKey)}]` : ''}`;
}

function diagnosticPrefix(message) {
  return message === undefined ? '' : `${message}\n`;
}

export function assertAbsent(found, message) {
  const label = found && `${found.localName}.${found.getAttribute('class') || ''}`;
  if (message === undefined) {
    assert.equal(label, null);
    return;
  }
  assert.equal(label, null, message);
}

export function assertSameNode(actual, expected, message) {
  if (actual === expected) return;
  assert.fail(`${diagnosticPrefix(message)}expected the same DOM node\n`
    + `actual: ${describeNode(actual)}\nexpected: ${describeNode(expected)}`);
}

export function assertDifferentNode(actual, expected, message) {
  if (actual !== expected) return;
  assert.fail(`${diagnosticPrefix(message)}expected different DOM nodes\n`
    + `both: ${describeNode(actual)}`);
}
