// Package ccworkflows is the read-only data layer for Claude Code's builtin
// "workflows" feature: it enumerates and parses workflow runs and saved
// workflow templates from disk, without executing any JavaScript and without
// any CLI or web dependencies, so the CLI / web / live-progress slices can layer
// cleanly on top.
//
// # On-disk layout (verified empirically; CC-internal, may drift across CC bumps)
//
// A workflow run is persisted under the transcript directory of the CC session
// that launched it — the same tree tclaude's conv package already walks:
//
//	<projectsRoot>/<projectEncoded>/<sessionUUID>/
//	  workflows/<runId>.json                       completed-run record (written at completion only)
//	  workflows/scripts/<name>-<runId>.js          resolved script snapshot (written at launch)
//	  subagents/workflows/<runId>/journal.jsonl    append-only live journal (the only live signal)
//	  subagents/workflows/<runId>/agent-<id>.jsonl per-agent transcript (standard CC format)
//
// projectsRoot is normally ~/.claude/projects. Saved templates live separately
// under ~/.claude/workflows/saved/<name>.js (and a project-local mirror). The
// run id has the form wf_<hex>; there is no global index, so enumeration is a
// glob across the session tree.
//
// # Two parse paths
//
//   - Completed runs are read from <runId>.json — authoritative and
//     self-contained (every agent's label, phase, state, and token usage), so no
//     script analysis is needed. This is always preferred when present.
//   - In-flight runs have no <runId>.json yet; they are reconstructed from the
//     journal (agent lifecycle keyed by agentId, with no phase/label/timestamp)
//     joined to the static spawn order parsed from the script snapshot. That
//     join is best-effort for data-dependent fan-out — see Agent.LabelConfident.
//
// The exported surface: ListSavedScripts, ListRuns, LoadRun, ParseCompletedRun,
// ParseJournal, ParseScriptMeta, and ParseSpawnOrder.
//
// # Known limitations (by design — the meta literal is contractually pure)
//
// The static meta parse relies on the Workflow contract that `meta` is a pure
// literal that is the first `meta = {` in the file. Malformed/contract-violating
// meta (a stray non-numeric numeric token, a non-string phase() argument, a
// 0-based or duplicated workflow_phase index, or a `meta = {` inside a comment
// preceding the real one) is not handled specially: the failure mode is a
// graceful fallback (empty meta — and in ListSavedScripts the script is still
// returned with its filename-derived Name), never a crash or silent corruption.
package ccworkflows
