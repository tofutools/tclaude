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
	}

	assert.Nil(t, a.BuildAskArgv(AskSpec{Print: true, Ephemeral: true, ResumeID: "c1", Prompt: "q"}),
		"an ephemeral resume without its copy fails closed")
	assert.Equal(t,
		[]string{"pi", "--print", "--session", "/tmp/copy.jsonl", "--", "q"},
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
