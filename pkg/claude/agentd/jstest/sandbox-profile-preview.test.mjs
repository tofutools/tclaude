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
    'global:dev · write /data (global) · env: GOFLAGS (global) · network: internet (global)',
  );
  assert.deepEqual(policy.breakGlass, []);
  assert.equal(
    composeSandboxProfilePreview([{ scope: 'global', profile: dev }], { base, dev }),
    policy.text,
  );
});

test('break-glass merges as a union with write dominating and origins never hidden by includes', () => {
  const debugBase = {
    name: 'debug-base',
    break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }],
  };
  const wrapper = {
    name: 'wrapper',
    includes: ['debug-base'],
    break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'write' }],
  };
  const policy = composeSandboxProfilePolicy(
    [{ scope: 'explicit', profile: wrapper }], { 'debug-base': debugBase, wrapper },
  );
  assert.deepEqual(policy.breakGlass, [{
    path: '/home/op/.tclaude/data',
    access: 'write',
    origins: ['explicit:debug-base', 'explicit:wrapper'],
  }]);
  assert.match(policy.text, /⚠ BREAK-GLASS protected access: write \/home\/op\/\.tclaude\/data \(explicit:debug-base, explicit:wrapper\)/);
  assert.match(policy.text, /exposes protected tclaude\/harness state/);
});

test('break-glass origins accumulate across assignment scopes', () => {
  const globalDebug = {
    name: 'global-debug',
    break_glass_filesystem: [{ path: '/home/op/.codex', access: 'read' }],
  };
  const groupDebug = {
    name: 'group-debug',
    break_glass_filesystem: [
      { path: '/home/op/.codex', access: 'read' },
      { path: '/home/op/.claude/sessions', access: 'write' },
    ],
  };
  const policy = composeSandboxProfilePolicy(
    [
      { scope: 'global', profile: globalDebug },
      { scope: 'group', profile: groupDebug },
    ],
    { 'global-debug': globalDebug, 'group-debug': groupDebug },
  );
  assert.deepEqual(policy.breakGlass, [
    { path: '/home/op/.codex', access: 'read', origins: ['global:global-debug', 'group:group-debug'] },
    { path: '/home/op/.claude/sessions', access: 'write', origins: ['group:group-debug'] },
  ]);
});

test('unresolved includes still surface while break-glass from resolvable ones survives', () => {
  const wrapper = {
    name: 'wrapper',
    includes: ['missing', 'debug'],
  };
  const debug = {
    name: 'debug',
    break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }],
  };
  const policy = composeSandboxProfilePolicy(
    [{ scope: 'group', profile: wrapper }], { wrapper, debug },
  );
  assert.equal(policy.breakGlass.length, 1);
  assert.match(policy.text, /⚠ unresolved includes: missing/);
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
    'explicit:workspace · deny /home/op (explicit) · read /home/op/go (explicit) · write /home/op/work (explicit)',
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
  assert.equal(policy.text, 'global:legacy · read /data (global)');
  assert.equal(policy.readBaseline, undefined);
  assert.equal(policy.readExclusions, undefined);
});
