import test from 'node:test';
import assert from 'node:assert/strict';
import {
  composeSandboxProfilePolicy,
  composeSandboxProfilePreview,
} from '../dashboard/js/sandbox-profile-preview.js';

test('ordinary grants compose through the deny > write > read lattice', () => {
  const base = { name: 'base', filesystem: [{ path: '/data', access: 'read' }] };
  const dev = {
    name: 'dev',
    includes: ['base'],
    filesystem: [{ path: '/data', access: 'write' }],
    environment: [{ name: 'GOFLAGS', value: '-count=1' }],
    network_access: 'internet',
  };
  const policy = composeSandboxProfilePolicy(
    [{ scope: 'global', profile: dev }], { base, dev },
  );
  assert.equal(
    policy.text,
    'Composed sandbox-profile layers (global “dev”) · write /data (global)'
    + ' · env: GOFLAGS (global) · network: internet (global)',
  );
  assert.equal(
    composeSandboxProfilePreview([{ scope: 'global', profile: dev }], { base, dev }),
    policy.text,
  );
});

test('unresolved includes surface while rules from resolvable ones survive', () => {
  const wrapper = {
    name: 'wrapper',
    includes: ['missing', 'debug'],
  };
  const debug = {
    name: 'debug',
    filesystem: [{ path: '/home/op/work', access: 'read' }],
  };
  const policy = composeSandboxProfilePolicy(
    [{ scope: 'group', profile: wrapper }], { wrapper, debug },
  );
  assert.match(policy.text, /read \/home\/op\/work \(group\)/);
  assert.match(policy.text, /⚠ unresolved includes: missing/);
});

// TCL-791 removed break-glass. A profile that still carries the retired field
// must compose as if it were absent — the daemon refuses such a payload
// outright, so the preview's job is simply never to render protected access.
test('a retired break_glass_filesystem field composes to nothing', () => {
  const legacy = {
    name: 'legacy',
    filesystem: [{ path: '/home/op/work', access: 'write' }],
    break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'write' }],
  };
  const policy = composeSandboxProfilePolicy([{ scope: 'global', profile: legacy }], { legacy });
  assert.equal(policy.breakGlass, undefined);
  assert.doesNotMatch(policy.text, /BREAK-GLASS/i);
  assert.doesNotMatch(policy.text, /\.tclaude\/data/);
  assert.match(policy.text, /write \/home\/op\/work \(global\)/);
});

test('a deny row and its narrower reopens both survive composition as authored', () => {
  // Strictness is now expressed purely as table rows: a broad deny plus the
  // narrower read/write rows that reopen exactly what the agent needs. Only an
  // identical path folds through the lattice; overlapping ancestors and
  // descendants must both reach the daemon as authored.
  const hardened = {
    name: 'hardened',
    filesystem: [
      { path: '/home/op', access: 'deny' },
      { path: '/home/op/go', access: 'read' },
    ],
  };
  const workspace = {
    name: 'workspace',
    includes: ['hardened'],
    filesystem: [{ path: '/home/op/work', access: 'write' }],
  };
  const policy = composeSandboxProfilePolicy(
    [{ scope: 'explicit', profile: workspace }], { hardened, workspace },
  );
  assert.equal(
    policy.text,
    'Composed sandbox-profile layers (explicit “workspace”) · deny /home/op (explicit)'
    + ' · read /home/op/go (explicit) · write /home/op/work (explicit)',
  );
});

test('retired read_baseline/read_baseline_exclusions JSON is ignored, never rendered', () => {
  // Profiles saved before TCL-623 may still carry the old fields. They compose
  // as if absent — no error, and no claim of an enforcement that no longer
  // exists.
  const legacy = {
    name: 'legacy',
    filesystem: [{ path: '/data', access: 'read' }],
    read_baseline: 'minimal',
    read_baseline_exclusions: ['secrets.ssh', 'home.directory'],
  };
  const policy = composeSandboxProfilePolicy([{ scope: 'global', profile: legacy }], { legacy });
  assert.equal(policy.text,
    'Composed sandbox-profile layers (global “legacy”) · read /data (global)');
  assert.equal(policy.readBaseline, undefined);
  assert.equal(policy.readExclusions, undefined);
});
