package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiAskerRendersTheRecordedPosture(t *testing.T) {
	argv := geminiAsker{}.BuildAskArgv(AskSpec{
		Print: true, Prompt: "--why?", Ephemeral: true, ResumeID: geminiUsageTestConv,
		ResumeFile: "/state/copy.jsonl", Model: "flash",
		LaunchPosture: &SpawnSpec{HarnessBuiltinMode: GeminiSandboxOff, ApprovalPolicy: GeminiApprovalYolo},
	})
	assert.Equal(t, []string{
		"env", "GEMINI_SANDBOX=false", "SANDBOX=", "gemini",
		"--session-file", "/state/copy.jsonl", "--model", "flash", "--approval-mode=yolo", "--prompt=--why?",
	}, argv, "the copy replaces --resume; the sandbox env and approval are the recorded ones")

	argv = geminiAsker{}.BuildAskArgv(AskSpec{
		Print: true, Prompt: "q",
		LaunchPosture: &SpawnSpec{HarnessBuiltinMode: GeminiSandboxInherit, ApprovalPolicy: GeminiApprovalInherit},
	})
	assert.Equal(t, []string{"gemini", "--prompt=q"}, argv, "inherit adds nothing")

	assert.Nil(t, geminiAsker{}.BuildAskArgv(AskSpec{Print: true, Prompt: "q", Ephemeral: true, ResumeID: geminiUsageTestConv}),
		"an ephemeral resume without a copy fails closed rather than appending to the conversation")
}

func TestGeminiEphemeralResumeForksACopyAndRemovesTheFork(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	source := writeGeminiUsageFixture(t, home, geminiUsageTestConv,
		`{"id":"u1","type":"user","content":"remember the token"}`)
	before, err := os.ReadFile(source)
	require.NoError(t, err)

	copyPath, cleanup, err := geminiAsker{}.PrepareEphemeralResume(geminiUsageTestConv)
	require.NoError(t, err)
	copied, err := os.ReadFile(copyPath)
	require.NoError(t, err)
	assert.Equal(t, before, copied)

	// What Gemini writes for `--session-file <copy>`: a new session whose
	// first message names the copy. Another import (of the original) stays.
	chats := filepath.Dir(source)
	fork := filepath.Join(chats, "session-1790925491807-11112222.jsonl")
	require.NoError(t, os.WriteFile(fork, []byte(`{"sessionId":"11112222-0000-0000-0000-000000000000","kind":"main"}`+"\n"+
		`{"id":"import-1","type":"info","content":"Imported session from `+copyPath+`"}`+"\n"), 0o644))
	other := filepath.Join(chats, "session-2026-10-02T09-00-33334444.jsonl")
	require.NoError(t, os.WriteFile(other, []byte(`{"sessionId":"33334444-0000-0000-0000-000000000000","kind":"main"}`+"\n"+
		`{"id":"import-2","type":"info","content":"Imported session from `+source+`"}`+"\n"), 0o644))

	cleanup()
	assert.NoFileExists(t, fork, "the séance's fork is removed")
	assert.NoFileExists(t, copyPath, "the copy is removed")
	assert.FileExists(t, other, "an import tclaude did not make is left alone")
	after, err := os.ReadFile(source)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the predecessor's own file is untouched")
	assert.False(t, strings.HasPrefix(copyPath, chats), "the copy never lives where conversations are listed")
}

func TestGeminiEphemeralResumeRefusesAPlantedSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	writeGeminiUsageFixture(t, home, geminiUsageTestConv, `{"id":"u1","type":"user","content":"x"}`)
	elsewhere := t.TempDir()
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(home, ".gemini", geminiSeanceDirName)))

	_, cleanup, err := geminiAsker{}.PrepareEphemeralResume(geminiUsageTestConv)
	cleanup()
	assert.ErrorContains(t, err, "not a plain directory")
	entries, _ := os.ReadDir(elsewhere)
	assert.Empty(t, entries, "nothing is written through the planted link")
}

func TestGeminiRemoveEphemeralSessionDeletesThePinnedConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	path := writeGeminiUsageFixture(t, home, geminiUsageTestConv, `{"id":"u1","type":"user","content":"x"}`)
	geminiAsker{}.RemoveEphemeralSession(geminiUsageTestConv)
	assert.NoFileExists(t, path)
	geminiAsker{}.RemoveEphemeralSession(geminiUsageTestConv) // nothing left: a no-op
}
