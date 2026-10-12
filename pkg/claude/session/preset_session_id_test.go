package session

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestValidatePresetSessionIDPerHarness pins the accept/refuse behaviour and
// the exact messages of `session new --session-id` for every harness that
// existed when the name switch was lifted into PresetSessionID.
func TestValidatePresetSessionIDPerHarness(t *testing.T) {
	const uuid = "0b9d9a6e-3c8c-4f43-9d0c-2b8b6f1e7a10"
	cases := []struct {
		harness string
		id      string
		resume  string
		managed bool
		wantErr string
	}{
		{harness: harness.DefaultName, id: ""},
		{harness: harness.DefaultName, id: uuid},
		{harness: harness.DefaultName, id: "not-a-uuid", wantErr: `--session-id must be a valid UUID, got "not-a-uuid"`},
		{harness: harness.DefaultName, id: uuid, resume: "abc", wantErr: "--session-id cannot be combined with --resume"},
		{harness: harness.CopilotName, id: uuid},
		{harness: harness.CopilotName, id: "ses_1", wantErr: `--session-id must be a valid UUID, got "ses_1"`},
		{harness: harness.GeminiName, id: uuid},
		{harness: harness.GeminiName, id: "x", wantErr: `--session-id must be a valid UUID, got "x"`},
		{harness: harness.ShellName, id: uuid},
		{harness: harness.OpenCodeName, id: "ses_abc", managed: true},
		{harness: harness.OpenCodeName, id: "ses_abc", wantErr: "--session-id for OpenCode is an internal agentd-minted ses_ id"},
		{harness: harness.OpenCodeName, id: uuid, managed: true, wantErr: "--session-id for OpenCode is an internal agentd-minted ses_ id"},
		{harness: harness.CodexName, id: uuid, wantErr: `--session-id is not supported for the "codex" harness`},
		{harness: harness.CodexName, id: ""},
	}
	for _, tc := range cases {
		err := validatePresetSessionID(harness.MustGet(tc.harness), tc.id, tc.resume, tc.managed)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.wantErr {
			t.Errorf("%s id=%q resume=%q managed=%v: err = %q, want %q", tc.harness, tc.id, tc.resume, tc.managed, got, tc.wantErr)
		}
	}
}

// TestResolveResumeConvFullIDWithoutStore pins the resume call site of
// ResumeFullIDWithoutStore: Gemini resumes an unknown full UUID straight from
// the caller's cwd, while a non-UUID still has to resolve through the store.
func TestResolveResumeConvFullIDWithoutStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const uuid = "0b9d9a6e-3c8c-4f43-9d0c-2b8b6f1e7a10"
	cwd := t.TempDir()
	gemini := harness.MustGet(harness.GeminiName)
	id, project, err := resolveResumeConv(gemini, uuid, false, cwd)
	if err != nil || id != uuid || project != cwd {
		t.Fatalf("gemini full-id resume = (%q, %q, %v), want (%q, %q, nil)", id, project, err, uuid, cwd)
	}
	if _, _, err := resolveResumeConv(gemini, "0b9d9a6e", false, cwd); err == nil {
		t.Fatal("a gemini id prefix must resolve through the store, not pass through")
	}
}
