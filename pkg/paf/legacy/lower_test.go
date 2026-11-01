package legacy

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func history(t *testing.T, b *agentbundle.Bundle) []byte {
	t.Helper()
	r, err := b.OpenHistory()
	require.NoError(t, err)
	defer r.Close()
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	return raw
}

// sameManifest compares manifests with the inline profile compared as JSON:
// encoders re-indent it.
func sameManifest(t *testing.T, want, got agentbundle.Manifest) {
	t.Helper()
	assert.JSONEq(t, string(want.Agent.Profile), string(got.Agent.Profile))
	want.Agent.Profile, got.Agent.Profile = nil, nil
	assert.Equal(t, want, got)
}

func writeTemp(t *testing.T, dir string, raw []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(dir, "archive-")
	require.NoError(t, err)
	_, err = f.Write(raw)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// v1 -> PAF -> v1 is lossless: the lowered bundle is the original.
func TestAgentBundleRoundTripThroughPAF(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	orig := sampleBundle(t)
	var buf bytes.Buffer
	tr := transcript.New(transcript.Source{Harness: "claude", ConvID: "conv-1"})
	tr.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("hello")}}}
	require.NoError(t, WriteAgentBundle(orig, &buf, WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: "v1"}, Context: tr}))

	got, info, err := DecodeBundleFile(writeTemp(t, dir, buf.Bytes()), DecodeLimits{MaxBytes: agentbundle.MaxBytes}, dir)
	require.NoError(t, err)
	defer got.Close()
	assert.Equal(t, paf.Format, info.Source)
	assert.True(t, info.HasContext)
	sameManifest(t, orig.Manifest, got.Manifest)
	assert.Equal(t, orig.MailDeliveries, got.MailDeliveries)
	assert.Equal(t, history(t, orig), history(t, got))

	path := got.TranscriptPath
	require.NoError(t, got.Close())
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "Close removes the extracted native history")
}

func TestDecodeBundleFileReadsV1Unchanged(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	orig := sampleBundle(t)
	raw, err := orig.Encode()
	require.NoError(t, err)
	got, info, err := DecodeBundleFile(writeTemp(t, dir, raw), DecodeLimits{MaxBytes: agentbundle.MaxBytes}, dir)
	require.NoError(t, err)
	defer got.Close()
	assert.Equal(t, SourceAgentBundleV1, info.Source)
	sameManifest(t, orig.Manifest, got.Manifest)

	b2, info, err := DecodeBundleBytes(raw, DecodeLimits{MaxBytes: agentbundle.MaxBytes}, dir)
	require.NoError(t, err)
	assert.Equal(t, SourceAgentBundleV1, info.Source)
	sameManifest(t, orig.Manifest, b2.Manifest)
}

// A PAF from another producer, with no tclaude extension and only neutral
// context, lowers to configuration with a warning.
func TestForeignContextOnlyPAFLowersToConfiguration(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.AddAgent("a", paf.Agent{Name: "reviewer", Definition: paf.Definition{Role: "reviewer"}}, paf.Environment{Harness: paf.Harness{Name: "codex"}}))
	tr := transcript.New(transcript.Source{Harness: "codex"})
	tr.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("hi")}}}
	require.NoError(t, paf.WriteTranscript(w, "a", paf.ScopeEffective, tr))
	require.NoError(t, w.WriteJSON(paf.AgentPath("a", "context/losses.json"), paf.LossesFromReport(tr.Losses)))
	_, err := w.Close(paf.Producer{Name: "other-tool"})
	require.NoError(t, err)

	b, info, err := DecodeBundleBytes(buf.Bytes(), DecodeLimits{MaxBytes: agentbundle.MaxBytes}, dir)
	require.NoError(t, err)
	defer b.Close()
	assert.Equal(t, paf.Format, info.Source)
	assert.Equal(t, "reviewer", b.Manifest.Agent.Name)
	assert.Equal(t, "codex", b.Manifest.Agent.Harness)
	assert.JSONEq(t, `{}`, string(b.Manifest.Agent.Profile))
	assert.Nil(t, b.Manifest.History)
	assert.Contains(t, b.Manifest.Warnings, WarningContextOnly)
	left, _ := filepath.Glob(filepath.Join(dir, ".paf-decode-*"))
	assert.Empty(t, left)
}

func TestDecodeRefusesGroupPAF(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindGroup, paf.Limits{})
	require.NoError(t, w.AddAgent("a", paf.Agent{Name: "a"}, paf.Environment{Harness: paf.Harness{Name: "codex"}}))
	require.NoError(t, w.WriteJSON(paf.GroupPath, paf.Group{Name: "g", Members: []paf.GroupMember{{Agent: "a"}}}))
	_, err := w.Close(paf.Producer{Name: "t"})
	require.NoError(t, err)
	_, _, err = DecodeBundleBytes(buf.Bytes(), DecodeLimits{MaxBytes: agentbundle.MaxBytes}, dir)
	assert.ErrorContains(t, err, "exactly one agent")
}

// A bundle holds several agents with no group. A federation offer names none of
// them, so it is refused rather than taking whichever comes first.
func TestDecodeRefusesBundlePAFWithoutAnAgentKey(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindBundle, paf.Limits{})
	for _, key := range []string{"a", "b"} {
		require.NoError(t, w.AddAgent(key, paf.Agent{Name: key}, paf.Environment{Harness: paf.Harness{Name: "codex"}}))
	}
	require.NoError(t, w.AddWorkflow("flow", "flow@sha256:aa", nil))
	require.NoError(t, w.WriteJSON(paf.WorkflowPath("flow", "workflow.json"), map[string]string{"root": "flow@sha256:aa"}))
	_, err := w.Close(paf.Producer{Name: "t"})
	require.NoError(t, err)
	_, _, err = DecodeBundleBytes(buf.Bytes(), DecodeLimits{MaxBytes: agentbundle.MaxBytes}, dir)
	require.Error(t, err)
	b, _, err := DecodeBundleBytes(buf.Bytes(), DecodeLimits{MaxBytes: agentbundle.MaxBytes, AgentKey: "b"}, dir)
	require.NoError(t, err, "naming one lands that one")
	defer func() { _ = b.Close() }()
	assert.Equal(t, "b", b.Manifest.Agent.Name)
}
