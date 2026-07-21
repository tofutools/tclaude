import test from 'node:test';
import assert from 'node:assert/strict';
import {
  composeSandboxProfilePolicy,
  composeSandboxProfilePreview,
} from '../dashboard/js/sandbox-profile-preview.js';

test('profiles without the TCL-609 fields compose exactly as before', () => {
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
  assert.equal(policy.readBaseline, null);
  assert.equal(
    composeSandboxProfilePreview([{ scope: 'global', profile: dev }], { base, dev }),
    policy.text,
  );
});

test('read baseline composes strictest-wins and names its originating profile', () => {
  const broad = { name: 'broad' };
  const strictBase = { name: 'strict-base', read_baseline: 'minimal' };
  const viaInclude = { name: 'via-include', includes: ['strict-base'] };
  const policy = composeSandboxProfilePolicy(
    [
      { scope: 'global', profile: broad },
      { scope: 'group', profile: viaInclude },
      { scope: 'explicit', profile: broad },
    ],
    { broad, 'strict-base': strictBase, 'via-include': viaInclude },
  );
  // Any minimal layer wins, a later default layer never widens it back, and
  // the include that introduced it stays attributed.
  assert.deepEqual(policy.readBaseline, { scope: 'group', profile: 'strict-base' });
  assert.match(policy.text, /read baseline: minimal — strict \(group:strict-base\)/);
});

test('an explicit minimal profile reports itself as the baseline origin', () => {
  const strict = { name: 'strict', read_baseline: 'minimal' };
  const policy = composeSandboxProfilePolicy([{ scope: 'explicit', profile: strict }], { strict });
  assert.deepEqual(policy.readBaseline, { scope: 'explicit', profile: 'strict' });
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
    read_baseline: 'minimal',
  };
  const policy = composeSandboxProfilePolicy(
    [{ scope: 'group', profile: wrapper }], { wrapper, debug },
  );
  assert.equal(policy.breakGlass.length, 1);
  assert.deepEqual(policy.readBaseline, { scope: 'group', profile: 'debug' });
  assert.match(policy.text, /⚠ unresolved includes: missing/);
});
