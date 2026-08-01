import assert from 'node:assert/strict';

export function assertAbsent(found, message) {
  assert.equal(
    found && `${found.localName}.${found.getAttribute('class') || ''}`,
    null,
    message,
  );
}
