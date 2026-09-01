// Unit tests for the command palette's ranking logic
// (dashboard/js/palette-score.js), run with Node's BUILT-IN test runner —
// `node --test`, asserting via `node:assert`. No bundler, no framework, no
// package.json, no node_modules: the test imports the same raw ES module
// the browser loads. The Go wrapper (dashboard_node_test.go) runs this
// under `go test ./...` and skips when node is absent.
//
// This file lives OUTSIDE dashboard/ on purpose, so `//go:embed dashboard`
// doesn't ship the test inside the agentd binary.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  scoreMatch, expandQuery, scoreCommand, rankCommands, SYNONYMS,
} from '../dashboard/js/palette-score.js';

// Fixture mirroring the real palette commands (label + keywords), in the
// SAME build order palette.js pushes them — so the stable-sort
// tie-break-by-build-order is exercised too. Verbs match the unified
// Hide/Focus presentation. Keywords carry BOTH the plain vocabulary AND the
// 🧙 wizard synonyms palette.js appends unconditionally (veil/reveal/summon/
// slumber/awaken/banish…), so the scorer sees exactly what ships.
const COMMANDS = [
  { label: 'Hide all windows', keywords: 'hide unfocus all windows declutter detach panic minimize veil conceal cloak shroud portal scrying vision familiars' },
  { label: 'Focus all windows', keywords: 'show all windows raise focus bring up reveal behold conjure portal scrying vision familiars' },
  { label: 'Pick windows to focus / hide…', keywords: 'windows subset choose select modal some reveal veil portals scrying familiars' },
  { label: 'Shut down all agents', keywords: 'shutdown shut down stop kill power off halt all agents global everything batch slumber sleep rest lull dormant quell still familiars' },
  { label: 'Power on all agents', keywords: 'power on start resume wake boot up all agents global everything batch awaken rouse stir revive kindle familiars' },
  { label: 'Create new group…', keywords: 'new group create make add team squad party form fellowship warband adventuring muster gather assemble guild' },
  { label: 'Import a group archive…', keywords: 'import group archive zip restore recreate upload unseal party archive familiar wards missives scrolls' },
  { label: 'Clean up agents and conversations…', keywords: 'clean up agents and conversations cleanup all categories unjoin retire delete reinstate tidy familiars and scrolls release banish dispel restore' },
  { label: 'Delete retired agents…', keywords: 'delete purge retired cleanup remove wipe agents dispel banished obliterate destroy erase vanquish incinerate familiars' },
  { label: 'Retire ungrouped agents…', keywords: 'retire demote cleanup remove tidy bulk ungrouped no group groupless loose solo orphan stray agents banish exile dismiss unbound loose unattached familiars' },
  { label: 'Spawn agent…', keywords: 'new agent create spawn launch start summon conjure invoke call forth familiar' },
  { label: 'Manage spawn profiles…', keywords: 'spawn profiles profile edit manage recipe recipes bundle preset presets defaults familiar patterns pattern weave inscribe grimoire loom blueprint' },
  { label: 'Manage group templates…', keywords: 'group templates template manage team blueprint blueprints roster deploy import library summoning circles circle party chalk trace cast unseal rite' },
  { label: 'Manage role library…', keywords: 'roles role library edit manage brief defaults permission permissions class library classes reviewer tester lead dev designer po party' },
  { label: 'Manage sandbox profiles…', keywords: 'sandbox profiles profile manage policy filesystem environment network launch security wards ward protections boundaries realm' },
  { label: 'Manage cross-harness spawns…', keywords: 'cross-harness spawns cross harness spawn manage policy matrix source target allow deny delegation cross-realm summons realm realms summon wards familiar' },
  { label: 'Manage inter-group links…', keywords: 'inter-group links inter group link manage communication directed edge message groups arcane channels channel parties missive whisper weave' },
  { label: 'Message group: alpha…', keywords: 'message send compose notify group broadcast multicast all members agents alpha missive inscribe party familiars circle' },
  { label: 'Switch to slop theme', keywords: 'toggle switch theme slop regular vegas casino mode appearance descend leave depart halls machine' },
  { label: 'Go to Groups', keywords: 'tab navigate go open groups scry peer gaze behold chamber vision' },
  { label: 'Hide group: alpha', keywords: 'hide unfocus group windows alpha veil conceal cloak portal scrying party' },
  { label: 'Focus group: alpha', keywords: 'focus show group windows alpha reveal behold conjure portal scrying party' },
  { label: 'Focus window: worker-7', keywords: 'focus show jump bring up window agent worker-7 reveal behold conjure portal scrying familiar' },
  { label: 'Hide window: worker-7', keywords: 'hide detach window agent worker-7 veil conceal cloak portal scrying familiar' },
  { label: 'Retire agent: worker-7', keywords: 'retire demote cleanup remove agent worker-7 banish exile dismiss familiar' },
];

// The wizard-mode twin: the SAME commands as they PRESENT under body.wizard —
// arcane labels, identical keywords. buildCommands' wiz() picks these labels in
// wizard mode, so a plain-word search (spawn / shutdown / retire / hide) must
// still land them via the SYNONYMS bridge. This fixture exercises that reverse
// direction (plain query → arcane label), the one that matters in wizard mode.
const WIZ_COMMANDS = [
  { label: 'Veil all familiars', keywords: 'hide unfocus all windows declutter detach panic minimize veil conceal cloak shroud portal scrying vision familiars' },
  { label: 'Slumber all familiars', keywords: 'shutdown shut down stop kill power off halt all agents global everything batch slumber sleep rest lull dormant quell still familiars' },
  { label: 'Form a party…', keywords: 'new group create make add team squad party form fellowship warband adventuring muster gather assemble guild' },
  { label: 'Unseal a party archive…', keywords: 'import group archive zip restore recreate upload unseal party archive familiar wards missives scrolls' },
  { label: 'Tidy familiars and scrolls…', keywords: 'clean up agents and conversations cleanup all categories unjoin retire delete reinstate tidy familiars and scrolls release banish dispel restore' },
  { label: 'Dispel banished familiars…', keywords: 'delete purge retired cleanup remove wipe agents dispel banished obliterate destroy erase vanquish incinerate familiars' },
  { label: 'Banish unbound familiars…', keywords: 'retire demote cleanup remove tidy bulk ungrouped no group groupless loose solo orphan stray agents banish exile dismiss unbound loose unattached familiars' },
  { label: 'Summon a familiar…', keywords: 'new agent create spawn launch start summon conjure invoke call forth familiar' },
  { label: 'Manage familiar patterns…', keywords: 'spawn profiles profile edit manage recipe recipes bundle preset presets defaults familiar patterns pattern weave inscribe grimoire loom blueprint' },
  { label: 'Manage summoning circles…', keywords: 'group templates template manage team blueprint blueprints roster deploy import library summoning circles circle party chalk trace cast unseal rite' },
  { label: 'Manage class library…', keywords: 'roles role library edit manage brief defaults permission permissions class library classes reviewer tester lead dev designer po party' },
  { label: 'Manage wards…', keywords: 'sandbox profiles profile manage policy filesystem environment network launch security wards ward protections boundaries realm' },
  { label: 'Manage cross-realm summons…', keywords: 'cross-harness spawns cross harness spawn manage policy matrix source target allow deny delegation cross-realm summons realm realms summon wards familiar' },
  { label: 'Manage arcane channels between parties…', keywords: 'inter-group links inter group link manage communication directed edge message groups arcane channels channel parties missive whisper weave' },
  { label: 'Send missive to party: alpha…', keywords: 'message send compose notify group broadcast multicast all members agents alpha missive inscribe party familiars circle' },
  { label: 'Banish familiar: worker-7', keywords: 'retire demote cleanup remove agent worker-7 banish exile dismiss familiar' },
];

const labels = (q) => rankCommands(COMMANDS, q).map((c) => c.label);
const top = (q) => labels(q)[0];
const wizTop = (q) => rankCommands(WIZ_COMMANDS, q).map((c) => c.label)[0];

test('empty / whitespace query returns the whole list in build order', () => {
  assert.deepEqual(labels(''), COMMANDS.map((c) => c.label));
  assert.deepEqual(labels('   '), COMMANDS.map((c) => c.label));
});

test('prefix beats mid-word: "focus all" → Focus all windows (not Unfocus)', () => {
  assert.equal(top('focus all'), 'Focus all windows');
  // Hide all (whose keyword carries "unfocus all") is still listed, lower.
  assert.ok(labels('focus all').includes('Hide all windows'));
});

test('synonym hide→unfocus: "hide all" → Hide all windows on top', () => {
  assert.equal(top('hide all'), 'Hide all windows');
});

test('synonym show→focus: "show all" → Focus all windows on top', () => {
  assert.equal(top('show all'), 'Focus all windows');
});

test('legacy term still works: "unfocus all" → Hide all windows', () => {
  // The label says "Hide" now, but "unfocus" must still find it (via the
  // keyword AND the unfocus→hide synonym), so old muscle memory isn't lost.
  assert.equal(top('unfocus all'), 'Hide all windows');
});

test('bare "hide" surfaces the hide commands', () => {
  const out = labels('hide');
  assert.ok(out.includes('Hide all windows'));
  assert.ok(out.includes('Hide group: alpha'));
  assert.ok(out.includes('Hide window: worker-7'));
});

test('bare "show" surfaces the focus commands via synonym', () => {
  const out = labels('show');
  assert.ok(out.includes('Focus all windows'));
  assert.ok(out.includes('Focus group: alpha'));
});

test('"theme" finds the theme toggle', () => {
  assert.equal(top('theme'), 'Switch to slop theme');
});

test('no match returns an empty list', () => {
  assert.deepEqual(rankCommands(COMMANDS, 'zzzzz'), []);
});

test('expandQuery maps synonyms bidirectionally', () => {
  assert.ok(expandQuery('hide all').includes('unfocus all'));
  assert.ok(expandQuery('show all').includes('focus all'));
  assert.ok(expandQuery('unfocus').includes('hide'));
  assert.ok(expandQuery('focus').includes('show'));
  // The typed query is always a variant itself.
  assert.ok(expandQuery('hide all').includes('hide all'));
  // Wizard verbs expand to their plain twins and vice versa.
  assert.ok(expandQuery('summon').includes('spawn'));
  assert.ok(expandQuery('slumber').includes('shutdown'));
  assert.ok(expandQuery('slumber').includes('stop'));
  assert.ok(expandQuery('awaken').includes('resume'));
  assert.ok(expandQuery('banish').includes('retire'));
  assert.ok(expandQuery('veil').includes('hide'));
  assert.ok(expandQuery('reveal').includes('focus'));
  assert.ok(expandQuery('hide').includes('veil'));
  // Profile ↔ familiar-pattern noun bridge.
  assert.ok(expandQuery('patterns').includes('profiles'));
  assert.ok(expandQuery('profiles').includes('patterns'));
  assert.ok(expandQuery('circles').includes('templates'));
  assert.ok(expandQuery('templates').includes('circles'));
  assert.ok(expandQuery('classes').includes('roles'));
  assert.ok(expandQuery('roles').includes('classes'));
  assert.ok(expandQuery('wards').includes('sandbox'));
  assert.ok(expandQuery('sandbox').includes('wards'));
  assert.ok(expandQuery('realms').includes('harnesses'));
  assert.ok(expandQuery('harnesses').includes('realms'));
  assert.ok(expandQuery('channels').includes('links'));
  assert.ok(expandQuery('links').includes('channels'));
  assert.ok(expandQuery('unseal').includes('import'));
  assert.ok(expandQuery('import').includes('unseal'));
  assert.ok(expandQuery('tidy').includes('cleanup'));
  assert.ok(expandQuery('cleanup').includes('tidy'));
  assert.ok(expandQuery('dispel').includes('delete'));
  assert.ok(expandQuery('delete').includes('dispel'));
  assert.ok(expandQuery('banished').includes('retired'));
  assert.ok(expandQuery('retired').includes('banished'));
  // Group ↔ party (the wizard label for "Create new group…" is "Form a party…").
  assert.ok(expandQuery('party').includes('group'));
  assert.ok(expandQuery('group').includes('party'));
  assert.ok(expandQuery('message').includes('missive'));
  assert.ok(expandQuery('missive').includes('message'));
});

test('SYNONYMS pairs are bidirectional', () => {
  assert.deepEqual(SYNONYMS.hide, ['unfocus', 'veil']);
  assert.deepEqual(SYNONYMS.unfocus, ['hide']);
  assert.deepEqual(SYNONYMS.veil, ['hide']);
  assert.deepEqual(SYNONYMS.show, ['focus', 'reveal']);
  assert.deepEqual(SYNONYMS.focus, ['show', 'reveal']);
  assert.deepEqual(SYNONYMS.reveal, ['focus', 'show']);
  // Wizard verb ↔ plain verb bridges.
  assert.deepEqual(SYNONYMS.spawn, ['summon']);
  assert.deepEqual(SYNONYMS.summon, ['spawn']);
  assert.deepEqual(SYNONYMS.shutdown, ['slumber']);
  assert.deepEqual(SYNONYMS.stop, ['slumber']);
  assert.deepEqual(SYNONYMS.slumber, ['shutdown', 'stop']);
  assert.deepEqual(SYNONYMS.resume, ['awaken']);
  assert.deepEqual(SYNONYMS.awaken, ['resume']);
  assert.deepEqual(SYNONYMS.retire, ['banish']);
  assert.deepEqual(SYNONYMS.banish, ['retire']);
  assert.deepEqual(SYNONYMS.profiles, ['patterns']);
  assert.deepEqual(SYNONYMS.patterns, ['profiles']);
  assert.deepEqual(SYNONYMS.templates, ['circles']);
  assert.deepEqual(SYNONYMS.circles, ['templates']);
  assert.deepEqual(SYNONYMS.roles, ['classes']);
  assert.deepEqual(SYNONYMS.classes, ['roles']);
  assert.deepEqual(SYNONYMS.sandbox, ['wards']);
  assert.deepEqual(SYNONYMS.wards, ['sandbox']);
  assert.deepEqual(SYNONYMS.harnesses, ['realms']);
  assert.deepEqual(SYNONYMS.realms, ['harnesses']);
  assert.deepEqual(SYNONYMS.links, ['channels']);
  assert.deepEqual(SYNONYMS.channels, ['links']);
  assert.deepEqual(SYNONYMS.import, ['unseal']);
  assert.deepEqual(SYNONYMS.unseal, ['import']);
  assert.deepEqual(SYNONYMS.cleanup, ['tidy', 'clean up']);
  assert.deepEqual(SYNONYMS.tidy, ['cleanup', 'clean up']);
  assert.deepEqual(SYNONYMS.delete, ['dispel']);
  assert.deepEqual(SYNONYMS.dispel, ['delete']);
  assert.deepEqual(SYNONYMS.retired, ['banished']);
  assert.deepEqual(SYNONYMS.banished, ['retired']);
  assert.deepEqual(SYNONYMS.party, ['group']);
  assert.deepEqual(SYNONYMS.group, ['party']);
  assert.deepEqual(SYNONYMS.message, ['missive']);
  assert.deepEqual(SYNONYMS.missive, ['message']);
});

// -- Wizard-theme synonyms: the arcane vocabulary must find the plain-labelled
//    commands, and (the direction that matters in wizard mode) the plain
//    vocabulary must find the arcane-labelled ones. --------------------------

test('wizard verb finds the plain command: "summon" → Spawn agent…', () => {
  assert.equal(top('summon'), 'Spawn agent…');
});

test('wizard verb finds the plain command: "slumber" → Shut down all agents', () => {
  assert.equal(top('slumber'), 'Shut down all agents');
});

test('wizard verb finds the plain command: "awaken" → Power on all agents', () => {
  assert.equal(top('awaken'), 'Power on all agents');
});

test('wizard verb finds plain retire commands', () => {
  assert.ok(labels('banish').includes('Retire agent: worker-7'));
});

test('wizard verb finds the plain command: "veil all" → Hide all windows', () => {
  assert.equal(top('veil all'), 'Hide all windows');
});

test('plain verb still finds the arcane label in wizard mode', () => {
  // The load-bearing direction: under body.wizard the LABELS are arcane, so
  // old muscle memory (spawn / shutdown / retire / hide) must still land them.
  assert.equal(wizTop('spawn'), 'Summon a familiar…');
  assert.equal(wizTop('shutdown'), 'Slumber all familiars');
  assert.ok(rankCommands(WIZ_COMMANDS, 'retire')
    .some((command) => command.label === 'Banish familiar: worker-7'));
  assert.equal(wizTop('hide'), 'Veil all familiars');
});

test('per-group message is reachable by plain and wizard vocabulary', () => {
  assert.equal(top('message alpha'), 'Message group: alpha…');
  assert.equal(top('missive alpha'), 'Message group: alpha…');
  assert.equal(wizTop('message alpha'), 'Send missive to party: alpha…');
  assert.equal(wizTop('missive alpha'), 'Send missive to party: alpha…');
});

test('create-group is reachable by both vocabularies in both themes', () => {
  // Plain theme (label "Create new group…"): the literal terms rank it,
  // and the wizard word "party" lands it via the party→group bridge.
  assert.equal(top('new group'), 'Create new group…');
  assert.equal(top('create group'), 'Create new group…');
  assert.equal(top('party'), 'Create new group…');
  // Wizard theme (label "Form a party…"): the arcane words rank it, and the
  // plain word "group" lands it via the group→party bridge — the load-bearing
  // direction, so old muscle memory ("new group") still finds it.
  assert.equal(wizTop('party'), 'Form a party…');
  assert.equal(wizTop('new group'), 'Form a party…');
});

test('global menu managers are reachable by regular and wizard nouns in either theme', () => {
  const pairs = [
    ['spawn profiles', 'familiar patterns', 'Manage spawn profiles…', 'Manage familiar patterns…'],
    ['group templates', 'summoning circles', 'Manage group templates…', 'Manage summoning circles…'],
    ['role library', 'class library', 'Manage role library…', 'Manage class library…'],
    ['sandbox profiles', 'wards', 'Manage sandbox profiles…', 'Manage wards…'],
    ['cross-harness spawns', 'cross-realm summons', 'Manage cross-harness spawns…', 'Manage cross-realm summons…'],
    ['inter-group links', 'arcane channels', 'Manage inter-group links…', 'Manage arcane channels between parties…'],
    ['group archive', 'party archive', 'Import a group archive…', 'Unseal a party archive…'],
    ['clean up agents and conversations', 'tidy familiars and scrolls', 'Clean up agents and conversations…', 'Tidy familiars and scrolls…'],
    ['delete retired', 'dispel banished', 'Delete retired agents…', 'Dispel banished familiars…'],
  ];
  for (const [regular, wizard, regularLabel, wizardLabel] of pairs) {
    assert.equal(top(regular), regularLabel, `${regular} should lead to its regular manager`);
    assert.equal(top(wizard), regularLabel, `${wizard} should find the regular-labelled manager`);
    assert.equal(wizTop(wizard), wizardLabel, `${wizard} should lead to its wizard manager`);
    assert.equal(wizTop(regular), wizardLabel, `${regular} should find the wizard-labelled manager`);
  }
});

test('broad cleanup vocabulary prefers the all-categories tool over narrower cleanup actions', () => {
  assert.equal(top('cleanup'), 'Clean up agents and conversations…');
  assert.equal(top('tidy'), 'Clean up agents and conversations…');
  assert.equal(wizTop('cleanup'), 'Tidy familiars and scrolls…');
  assert.equal(wizTop('tidy'), 'Tidy familiars and scrolls…');
});

test('scoreMatch ladder: exact > prefix > word-start > substring', () => {
  const label = 'focus all windows';
  const hay = label + ' x';
  const exact = scoreMatch('focus all windows', ['focus', 'all', 'windows'], label, hay);
  const prefix = scoreMatch('focus', ['focus'], label, hay);
  const wordStart = scoreMatch('all', ['all'], label, hay); // " all" at word boundary
  const none = scoreMatch('zzz', ['zzz'], label, hay);
  assert.ok(exact > prefix, 'exact beats prefix');
  assert.ok(prefix > wordStart, 'prefix beats word-start');
  assert.ok(wordStart > 0 && none === 0);
});

test('scoreCommand lifts a keyword-phrase hit above scattered tokens', () => {
  // "hide all" is only a contiguous phrase in the keywords here (label is
  // "Unfocus…"), which must beat a command that merely scatters the tokens.
  const phraseHit = scoreCommand('hide all', 'unfocus all windows',
    'unfocus all windows hide all windows detach');
  const scattered = scoreCommand('hide all', 'focus window: hotkey-for-hide-all-windows',
    'focus window: hotkey-for-hide-all-windows jump');
  assert.ok(phraseHit > scattered, `phrase ${phraseHit} should beat scattered ${scattered}`);
});
