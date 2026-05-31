# ccworkflows test fixtures

Captured/authored for JOH-55 (phase-2 CC builtin-workflows integration). The
on-disk layout these mirror was verified empirically — see the JOH-55 comment
"Step 0 done — run-storage layout VERIFIED on-wire".

## `projects/` — a realistic machine-wide tree for `ListRuns` / `LoadRun`

```
projects/<projectEncoded>/<sessionUUID>/
  workflows/<runId>.json                       completed-run record
  workflows/scripts/<name>-<runId>.js          resolved script snapshot (written at launch)
  subagents/workflows/<runId>/journal.jsonl    append-only live journal
  subagents/workflows/<runId>/agent-<id>.*     per-agent transcript + meta
```

One fixture session (`-Users-johkjo-fixture-proj/1111…1111`) holds four runs:

| runId | provenance | shape |
|---|---|---|
| `wf_213c457c-3ac` | **REAL** — a throwaway 1+2-agent run launched from this CC session to capture the layout authoritatively | completed, 2 phases (Scout → Fan), small/clean (results are just "alpha"/"bravo"/"charlie"). Exercises the *absence* of the optional `agentType`/`lastToolName` fields (no tools used). |
| `wf_0fa30e48-d43` | **REAL, trimmed** — a pre-existing research run; long strings clipped with a `…[trimmed]` marker, all keys/structure preserved | completed, 2 phases (Research ×4 fan-out → Synthesize), exercises the optional `agentType`/`lastToolName`/`lastToolSummary` fields. |
| `wf_11ab22cd-e01` | **SYNTHETIC** — hand-authored | in-flight: journal-only (no completed JSON), one agent `done`, one still `running`. Has a script snapshot for spawn-order correlation. |
| `wf_fa11ed00-f01` | **SYNTHETIC** — hand-authored, clearly marked in its `summary` | completed-shaped record with `status:"failed"` and one agent `state:"failed"`. No real failed run was captured (CC writes no `failed` journal event); this exercises failure-state parsing only. |

## `saved/` — saved-template fixtures for `ListSavedScripts` / `ParseScriptMeta`

- `ccwf-fixture-probe.js` — the **real** resolved script snapshot (same `export const meta = {…}` format as a saved template).
- `no-phases.js` — meta omits `phases` entirely.
- `double-quoted.js` — double-quoted strings, trailing commas, and `//` + `/* */` comments (all must be tolerated by the static parser, no JS engine).

## `journals/` — standalone journal edge cases for `ParseJournal`

- `empty.jsonl` — zero bytes.
- `truncated_tail.jsonl` — two valid events followed by a partial, interrupted final line (no trailing newline) — the in-flight tail tolerance.
