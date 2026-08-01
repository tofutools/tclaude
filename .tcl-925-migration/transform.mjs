import fs from 'node:fs';

const [root, manifestPath, mode = '--check'] = process.argv.slice(2);
if (!root || !manifestPath || !['--check', '--apply', '--verify'].includes(mode)) {
  throw new Error('usage: node transform.mjs <repo-root> <manifest.json> [--check|--apply|--verify]');
}

const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
if (manifest.length !== 57) throw new Error(`manifest count ${manifest.length}, want 57`);
const agentd = manifest.filter((entry) => entry.file.startsWith('pkg/claude/agentd/'));
const existingQuiesce = manifest.filter((entry) => entry.existingQuiesce);
if (agentd.length !== 20) throw new Error(`agentd count ${agentd.length}, want 20`);
if (existingQuiesce.length !== 2) {
  throw new Error(`existing-quiesce count ${existingQuiesce.length}, want 2`);
}
const byFile = Object.groupBy(manifest, (entry) => entry.file);
let changedFiles = 0;
let changedSites = 0;
let helperSites = 0;
let resetOnlySites = 0;
let existingQuiesceSites = 0;

for (const [file, entries] of Object.entries(byFile)) {
  const filename = `${root}/${file}`;
  const before = fs.readFileSync(filename, 'utf8');
  const lines = before.split('\n');
  const ordered = [...entries].sort(mode === '--verify'
    ? (a, b) => a.line - b.line
    : (a, b) => b.line - a.line);
  for (const [entryIndex, entry] of ordered.entries()) {
    // Applying bottom-up preserves manifest line numbers. Verification walks
    // top-down and accounts for the one inserted line at each preceding site.
    const index = entry.line - 1 + (mode === '--verify' ? entryIndex : 0);
    if (lines[index] !== entry.anchor || entry.anchor.trim() !== 'db.ResetForTest()') {
      throw new Error(`${file}:${entry.line}: manifest anchor mismatch`);
    }
    const indent = entry.anchor.match(/^\s*/)[0];
    const isAgentd = entry.file.startsWith('pkg/claude/agentd/');
    const cleanup = isAgentd && !entry.existingQuiesce
      ? `${indent}cleanupAgentdTestDB(${entry.receiver})`
      : `${indent}${entry.receiver}.Cleanup(db.ResetForTest)`;
    if (mode === '--verify') {
      if (lines[index + 1]?.trim() !== cleanup.trim()) {
        throw new Error(`${file}:${entry.line}: transformed cleanup is absent`);
      }
    } else if (lines[index + 1]?.trim() === cleanup.trim()) {
      throw new Error(`${file}:${entry.line}: cleanup already present`);
    }
    if (entry.existingQuiesce) {
      const wait = `${entry.receiver}.Cleanup(bgWG.Wait)`;
      // One established reaper site carries the rationale comment between the
      // reset anchor and its cleanup. Bound the lookup to the local setup block.
      if (!lines.slice(index + 1, index + 12).some((line) => line.trim() === wait)) {
        throw new Error(`${file}:${entry.line}: declared existing quiesce is absent`);
      }
      existingQuiesceSites++;
    } else if (isAgentd) {
      helperSites++;
    } else {
      resetOnlySites++;
    }
    if (mode !== '--verify') lines.splice(index + 1, 0, cleanup);
    changedSites++;
  }
  const after = lines.join('\n');
  if (mode !== '--verify' && after === before) throw new Error(`${file}: transform produced no change`);
  changedFiles++;
  if (mode === '--apply') fs.writeFileSync(filename, after);
}

if (changedSites !== 57) throw new Error(`changed ${changedSites} sites, want 57`);
if (helperSites !== 18 || resetOnlySites !== 37 || existingQuiesceSites !== 2) {
  throw new Error(`unexpected action buckets: helper=${helperSites} resetOnly=${resetOnlySites} existingQuiesce=${existingQuiesceSites}`);
}
console.log(JSON.stringify({
  mode,
  manifest: manifest.length,
  changedSites,
  changedFiles,
  actions: { helperSites, resetOnlySites, existingQuiesceSites },
}));
