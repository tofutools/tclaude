package ccworkflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureProjectsRoot = "testdata/projects"
	fixtureProjectDir   = "-Users-johkjo-fixture-proj"
	fixtureSessionID    = "11111111-1111-1111-1111-111111111111"
)

func fixtureSessionDir() string {
	return filepath.Join(fixtureProjectsRoot, fixtureProjectDir, fixtureSessionID)
}

// --- ParseScriptMeta -------------------------------------------------------

func TestParseScriptMeta(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		wantName    string
		wantDesc    string
		wantPhases  []MetaPhase
		wantErr     bool
		wantErrText string
	}{
		{
			name: "single-quoted with phases (real snapshot shape)",
			src: `export const meta = {
  name: 'ccwf-fixture-probe',
  description: 'Tiny throwaway',
  phases: [
    { title: 'Scout', detail: 'one probe' },
    { title: 'Fan', detail: 'two agents' },
  ],
}
phase('Scout')`,
			wantName: "ccwf-fixture-probe",
			wantDesc: "Tiny throwaway",
			wantPhases: []MetaPhase{
				{Title: "Scout", Detail: "one probe"},
				{Title: "Fan", Detail: "two agents"},
			},
		},
		{
			name:     "double-quoted, trailing commas, comments",
			src:      "export const meta = {\n  name: \"dq\",\n  // a comment\n  description: \"d\", /* block */\n  phases: [ { title: \"A\", detail: \"x\", }, ],\n}\n",
			wantName: "dq",
			wantDesc: "d",
			wantPhases: []MetaPhase{
				{Title: "A", Detail: "x"},
			},
		},
		{
			name:       "no phases",
			src:        "export const meta = { name: 'np', description: 'no phases' }\nawait agent('x')",
			wantName:   "np",
			wantDesc:   "no phases",
			wantPhases: nil,
		},
		{
			name:     "bare const without export",
			src:      "const meta = { name: 'bare' }",
			wantName: "bare",
		},
		{
			name:     "backtick string value",
			src:      "export const meta = { name: `bt`, description: `multi\nline` }",
			wantName: "bt",
			wantDesc: "multi\nline",
		},
		{
			name:     "unicode escape in description",
			src:      `export const meta = { name: 'u', description: 'aéb' }`,
			wantName: "u",
			wantDesc: "aéb",
		},
		{
			name:        "no meta assignment",
			src:         "phase('x')\nawait agent('y')",
			wantErr:     true,
			wantErrText: "no `meta",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScriptMeta(tt.src)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Name != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Description != tt.wantDesc {
				t.Errorf("description = %q, want %q", got.Description, tt.wantDesc)
			}
			if len(got.Phases) != len(tt.wantPhases) {
				t.Fatalf("phases = %+v, want %+v", got.Phases, tt.wantPhases)
			}
			for i := range tt.wantPhases {
				if got.Phases[i] != tt.wantPhases[i] {
					t.Errorf("phase[%d] = %+v, want %+v", i, got.Phases[i], tt.wantPhases[i])
				}
			}
		})
	}
}

func TestParseScriptMetaSurrogatePair(t *testing.T) {
	// 🚀 (U+1F680) written as a UTF-16 surrogate-pair escape 🚀 must
	// round-trip, not degrade to two U+FFFD replacement chars (review #1).
	// Double-quoted Go string so \\u becomes a literal \u in the JS source.
	src := "export const meta = { name: 'rocket', description: 'go \\uD83D\\uDE80 now' }"
	meta, err := ParseScriptMeta(src)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "go \U0001F680 now" {
		t.Errorf("description = %q, want the rocket emoji intact", meta.Description)
	}
}

func TestParseScriptMetaBadUnicodeEscapeIsReplacement(t *testing.T) {
	// An out-of-range \u{...} must not corrupt; it becomes U+FFFD.
	src := `export const meta = { name: 'x', description: 'a\u{110000}b' }`
	meta, err := ParseScriptMeta(src)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "a�b" {
		t.Errorf("description = %q, want a<replacement>b", meta.Description)
	}
}

func TestParseScriptMetaRealSnapshot(t *testing.T) {
	data, err := os.ReadFile("testdata/saved/ccwf-fixture-probe.js")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ParseScriptMeta(string(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if meta.Name != "ccwf-fixture-probe" {
		t.Errorf("name = %q", meta.Name)
	}
	if len(meta.Phases) != 2 || meta.Phases[0].Title != "Scout" || meta.Phases[1].Title != "Fan" {
		t.Errorf("phases = %+v", meta.Phases)
	}
}

// --- ListSavedScripts ------------------------------------------------------

func TestListSavedScripts(t *testing.T) {
	scripts, err := ListSavedScripts("testdata/saved")
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 3 {
		t.Fatalf("got %d scripts, want 3: %+v", len(scripts), scripts)
	}
	// Sorted by Name: ccwf-fixture-probe, double-quoted, no-phases.
	wantNames := []string{"ccwf-fixture-probe", "double-quoted", "no-phases"}
	for i, w := range wantNames {
		if scripts[i].Name != w {
			t.Errorf("scripts[%d].Name = %q, want %q", i, scripts[i].Name, w)
		}
		if scripts[i].Scope != "user" {
			t.Errorf("scripts[%d].Scope = %q, want user", i, scripts[i].Scope)
		}
	}
	// double-quoted meta.name is "dq-sample" (differs from filename) — proves
	// we parse meta, not just the filename.
	if scripts[1].Meta.Name != "dq-sample" {
		t.Errorf("double-quoted meta.name = %q, want dq-sample", scripts[1].Meta.Name)
	}
	if len(scripts[1].Meta.Phases) != 2 {
		t.Errorf("double-quoted phases = %+v", scripts[1].Meta.Phases)
	}
}

func TestListSavedScriptsMissingDirTolerated(t *testing.T) {
	scripts, err := ListSavedScripts("testdata/does-not-exist", "testdata/saved")
	if err != nil {
		t.Fatalf("missing dir should be tolerated, got: %v", err)
	}
	if len(scripts) != 3 {
		t.Fatalf("got %d, want 3", len(scripts))
	}
	// Second dir is scope "project".
	if scripts[0].Scope != "project" {
		t.Errorf("scope = %q, want project (second dir)", scripts[0].Scope)
	}
}

func TestListSavedScriptsScopeOrdering(t *testing.T) {
	// First dir => user scope.
	scripts, err := ListSavedScripts("testdata/saved")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		if s.Scope != "user" {
			t.Errorf("script %q scope = %q, want user", s.Name, s.Scope)
		}
	}
}

// --- ParseJournal ----------------------------------------------------------

func TestParseJournal(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantEvents int
		check      func(t *testing.T, evs []JournalEvent)
	}{
		{
			name:       "empty file",
			path:       "testdata/journals/empty.jsonl",
			wantEvents: 0,
		},
		{
			name:       "truncated tail skips the partial line",
			path:       "testdata/journals/truncated_tail.jsonl",
			wantEvents: 2,
			check: func(t *testing.T, evs []JournalEvent) {
				if evs[0].Type != EventStarted || evs[1].Type != EventResult {
					t.Errorf("events = %+v", evs)
				}
			},
		},
		{
			name:       "real simple run journal",
			path:       filepath.Join(fixtureSessionDir(), "subagents/workflows/wf_213c457c-3ac/journal.jsonl"),
			wantEvents: 6, // 3 started + 3 result
			check: func(t *testing.T, evs []JournalEvent) {
				if evs[0].Type != EventStarted || evs[0].AgentID == "" {
					t.Errorf("first event = %+v", evs[0])
				}
				if !strings.HasPrefix(evs[0].Key, "v2:") {
					t.Errorf("key = %q, want v2: prefix", evs[0].Key)
				}
			},
		},
		{
			name:       "live in-flight journal (one result missing)",
			path:       filepath.Join(fixtureSessionDir(), "subagents/workflows/wf_11ab22cd-e01/journal.jsonl"),
			wantEvents: 3, // 2 started + 1 result
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := os.Open(tt.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			evs, err := ParseJournal(f)
			if err != nil {
				t.Fatalf("ParseJournal: %v", err)
			}
			if len(evs) != tt.wantEvents {
				t.Fatalf("got %d events, want %d: %+v", len(evs), tt.wantEvents, evs)
			}
			if tt.check != nil {
				tt.check(t, evs)
			}
		})
	}
}

// --- ParseCompletedRun -----------------------------------------------------

func TestParseCompletedRunSimple(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureSessionDir(), "workflows/wf_213c457c-3ac.json"))
	if err != nil {
		t.Fatal(err)
	}
	rs, err := ParseCompletedRun(data)
	if err != nil {
		t.Fatal(err)
	}
	if rs.RunID != "wf_213c457c-3ac" {
		t.Errorf("runId = %q", rs.RunID)
	}
	if rs.Status != RunCompleted {
		t.Errorf("status = %q, want completed", rs.Status)
	}
	if rs.Source != "completed-json" {
		t.Errorf("source = %q", rs.Source)
	}
	if rs.AgentCount != 3 {
		t.Errorf("agentCount = %d, want 3", rs.AgentCount)
	}
	if len(rs.Phases) != 2 {
		t.Fatalf("phases = %+v, want 2", rs.Phases)
	}
	if rs.Phases[0].Title != "Scout" || rs.Phases[1].Title != "Fan" {
		t.Errorf("phase titles = %q, %q", rs.Phases[0].Title, rs.Phases[1].Title)
	}
	// Phase detail comes from meta phases by title match.
	if rs.Phases[0].Detail == "" {
		t.Errorf("phase[0] detail empty, want meta detail")
	}
	if rs.Phases[0].Status != AgentDone || rs.Phases[1].Status != AgentDone {
		t.Errorf("phase statuses = %q, %q, want done", rs.Phases[0].Status, rs.Phases[1].Status)
	}
	if len(rs.Agents) != 3 {
		t.Fatalf("agents = %d, want 3", len(rs.Agents))
	}
	for _, a := range rs.Agents {
		if a.State != AgentDone {
			t.Errorf("agent %s state = %q, want done", a.ID, a.State)
		}
		if !a.LabelConfident {
			t.Errorf("agent %s LabelConfident = false, want true for completed run", a.ID)
		}
		if a.Label == "" || a.PhaseTitle == "" {
			t.Errorf("agent %s missing label/phase: %+v", a.ID, a)
		}
	}
}

func TestParseCompletedRunFanoutOptionalFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureSessionDir(), "workflows/wf_0fa30e48-d43.json"))
	if err != nil {
		t.Fatal(err)
	}
	rs, err := ParseCompletedRun(data)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Status != RunCompleted {
		t.Errorf("status = %q", rs.Status)
	}
	if len(rs.Phases) != 2 {
		t.Errorf("phases = %d, want 2", len(rs.Phases))
	}
	// Research phase has 4 agents; Synthesize has 1.
	countByPhase := map[int]int{}
	var sawAgentType, sawLastTool bool
	for _, a := range rs.Agents {
		countByPhase[a.PhaseIndex]++
		if a.AgentType != "" {
			sawAgentType = true
		}
		if a.LastTool != "" {
			sawLastTool = true
		}
	}
	if countByPhase[1] != 4 || countByPhase[2] != 1 {
		t.Errorf("phase fan-out = %v, want {1:4, 2:1}", countByPhase)
	}
	if !sawAgentType {
		t.Errorf("expected at least one agentType set (optional field present in this run)")
	}
	if !sawLastTool {
		t.Errorf("expected at least one lastTool set")
	}
}

func TestParseCompletedRunFailed(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureSessionDir(), "workflows/wf_fa11ed00-f01.json"))
	if err != nil {
		t.Fatal(err)
	}
	rs, err := ParseCompletedRun(data)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Status != RunFailed {
		t.Errorf("status = %q, want failed", rs.Status)
	}
	var failed, done int
	for _, a := range rs.Agents {
		switch a.State {
		case AgentFailed:
			failed++
		case AgentDone:
			done++
		}
	}
	if failed != 1 || done != 1 {
		t.Errorf("agent states: failed=%d done=%d, want 1/1", failed, done)
	}
	// The phase aggregates to failed because one agent failed.
	if len(rs.Phases) != 1 || rs.Phases[0].Status != AgentFailed {
		t.Errorf("phase = %+v, want single failed phase", rs.Phases)
	}
}

// --- LoadRun ---------------------------------------------------------------

func TestLoadRunPrefersCompletedJSON(t *testing.T) {
	rs, err := LoadRun(fixtureSessionDir(), "wf_213c457c-3ac")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Source != "completed-json" {
		t.Errorf("source = %q, want completed-json", rs.Source)
	}
	if rs.Status != RunCompleted {
		t.Errorf("status = %q", rs.Status)
	}
}

func TestLoadRunLiveFromJournal(t *testing.T) {
	rs, err := LoadRun(fixtureSessionDir(), "wf_11ab22cd-e01")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Source != "journal" {
		t.Fatalf("source = %q, want journal", rs.Source)
	}
	if rs.Status != RunRunning {
		t.Errorf("status = %q, want running", rs.Status)
	}
	if rs.WorkflowName != "live-probe" {
		t.Errorf("workflowName = %q (from script meta)", rs.WorkflowName)
	}
	if len(rs.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(rs.Agents))
	}
	// First agent done (has result), second still running.
	if rs.Agents[0].State != AgentDone {
		t.Errorf("agent[0] state = %q, want done", rs.Agents[0].State)
	}
	if rs.Agents[1].State != AgentRunning {
		t.Errorf("agent[1] state = %q, want running", rs.Agents[1].State)
	}
	// Static script has 2 agent() calls, journal has 2 agents, not dynamic =>
	// labels are confident, recovered from spawn order.
	if !rs.Agents[0].LabelConfident {
		t.Errorf("agent[0] LabelConfident = false, want true (static 1:1 match)")
	}
	if rs.Agents[0].Label != "build:a" || rs.Agents[1].Label != "build:b" {
		t.Errorf("labels = %q, %q, want build:a/build:b", rs.Agents[0].Label, rs.Agents[1].Label)
	}
	if rs.Agents[0].PhaseTitle != "Build" {
		t.Errorf("phase = %q, want Build", rs.Agents[0].PhaseTitle)
	}
	if rs.ScriptPath == "" {
		t.Errorf("ScriptPath empty, want the live script snapshot path")
	}
}

func TestLoadRunNotFound(t *testing.T) {
	_, err := LoadRun(fixtureSessionDir(), "wf_does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing run")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want 'not found'", err.Error())
	}
}

// --- ListRuns --------------------------------------------------------------

func TestListRuns(t *testing.T) {
	refs, err := ListRuns(fixtureProjectsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 4 {
		t.Fatalf("got %d runs, want 4: %+v", len(refs), refs)
	}
	byID := map[string]RunRef{}
	for _, r := range refs {
		byID[r.RunID] = r
		if r.SessionID != fixtureSessionID {
			t.Errorf("run %s SessionID = %q, want %q", r.RunID, r.SessionID, fixtureSessionID)
		}
		if r.ProjectDir != fixtureProjectDir {
			t.Errorf("run %s ProjectDir = %q", r.RunID, r.ProjectDir)
		}
	}

	// Completed simple run: name + start time come from the completed JSON.
	if r := byID["wf_213c457c-3ac"]; !r.HasCompletedJSON || r.Status != RunCompleted || r.WorkflowName != "ccwf-fixture-probe" || r.StartTimeMs == 0 {
		t.Errorf("wf_213c457c-3ac = %+v", r)
	}
	// Failed synthetic run (no script snapshot) — name still comes from JSON.
	if r := byID["wf_fa11ed00-f01"]; !r.HasCompletedJSON || r.Status != RunFailed || r.WorkflowName != "failing-probe" {
		t.Errorf("wf_fa11ed00-f01 = %+v", r)
	}
	// Live in-flight run: journal only, no completed JSON => running.
	if r := byID["wf_11ab22cd-e01"]; r.HasCompletedJSON || r.Status != RunRunning || r.WorkflowName != "live-probe" {
		t.Errorf("wf_11ab22cd-e01 = %+v", r)
	}
}

func TestListRunsMissingRootTolerated(t *testing.T) {
	refs, err := ListRuns("testdata/no-such-root")
	if err != nil {
		t.Fatalf("missing root should be tolerated: %v", err)
	}
	if refs != nil {
		t.Errorf("want nil refs, got %+v", refs)
	}
}

// --- ParseSpawnOrder / dynamic detection ----------------------------------

func TestParseSpawnOrder(t *testing.T) {
	src := `export const meta = { name: 'x', phases: [{title:'P1'},{title:'P2'}] }
phase('P1')
const a = await agent('do A', { label: 'a:one', phase: 'P1' })
phase('P2')
const bs = await parallel([
  () => agent('do B', { label: 'b:two' }),
  () => agent('do C', { label: 'b:three', phase: 'Override' }),
])`
	calls := ParseSpawnOrder(src)
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3: %+v", len(calls), calls)
	}
	want := []AgentCall{
		{Label: "a:one", Phase: "P1"},
		{Label: "b:two", Phase: "P2"},        // inherits active phase
		{Label: "b:three", Phase: "Override"}, // explicit phase opt wins
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d] = %+v, want %+v", i, calls[i], w)
		}
	}
}

func TestParseSpawnOrderIgnoresAgentInStrings(t *testing.T) {
	// The word "agent(" inside a prompt string must not be mistaken for a call.
	src := `export const meta = { name: 'x' }
const a = await agent('tell me how agent(foo) works', { label: 'real' })`
	calls := ParseSpawnOrder(src)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].Label != "real" {
		t.Errorf("label = %q, want real", calls[0].Label)
	}
}

func TestScriptHasDynamicSpawns(t *testing.T) {
	tests := []struct {
		src  string
		want bool
	}{
		{"const a = await agent('x')", false},
		{"phase('p'); await agent('x', {label:'l'})", false},
		{"const r = await pipeline(items, s1, s2)", true},
		{"items.map(i => agent(i))", true},
		{"for (const x of xs) { await agent(x) }", true},
		{"while (n < 10) { await agent('x') }", true},
		// The marker words inside a PROMPT STRING must NOT trigger dynamic
		// (cold-review finding #7): this script spawns statically.
		{"const a = await agent('iterate with .map() over a for-loop', {label:'l'})", false},
		{"const a = await agent('use pipeline(x)', {label:'l'})", false},
	}
	for _, tt := range tests {
		if got := ScriptHasDynamicSpawns(tt.src); got != tt.want {
			t.Errorf("ScriptHasDynamicSpawns(%q) = %v, want %v", tt.src, got, tt.want)
		}
	}
}

// --- jsliteral edge cases --------------------------------------------------

func TestParseJSValueTypes(t *testing.T) {
	src := `{ a: 1, b: -2.5, c: true, d: false, e: null, f: 'str', g: [1, 2, 3,], h: { nested: 'y' }, }`
	v, _, err := parseJSValue(src, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a map: %T", v)
	}
	if m["a"].(float64) != 1 || m["b"].(float64) != -2.5 {
		t.Errorf("numbers: %v %v", m["a"], m["b"])
	}
	if m["c"].(bool) != true || m["d"].(bool) != false {
		t.Errorf("bools: %v %v", m["c"], m["d"])
	}
	if m["e"] != nil {
		t.Errorf("null = %v", m["e"])
	}
	if m["f"].(string) != "str" {
		t.Errorf("str = %v", m["f"])
	}
	if arr, ok := m["g"].([]any); !ok || len(arr) != 3 {
		t.Errorf("array = %v", m["g"])
	}
	if nested, ok := m["h"].(map[string]any); !ok || nested["nested"].(string) != "y" {
		t.Errorf("nested = %v", m["h"])
	}
}
