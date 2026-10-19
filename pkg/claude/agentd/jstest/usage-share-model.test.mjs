// tcl-gke6id: the spawn dialog's "Usage from" choices and what a spawn
// billed to a peer's share sends.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const now = Date.parse('2026-10-19T12:00:00Z');
const forge = {
  peer: 'inst_forge', peer_name: 'forge', gateway: 'team-key', dialect: 'anthropic',
  models: ['claude-sonnet-5*', 'claude-opus-5-5'], max_usd: 20, spent_usd: 7.6,
  expires_at: '2026-10-24T12:00:00Z',
};
const lab = { peer: 'inst_lab', peer_name: 'lab', gateway: 'openai-key', dialect: 'openai', models: [] };
const spent = { peer: 'inst_lab', gateway: 'team-key', dialect: 'anthropic', max_usd: 10, spent_usd: 10, used_up: true };
const claude = { name: 'claude', display_name: 'Claude Code', model_proxy_dialect: 'anthropic' };

test('choices carry what is left, when it ends, and why one cannot be used', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/usage-share-model.js');
  const [f, l, s] = m.shareOptions([forge, lab, spent], claude, 'claude-sonnet-5-5', now);
  assert.equal(f.value, 'team-key@inst_forge');
  assert.equal(f.label, 'forge · team-key');
  assert.equal(f.detail, '$12.40 of $20.00 left · ends in 5 days');
  assert.equal(f.reason, '');
  assert.match(l.reason, /OpenAI models only: not for Claude Code/);
  assert.equal(l.detail, 'no cap');
  assert.match(s.reason, /used up \(\$10\.00 of \$10\.00\)/);
  assert.match(m.shareOptions([forge], claude, 'claude-haiku-5-5', now)[0].reason, /not for claude-haiku-5-5/);
  assert.equal(m.shareOptions([forge], claude, 'opus[1m]', now)[0].reason, '', 'an alias is the gateway\'s call');
  assert.equal(m.shareOptions([forge], claude, '', now)[0].reason, '');
  assert.equal(m.shareOptions([forge], claude, 'x', Date.parse('2026-10-25T00:00:00Z'))[0].reason, 'ended');
  assert.match(m.shareOptions([forge], { display_name: 'Pi' }, '', now)[0].reason, /Pi cannot use a model gateway/);
  assert.deepEqual(m.shareOptions(null, claude, '', now), []);
});

test('a spawn billed to a share names the gateway and grants the worker its scoped access', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/usage-share-model.js');
  assert.deepEqual(m.shareSpawnFields(forge, { 'message.direct': { effect: 'deny' } }), {
    model_proxy: 'team-key@inst_forge',
    permission_overrides: {
      'message.direct': { effect: 'deny' },
      'models.proxy': { effect: 'grant', scope: { peer: ['inst_forge'], http_proxy: ['team-key'] } },
    },
  });
  assert.deepEqual(m.shareSpawnFields(null), {});
  assert.match(m.shareHint(forge), /^forge pays for this agent's model calls through team-key\. Models: claude-sonnet-5\*, claude-opus-5-5\.$/);
});
