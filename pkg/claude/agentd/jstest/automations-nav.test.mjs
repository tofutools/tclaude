// automations-nav.test.mjs — the Automations anchor reopens the section the
// operator left Automations on (Jobs or Processes).

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  automationsNavTarget, clickNavExactly, noteAutomationsSection,
} from '../dashboard/js/automations-nav.js';

test('Automations reopens Processes after leaving from it, Jobs otherwise', () => {
  noteAutomationsSection('jobs');
  assert.equal(automationsNavTarget('jobs', 'groups', true), 'jobs');

  noteAutomationsSection('processes');
  noteAutomationsSection('groups'); // not an Automations section: ignored
  assert.equal(automationsNavTarget('jobs', 'groups', true), 'processes');
  // Processes off (or not offered on this view): Jobs.
  assert.equal(automationsNavTarget('jobs', 'groups', false), 'jobs');
  // Inside Automations the anchor keeps its own meaning.
  assert.equal(automationsNavTarget('jobs', 'processes', true), 'jobs');
  assert.equal(automationsNavTarget('jobs', 'jobs', true), 'jobs');
  // Other tabs are never redirected.
  assert.equal(automationsNavTarget('groups', 'jobs', true), 'groups');
  noteAutomationsSection('jobs');
});

test('clickNavExactly bypasses the redirect for precise locations', () => {
  noteAutomationsSection('processes');
  let seen;
  clickNavExactly({ click: () => { seen = automationsNavTarget('jobs', 'groups', true); } });
  assert.equal(seen, 'jobs');
  assert.equal(automationsNavTarget('jobs', 'groups', true), 'processes');
  noteAutomationsSection('jobs');
});
