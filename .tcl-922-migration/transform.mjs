import fs from 'node:fs';
import path from 'node:path';

const [root, manifestPath, mode = '--check'] = process.argv.slice(2);
if (!root || !manifestPath || !['--check', '--apply'].includes(mode)) {
  throw new Error('usage: node transform.mjs <jstest-dir> <manifest.json> [--check|--apply]');
}

const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
const shapes = Object.groupBy(manifest, (entry) => entry.shape);
const expected = { 'same-node': 261, 'different-node': 9, 'empty-node-array': 1 };
for (const [shape, count] of Object.entries(expected)) {
  if ((shapes[shape] ?? []).length !== count) {
    throw new Error(`manifest ${shape} count ${(shapes[shape] ?? []).length}, want ${count}`);
  }
}
if (manifest.length !== 271) throw new Error(`manifest count ${manifest.length}, want 271`);

const identity = manifest.filter((entry) => entry.shape !== 'empty-node-array');
const byFile = Object.groupBy(identity, (entry) => entry.file);
let changedFiles = 0;
let changedCalls = 0;

for (const [file, entries] of Object.entries(byFile)) {
  const filename = path.join(root, file);
  const before = fs.readFileSync(filename, 'utf8');
  let after = before;
  const helpers = new Set();

  for (const entry of [...entries].sort((a, b) => b.start - a.start)) {
    const anchored = before.slice(entry.start, entry.end);
    if (anchored !== entry.call) {
      throw new Error(`${file}:${entry.line}: manifest anchor mismatch`);
    }
    const helper = entry.shape === 'same-node' ? 'assertSameNode' : 'assertDifferentNode';
    const replacement = entry.call.replace(/^assert\.(?:equal|notEqual)/, helper);
    if (replacement === entry.call) {
      throw new Error(`${file}:${entry.line}: call prefix did not transform`);
    }
    after = `${after.slice(0, entry.start)}${replacement}${after.slice(entry.end)}`;
    helpers.add(helper);
    changedCalls++;
  }

  const importPattern = /import \{ ([^}]+) \} from '\.\/assertions\.mjs';/;
  const existing = after.match(importPattern);
  if (existing) {
    const names = new Set(existing[1].split(',').map((name) => name.trim()));
    for (const helper of helpers) names.add(helper);
    after = after.replace(importPattern,
      `import { ${[...names].sort().join(', ')} } from './assertions.mjs';`);
  } else {
    const anchor = "import assert from 'node:assert/strict';";
    if (!after.includes(anchor)) throw new Error(`${file}: strict assert import anchor missing`);
    after = after.replace(anchor,
      `${anchor}\nimport { ${[...helpers].sort().join(', ')} } from './assertions.mjs';`);
  }

  if (after === before) throw new Error(`${file}: transform produced no change`);
  changedFiles++;
  if (mode === '--apply') fs.writeFileSync(filename, after);
}

if (changedCalls !== 270) throw new Error(`transformed ${changedCalls} calls, want 270`);
console.log(JSON.stringify({ mode, manifest: manifest.length, identityCalls: changedCalls,
  identityFiles: changedFiles, manualArraySites: manifest.length - changedCalls }));
