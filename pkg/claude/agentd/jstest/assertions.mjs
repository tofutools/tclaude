import assert from 'node:assert/strict';

export function assertAbsent(found, message) {
  const label = found && `${found.localName}.${found.getAttribute('class') || ''}`;
  if (message === undefined) {
    assert.equal(label, null);
    return;
  }
  assert.equal(label, null, message);
}
