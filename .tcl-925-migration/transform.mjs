import fs from 'node:fs';

const [root, manifestPath, mode = '--check'] = process.argv.slice(2);
if (!root || !manifestPath || !['--check', '--apply'].includes(mode)) {
  throw new Error('usage: node transform.mjs <repo-root> <manifest.json> [--check|--apply]');
}

const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
if (manifest.length !== 57) throw new Error(`manifest count ${manifest.length}, want 57`);
const byFile = Object.groupBy(manifest, (entry) => entry.file);
let changedFiles = 0;
let changedSites = 0;

for (const [file, entries] of Object.entries(byFile)) {
  const filename = `${root}/${file}`;
  const before = fs.readFileSync(filename, 'utf8');
  const lines = before.split('\n');
  for (const entry of [...entries].sort((a, b) => b.line - a.line)) {
    const index = entry.line - 1;
    if (lines[index] !== entry.anchor || entry.anchor.trim() !== 'db.ResetForTest()') {
      throw new Error(`${file}:${entry.line}: manifest anchor mismatch`);
    }
    const cleanup = `${entry.anchor.match(/^\s*/)[0]}${entry.receiver}.Cleanup(db.ResetForTest)`;
    if (lines[index + 1]?.trim() === cleanup.trim()) {
      throw new Error(`${file}:${entry.line}: cleanup already present`);
    }
    lines.splice(index + 1, 0, cleanup);
    changedSites++;
  }
  const after = lines.join('\n');
  if (after === before) throw new Error(`${file}: transform produced no change`);
  changedFiles++;
  if (mode === '--apply') fs.writeFileSync(filename, after);
}

if (changedSites !== 57) throw new Error(`changed ${changedSites} sites, want 57`);
console.log(JSON.stringify({ mode, manifest: manifest.length, changedSites, changedFiles }));
