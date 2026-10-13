package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPiAskerArgv(t *testing.T) {
	withPiExtensionCache(t)
	prev := piSessionFileLookup
	piSessionFileLookup = func(_, id string) string { return "/store/x/2026_" + id + ".jsonl" }
	t.Cleanup(func() { piSessionFileLookup = prev })
	a := piAsker{}

	assert.Equal(t,
		[]string{"pi", "--print", "--session-id", "s1", "--model", "openai/gpt-5.5", "--thinking", "high",
			"--tools", "read,grep,find,ls", "--", "-why?"},
		a.BuildAskArgv(AskSpec{Print: true, SessionID: "s1", Model: "openai/gpt-5.5", Effort: "high", Prompt: "-why?"}),
		"a capture has nobody to approve anything, so it gets pi's read-only tools")

	assert.Equal(t,
		[]string{"pi", "--mode", "json", "--session", "/store/x/2026_c1.jsonl", "--tools", "read,grep,find,ls", "--", "q"},
		a.BuildAskArgv(AskSpec{Print: true, Stream: true, ResumeID: "c1", Prompt: "q"}),
		"a resume passes the session file, as the spawner does")

	interactive := a.BuildAskArgv(AskSpec{SessionID: "s1", Prompt: "q"})
	assert.NotContains(t, interactive, "--print")
	require.Contains(t, interactive, "--extension")
	assert.True(t, strings.HasSuffix(interactive[len(interactive)-3], piPermissionExtensionFile),
		"the human at the terminal approves each acting call: %v", interactive)

	for policy, wantTools := range map[string]bool{
		PiApprovalAuto: false, PiApprovalAsk: true, PiApprovalReadOnly: true, "": true, "bogus": true,
	} {
		argv := a.BuildAskArgv(AskSpec{Print: true, Ephemeral: true, Prompt: "q",
			LaunchPosture: &SpawnSpec{ApprovalPolicy: policy}})
		assert.Equal(t, wantTools, strings.Contains(strings.Join(argv, " "), "--tools read,grep,find,ls"), "posture %q: %v", policy, argv)
		assert.Contains(t, argv, "--no-session", "a fresh ephemeral turn is not persisted")
		assert.Contains(t, argv, "--no-approve", "a brokered one-shot ignores project-local resources")
	}
	assert.NotContains(t, a.BuildAskArgv(AskSpec{Print: true, Prompt: "q"}), "--no-approve",
		"the operator's own ask keeps their project trust")

	assert.Nil(t, a.BuildAskArgv(AskSpec{Print: true, Ephemeral: true, ResumeID: "c1", Prompt: "q"}),
		"an ephemeral resume without its copy fails closed")
	assert.Equal(t,
		[]string{"pi", "--print", "--session", "/tmp/copy.jsonl", "--no-approve", "--", "q"},
		a.BuildAskArgv(AskSpec{Print: true, Ephemeral: true, ResumeID: "c1", ResumeFile: "/tmp/copy.jsonl", Prompt: "q",
			LaunchPosture: &SpawnSpec{ApprovalPolicy: PiApprovalAuto}}))
}

func TestPiAskerStreamFilterShowsOnlyTheAnswer(t *testing.T) {
	var out bytes.Buffer
	f := piAsker{}.StreamFilter(&out, false, nil)
	for _, line := range []string{
		`{"type":"agent_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"secret plan"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Hello"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","delta":"{\"command\":"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":2}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"world."}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hello world."}]}}`,
		`{"type":"agent_settled","aborted":false}`,
	} {
		_, _ = f.Write([]byte(line + "\n"))
	}
	require.NoError(t, f.(AskStreamFlusher).Flush())
	assert.Equal(t, "Hello\nworld.\n", out.String())

	out.Reset()
	f = piAsker{}.StreamFilter(&out, false, nil)
	_, _ = f.Write([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"401 Unauthorized"}}` + "\n"))
	require.NoError(t, f.(AskStreamFlusher).Flush())
	assert.Equal(t, "401 Unauthorized\n", out.String(), "a failed turn is never silent")
}

// An ephemeral resume continues a copy two levels below the session store's
// project directories, out of sight of pi's and tclaude's session scans, and
// cleanup removes it.
func TestPiAskerEphemeralResumeCopiesTheSession(t *testing.T) {
	agentDir := piTestAgentDir(t)
	cwd := t.TempDir()
	const id = "aaaa1111-0000-4000-8000-00000000000a"
	original := piWriteSession(t, agentDir, cwd, id, piHeader(id, cwd))

	copyPath, cleanup, err := piAsker{}.PrepareEphemeralResume(id)
	require.NoError(t, err)
	assert.NotEqual(t, original, copyPath)
	assert.Equal(t, filepath.Join(agentDir, "sessions", piSeanceDirName), filepath.Dir(filepath.Dir(copyPath)))
	got, err := os.ReadFile(copyPath)
	require.NoError(t, err)
	want, err := os.ReadFile(original)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	for _, root := range piSessionRoots() {
		for _, file := range piSessionFiles(root) {
			assert.NotEqual(t, copyPath, file, "the copy is not a conversation")
		}
	}
	cleanup()
	assert.NoFileExists(t, copyPath)

	_, _, err = piAsker{}.PrepareEphemeralResume("bbbb2222-0000-4000-8000-00000000000b")
	assert.Error(t, err)
}

// The session store is writable to a walled pi. A session file planted as a
// symlink (or hard link) to a file the sandbox hides is refused rather than
// copied into the store, and a seance directory planted as a symlink out of
// the store cannot steer where the copy is written.
func TestPiAskerEphemeralResumeRefusesPlantedLinks(t *testing.T) {
	agentDir := piTestAgentDir(t)
	cwd := t.TempDir()
	secret := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(secret, []byte("SECRET"), 0o600))

	const linked = "cccc3333-0000-4000-8000-00000000000c"
	planted := piWriteSession(t, agentDir, cwd, linked, piHeader(linked, cwd))
	require.NoError(t, os.Remove(planted))
	require.NoError(t, os.Symlink(secret, planted))
	_, _, err := piAsker{}.PrepareEphemeralResume(linked)
	assert.Error(t, err, "a symlinked session file is not copied")

	const hard = "dddd4444-0000-4000-8000-00000000000d"
	planted = piWriteSession(t, agentDir, cwd, hard, piHeader(hard, cwd))
	require.NoError(t, os.Remove(planted))
	require.NoError(t, os.Link(secret, planted))
	_, _, err = piAsker{}.PrepareEphemeralResume(hard)
	assert.Error(t, err, "a hard-linked session file is not copied")

	const id = "eeee5555-0000-4000-8000-00000000000e"
	piWriteSession(t, agentDir, cwd, id, piHeader(id, cwd))
	outside := t.TempDir()
	seance := filepath.Join(agentDir, "sessions", piSeanceDirName)
	require.NoError(t, os.RemoveAll(seance))
	require.NoError(t, os.Symlink(outside, seance))
	_, _, err = piAsker{}.PrepareEphemeralResume(id)
	assert.Error(t, err, "a seance directory linked out of the store is refused")
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was written outside the store")
}
