import test from 'node:test';
import assert from 'node:assert/strict';
import { buildResourceCreateCommands } from '../dashboard/js/palette-resources.js';
import { rankCommands } from '../dashboard/js/palette-score.js';

const groups = [{ name: 'tclaude' }, { name: 'awb' }];

function build(wizard, processes = true) {
  const made = [];
  const touched = [];
  const make = {
    board: (g) => made.push(['board', g.name]),
    page: (g) => made.push(['page', g.name]),
    process: (g) => made.push(['process', g.name]),
  };
  const commands = buildResourceCreateCommands(groups, {
    wiz: (regular, arcane) => (wizard ? arcane : regular), processes, make, touch: (g) => touched.push(g),
  });
  return { commands, made, touched };
}

test('a phrased query makes the named resource in the named group, in either theme', () => {
  for (const wizard of [false, true]) {
    const cases = [
      ['open drawing board in tclaude', ['board', 'tclaude']],
      ['drawing board tclaude', ['board', 'tclaude']],
      ['whiteboard awb', ['board', 'awb']],
      ['open a whiteboard in tclaude', ['board', 'tclaude']],
      ['slate tclaude', ['board', 'tclaude']],
      ['new page awb', ['page', 'awb']],
      ['looking glass tclaude', ['page', 'tclaude']],
      ['start process in awb', ['process', 'awb']],
      ['rite tclaude', ['process', 'tclaude']],
    ];
    for (const [query, want] of cases) {
      const { commands, made, touched } = build(wizard);
      const ranked = rankCommands(commands, query);
      assert.ok(ranked.length, `${query} (wizard ${wizard}) finds a command`);
      ranked[0].run();
      assert.deepEqual(made, [want], `${query} (wizard ${wizard})`);
      assert.deepEqual(touched, [want[1]]);
    }
  }
});

test('the kind narrows: a drawing board query offers no page or process', () => {
  const { commands } = build(false);
  const labels = rankCommands(commands, 'drawing board').map((c) => c.label);
  assert.deepEqual(labels, ['New drawing board in tclaude', 'New drawing board in awb']);
});

test('no process command where the node offers no processes', () => {
  const { commands } = build(false, false);
  assert.equal(rankCommands(commands, 'process tclaude').length, 0);
  assert.equal(commands.length, 4);
});
