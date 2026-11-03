// A node:test reporter that records each suite file's wall time.
//
// node --test runs every file in its own process and reports one
// `test:summary` per file whose duration_ms spans that process: imports,
// tests, and any tail a pending timer adds before the process exits. Those
// per-file times are what the jstest shard's wall time is made of, so CI
// writes them to a file (TCLAUDE_JS_TEST_TIMINGS, see dashboard_node_test.go)
// and prints the slowest ones (.github/actions/test-suite).
//
// Output: one tab-separated line per file, slowest first:
//   <seconds>\t<tests>\t<file>
// followed by a `# total` line: the summed file seconds and the run's wall
// seconds.
import { basename } from 'node:path';

export default async function* fileTimings(source) {
  const files = [];
  let wallMs = 0;
  for await (const event of source) {
    if (event.type !== 'test:summary') continue;
    const { file, duration_ms: ms, counts } = event.data;
    if (file) files.push({ file: basename(file), ms, tests: counts?.tests ?? 0 });
    else wallMs = Math.max(wallMs, ms);
  }
  files.sort((a, b) => b.ms - a.ms);
  for (const { file, ms, tests } of files) yield `${(ms / 1000).toFixed(1)}\t${tests}\t${file}\n`;
  const sumMs = files.reduce((sum, { ms }) => sum + ms, 0);
  yield `# total\t${files.length} files\t${(sumMs / 1000).toFixed(1)}s summed\t${(wallMs / 1000).toFixed(1)}s wall\n`;
}
