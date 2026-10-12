package privacy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/paf"
)

func TestDetectFloorAndEntropy(t *testing.T) {
	cases := map[string]string{
		"export GH=ghp_abcdefghijklmnopqrstuvwxyz0123":           "github-token",
		"key AKIAIOSFODNN7EXAMPLE here":                          KindAWSAccessKey,
		"the token is xK9fQ2mZ7pL4vR8sT1wN6yB3cD5eH0jA, keep it": KindHighEntropy,
		"password = hunter2hunter2":                              "credential-assignment",
	}
	for text, kind := range cases {
		assert.Contains(t, Detect(text), kind, text)
	}
	// Ordinary transcript material is not flagged.
	for _, text := range []string{
		"commit 3c7b1ef6b8a04d2e9f1c5a7b3d9e0f1a2b3c4d5e merged",
		"conv aaaaaaaa-1111-2222-3333-444444444444",
		"toolu_01PjWcS8T5g6kiKebKcTErti and msg_01XyZ3abc9DEF4ghi7JKL2mno",
		"/home/johkjo/git/tclaude-paf-lib/pkg/claude/agentd/groups_export_land.go",
		"TestGroupImport_PAFMembersLandOnAnotherHarness passed",
		"sha256-4f6a8c1e2b3d5f7a9c0e1b2d3f4a5c6e7d8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b",
		strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk", 6),
	} {
		assert.Empty(t, Detect(text), text)
	}
}

func TestCollectorSkipsEntropyInStructuralKeys(t *testing.T) {
	c := NewCollector()
	c.JSON([]byte(`{"id":"xK9fQ2mZ7pL4vR8sT1wN6yB3cD5eH0jA","message":{"content":"use xK9fQ2mZ7pL4vR8sT1wN6yB3cD5eH0jA"}}`), "line 1")
	f := c.Findings()
	require.Len(t, f, 1)
	assert.Equal(t, KindHighEntropy, f[0].Kind)
	assert.Equal(t, 1, f[0].Count)
	assert.Equal(t, []string{"line 1.message.content"}, f[0].Locations)
	assert.False(t, Blocking(f), "entropy alone is advisory")
	c.Text("ghp_abcdefghijklmnopqrstuvwxyz0123", "x")
	assert.True(t, Blocking(c.Findings()))
}

func TestScanArchiveCoversContextNativeAndAttachments(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	native := paf.AgentPath("a", "native/claude-jsonl/history.jsonl")
	require.NoError(t, w.WriteFile(native, "application/jsonl", []byte(`{"message":{"content":"ghp_abcdefghijklmnopqrstuvwxyz0123"}}`+"\n")))
	require.NoError(t, w.AddAgent("a", paf.Agent{Name: "a", Native: &paf.Native{Format: "claude-jsonl", Path: native}}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	_, err := w.AddAttachment([]byte("notes\nAKIAIOSFODNN7EXAMPLE\n"), "text/plain")
	require.NoError(t, err)
	_, err = w.AddAttachment([]byte("\x89PNG\x00\x00AKIAIOSFODNN7EXAMPLE binary"), "image/png")
	require.NoError(t, err)
	_, err = w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	f, err := ScanArchive(a, 1<<20)
	require.NoError(t, err)
	kinds := map[string]paf.Finding{}
	for _, x := range f {
		kinds[x.Kind] = x
	}
	assert.Equal(t, 1, kinds["github-token"].Count)
	assert.Equal(t, []string{native + " line 1.message.content"}, kinds["github-token"].Locations)
	assert.Equal(t, 1, kinds[KindAWSAccessKey].Count, "a text attachment is scanned, a binary one is not")
}

func TestScanArchiveSniffsMislabelledAttachmentsAndManifestExtensions(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.AddAgent("a", paf.Agent{Name: "a"}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	_, err := w.AddAttachment([]byte("API=AKIAIOSFODNN7EXAMPLE\n"), "image/png")
	require.NoError(t, err)
	require.NoError(t, w.DeclareExtension("org.example", paf.ExtensionDecl{Version: "1.0", System: "x", Spec: "https://example.org/ext"}, false))
	var ext paf.Extensions
	require.NoError(t, ext.Set("org.example", map[string]string{"note": "ghp_abcdefghijklmnopqrstuvwxyz0123"}))
	w.SetManifestExtensions(ext)
	_, err = w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	f, err := ScanArchive(a, 1<<20)
	require.NoError(t, err)
	kinds := map[string]bool{}
	for _, x := range f {
		kinds[x.Kind] = true
	}
	assert.True(t, kinds[KindAWSAccessKey], "a text attachment labelled image/png is still scanned")
	assert.True(t, kinds["github-token"], "manifest extensions are scanned")
}

func TestChunksScanOverlongLines(t *testing.T) {
	// A single 3 MiB line with tokens at the start, across a window cut and
	// at the end.
	line := []byte(strings.Repeat("x ", 3<<19))
	copy(line[10:], " AKIAIOSFODNN7EXAMPLE ")
	copy(line[chunkLen-8:], " AKIAIOSFODNN7EXAMPLE ")
	copy(line[len(line)-30:], " AKIAIOSFODNN7EXAMPLE ")
	c := NewCollector()
	require.NoError(t, c.chunks(bytes.NewReader(line), "big"))
	f := c.Findings()
	require.Len(t, f, 1)
	assert.GreaterOrEqual(t, f[0].Count, 3)
}
