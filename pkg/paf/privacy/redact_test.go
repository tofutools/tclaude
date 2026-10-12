package privacy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

const (
	ghToken = "ghp_abcdefghijklmnopqrstuvwxyz0123"
	home    = "/home/alice"
	project = "/home/alice/git/app"
)

// redactSample is an agent PAF holding every kind of content a redaction
// touches: a credential in prose, a tool input, a text attachment and the
// definition; old and recent tool results; reasoning with a signature;
// media; a native blob; paths; and an unknown extension.
func redactSample(t *testing.T, withContext bool) *paf.Archive {
	t.Helper()
	tr := transcript.New(transcript.Source{Harness: "claude", Format: "claude-jsonl", ConvID: "c1", Cwd: project})
	tr.Entries = []transcript.Entry{
		{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("my token is " + ghToken + " in " + project + "/main.go")}},
		{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{
			{Kind: transcript.BlockThinking, Text: "look at " + project, Opaque: &transcript.Opaque{Vendor: "anthropic", Data: "sig"}},
			{Kind: transcript.BlockToolCall, CallID: "t1", Name: "Read", Input: json.RawMessage(`{"path":"` + project + `/main.go","key":"` + ghToken + `"}`)},
		}},
		{Role: transcript.RoleUser, Origin: transcript.OriginTool, Blocks: []transcript.Block{
			{Kind: transcript.BlockToolResult, CallID: "t1", Text: "old output by alice", Media: []transcript.Media{{MediaType: "text/plain", Data: []byte("notes " + ghToken)}}},
		}},
		{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("again"), {Kind: transcript.BlockMedia, Media: []transcript.Media{{MediaType: "image/png", Data: []byte("\x89PNG\x00")}}}}},
		{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{{Kind: transcript.BlockToolCall, CallID: "t2", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}},
		{Role: transcript.RoleUser, Origin: transcript.OriginTool, Blocks: []transcript.Block{{Kind: transcript.BlockToolResult, CallID: "t2", Text: "recent output"}}},
	}
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.DeclareExtension(paf.ExtTclaude, paf.ExtensionDecl{Version: "0.1", System: "tclaude", Spec: "https://example.invalid/tclaude"}, false))
	require.NoError(t, w.DeclareExtension("com.acme", paf.ExtensionDecl{Version: "1.0", System: "acme", Spec: "https://example.invalid/acme", Copy: "safe"}, false))
	native := paf.AgentPath("a", "native/claude-jsonl/history.jsonl")
	require.NoError(t, w.WriteFile(native, "application/jsonl", []byte(`{"message":{"content":"`+ghToken+`"}}`+"\n")))
	ag := paf.Agent{Name: "a", Definition: paf.Definition{StartupContext: "work in " + project + " with token=" + ghToken}, Native: &paf.Native{Format: "claude-jsonl", Path: native, Harness: "claude", SourceConvID: "c1"}}
	require.NoError(t, ag.Extensions.Set("com.acme", map[string]any{"secret": "x"}))
	require.NoError(t, ag.Extensions.Set(paf.ExtTclaude, map[string]any{"note": "from " + project}))
	env := paf.Environment{Harness: paf.Harness{Name: "claude"}, Workspace: &paf.Workspace{Cwd: project}}
	require.NoError(t, w.AddAgent("a", ag, env))
	if withContext {
		require.NoError(t, paf.WriteTranscript(w, "a", paf.ScopeEffective, tr))
		require.NoError(t, w.WriteJSON(paf.AgentPath("a", "context/losses.json"), paf.LossesFromReport(tr.Losses)))
	}
	require.NoError(t, w.WriteFile("ext/com.acme/state.json", "application/json", []byte(`{"x":1}`)))
	require.NoError(t, w.WriteFile("ext/tclaude/notes.json", "application/json", []byte(`{"body":"`+ghToken+`"}`)))
	_, err := w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{})
	require.NoError(t, err)
	return a
}

func redactAndOpen(t *testing.T, a *paf.Archive, opts RedactOptions) (*paf.Archive, *RedactReport, []byte) {
	t.Helper()
	var out bytes.Buffer
	rep, err := Redact(a, &out, opts, paf.Producer{Name: "test"})
	require.NoError(t, err)
	b, err := paf.Open(bytes.NewReader(out.Bytes()), int64(out.Len()), paf.Options{})
	require.NoError(t, err, "the redacted copy is a valid PAF")
	return b, rep, out.Bytes()
}

func allText(t *testing.T, a *paf.Archive) string {
	t.Helper()
	var sb strings.Builder
	for _, e := range a.Manifest.Entries {
		raw, err := a.ReadEntry(e.Path, 1<<20)
		require.NoError(t, err)
		sb.Write(raw)
		sb.WriteByte('\n')
	}
	m, _ := json.Marshal(a.Manifest.Extensions)
	sb.Write(m)
	return sb.String()
}

func redactionKinds(rs []paf.Redaction) map[string]int {
	out := map[string]int{}
	for _, r := range rs {
		out[r.Kind] = r.Count
	}
	return out
}

func TestRedactAppliesToEveryCopy(t *testing.T) {
	a := redactSample(t, true)
	b, rep, _ := redactAndOpen(t, a, RedactOptions{MaskFlagged: true, PathsToPlaceholders: true, StubToolResults: true, KeepTurns: 1, DropMedia: true, DropReasoning: true})

	text := allText(t, b)
	assert.NotContains(t, text, ghToken)
	assert.NotContains(t, text, home)
	assert.NotContains(t, text, "alice", "the user name goes too")
	assert.NotContains(t, text, "old output")
	assert.NotContains(t, text, `"thinking"`)
	assert.NotContains(t, text, `"secret"`, "unknown extension data is dropped")
	assert.False(t, b.HasEntry("ext/com.acme/state.json"))
	assert.Contains(t, text, "[REDACTED:github-token]")
	assert.Contains(t, text, "${PROJECT}/main.go")
	assert.Contains(t, text, "recent output", "the last human turn keeps its tool results")
	assert.Empty(t, rep.Findings)

	ag, _ := b.Agent("a")
	assert.Nil(t, ag.Native, "the native blob is never carried through a redaction")
	assert.False(t, b.HasEntry(paf.AgentPath("a", "native/claude-jsonl/history.jsonl")))
	env, _ := b.Environment("a")
	assert.Equal(t, "${PROJECT}", env.Workspace.Cwd)
	require.NotEmpty(t, env.Placeholders)
	for _, p := range env.Placeholders {
		assert.Empty(t, p.Original, "placeholders carry no original value")
	}
	assert.Contains(t, ag.Extensions, paf.ExtTclaude)
	assert.NotContains(t, ag.Extensions, "com.acme")
	assert.NotContains(t, b.Manifest.ExtensionsUsed, "com.acme")

	kinds := redactionKinds(b.Privacy().Redactions)
	assert.Equal(t, rep.Redactions, b.Privacy().Redactions)
	for _, k := range []string{RedactMaskSpans, RedactPaths, RedactStubToolResults, RedactDropMedia, RedactDropReasoning, RedactDropNative, RedactDropUnknown} {
		assert.Positive(t, kinds[k], k)
	}
	assert.Equal(t, 1, kinds[RedactStubToolResults])
	assert.Empty(t, b.Privacy().Consent)

	l, ok := b.Losses("a")
	require.True(t, ok)
	var redacted []string
	for _, it := range l.Items {
		if it.Kind == LossRedacted {
			redacted = append(redacted, it.Detail)
		}
	}
	assert.Contains(t, redacted, RedactMaskSpans)
	assert.Contains(t, redacted, RedactStubToolResults)
}

func TestRedactDecodesNativeOnlyHistory(t *testing.T) {
	a := redactSample(t, false)
	decode := func(n paf.Native, r io.Reader) (*transcript.Transcript, error) {
		raw, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		tr := transcript.New(transcript.Source{Harness: n.Harness, Format: n.Format, ConvID: n.SourceConvID})
		tr.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text(string(raw))}}}
		return tr, nil
	}
	b, _, _ := redactAndOpen(t, a, RedactOptions{MaskFlagged: true, Native: decode})
	assert.True(t, b.HasContext("a", paf.ScopeEffective), "the history is carried as a decoded context")
	assert.NotContains(t, allText(t, b), ghToken)

	// Without a decoder the history cannot be redacted, so it is dropped
	// and recorded.
	b, rep, _ := redactAndOpen(t, a, RedactOptions{MaskFlagged: true})
	assert.False(t, b.HasContext("a", paf.ScopeEffective))
	assert.Contains(t, redactionKinds(rep.Redactions), RedactDropUnknown)
}

func TestRedactPreviewAndNoOp(t *testing.T) {
	a := redactSample(t, true)
	_, err := Redact(a, io.Discard, RedactOptions{}, paf.Producer{Name: "test"})
	require.Error(t, err)
	rep, err := Redact(a, io.Discard, RedactOptions{MaskFlagged: true}, paf.Producer{Name: "test"})
	require.NoError(t, err)
	assert.Positive(t, redactionKinds(rep.Redactions)[RedactMaskSpans])

	// Fewer human turns than kept: no tool result is old enough to stub.
	rep, err = Redact(a, io.Discard, RedactOptions{StubToolResults: true, KeepTurns: 5}, paf.Producer{Name: "test"})
	require.NoError(t, err)
	assert.Zero(t, redactionKinds(rep.Redactions)[RedactStubToolResults])
}

func TestRedactKeepsGroupMemberKeys(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindGroup, paf.Limits{})
	key := "alice-helper"
	require.NoError(t, w.AddAgent(key, paf.Agent{Name: key}, paf.Environment{Harness: paf.Harness{Name: "claude"}, Workspace: &paf.Workspace{Cwd: project}}))
	require.NoError(t, w.WriteJSON(paf.GroupPath, paf.Group{Name: "team", Members: []paf.GroupMember{{Agent: key}}}))
	_, err := w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{})
	require.NoError(t, err)
	b, _, _ := redactAndOpen(t, a, RedactOptions{PathsToPlaceholders: true})
	assert.Equal(t, key, b.Group().Members[0].Agent, "an agent key is an identifier, not redacted text")
}

func TestMaskSpans(t *testing.T) {
	counts := map[string]int{}
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----"
	out := Mask("a "+pem+" b AKIAIOSFODNN7EXAMPLE c", true, counts)
	assert.Equal(t, "a [REDACTED:private-key] b [REDACTED:aws-access-key] c", out)
	assert.Equal(t, map[string]int{"private-key": 1, KindAWSAccessKey: 1}, counts)

	// A key cut off before its END line is masked through its body.
	cut := "$ head -n 3 id_rsa\n-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\nVTLw7onLRnrq0/abc\n$ echo done"
	assert.Equal(t, "$ head -n 3 id_rsa\n[REDACTED:private-key]\n$ echo done", Mask(cut, false, map[string]int{}))
}

func TestReplacePathsBoundaries(t *testing.T) {
	ps := []placeholder{{name: "PROJECT", original: "/home/alice/app"}, {name: "HOME", original: "/home/alice"}, {name: "USER", original: "alice", word: true}}
	out, n := replacePaths("/home/alice/app/x /home/alice/apples /home/alicex alice malice", ps)
	assert.Equal(t, "${PROJECT}/x ${HOME}/apples /home/alicex ${USER} malice", out)
	assert.Equal(t, 3, n)
}
