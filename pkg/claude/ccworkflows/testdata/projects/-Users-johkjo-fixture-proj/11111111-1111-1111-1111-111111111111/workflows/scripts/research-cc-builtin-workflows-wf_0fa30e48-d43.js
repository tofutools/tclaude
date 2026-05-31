export const meta = {
  name: 'research-cc-builtin-workflows',
  description: "Research Claude Code's builtin Workflow feature data model (saved scripts, run records/status, spawned agents, detection, triggering) — grounded in real wf_* artifacts on this machine — and synthesize a per-product-requirement feasibility assessment for a tclaude 'Workflows' tab",
  phases: [
    { title: 'Research', detail: '4 parallel streams: feature model / on-disk forensics / observability / triggering' },
    { title: 'Synthesize', detail: 'combine into a data-model map + per-requirement feasibility + recommended first slice' },
  ],
}

// Concrete artifacts this session already produced — ground the research in reality, not just docs.
const GROUND = `GROUND YOUR FINDINGS IN REAL ARTIFACTS ON THIS MACHINE (don't rely on docs alone — verify on disk):
- A real builtin-Workflow run happened THIS session: runId "wf_49a51cbf-094".
- Its persisted script: /Users/johkjo/.claude/projects/-Users-johkjo-git-tclaude-agent-workflows/bb3198c2-d2d3-455e-8d23-1bc8a8c189b7/workflows/scripts/rename-workflows-to-workgraphs-wf_49a51cbf-094.js
- Its subagents transcript dir: /Users/johkjo/.claude/projects/-Users-johkj
// …[body trimmed for fixture]…
