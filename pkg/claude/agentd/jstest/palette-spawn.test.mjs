import test from 'node:test';
import assert from 'node:assert/strict';
import { buildProfileSpawnCommands } from '../dashboard/js/palette-spawn.js';
import { rankCommands } from '../dashboard/js/palette-score.js';

const snapshot = {
  profiles: [
    { name: 'gpt-luna-high', aliases: ['moon'], harness: 'codex', fast_mode: false },
    { name: 'luna-claude', harness: 'claude' },
    { name: 'luna-unknown', harness: 'missing' },
    { name: 'ready', harness: 'codex', fast_mode: true },
  ],
  harnesses: [{ name: 'codex', can_fast_mode: true }, { name: 'claude', can_fast_mode: false }],
};

test('profile shortcuts match partial names, aliases and fast capability in both themes', () => {
  for (const wizard of [false, true]) {
    const calls = [];
    const commands = buildProfileSpawnCommands(snapshot, {
      defaultGroup: 'team', wiz: (regular, arcane) => wizard ? arcane : regular,
      openSpawn: (options) => calls.push(options),
    });
    assert.equal(rankCommands(commands, 'spawn lun').length, 4);
    const fast = rankCommands(commands, 'spawn luna fast');
    assert.equal(fast.length, 1);
    fast[0].run();
    assert.deepEqual(calls.pop(), { profileName: 'gpt-luna-high', defaultGroup: 'team', fastMode: true });
    rankCommands(commands, 'spawn moon')[0].run();
    assert.deepEqual(calls.pop(), { profileName: 'gpt-luna-high', defaultGroup: 'team' });
    assert.equal(rankCommands(commands, 'spawn ready fast').length, 1, 'saved fast profiles have no duplicate');
  }
  assert.deepEqual(buildProfileSpawnCommands({}, { openSpawn() {} }), []);
});
