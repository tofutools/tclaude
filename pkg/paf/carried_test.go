package paf

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foreignArchive is an agent PAF with the given extensions, each with one
// agent object and, when entry is set, one ext/ entry of that many bytes.
func foreignArchive(t *testing.T, decls map[string]ExtensionDecl, entry int) *Archive {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, KindAgent, Limits{})
	ag := Agent{Name: "a"}
	for id, d := range decls {
		require.NoError(t, w.DeclareExtension(id, d, false))
		require.NoError(t, ag.Extensions.Set(id, map[string]any{"v": 1}))
		if entry > 0 {
			require.NoError(t, w.WriteFile(ExtDir+id+"/data.txt", "text/plain", []byte(strings.Repeat("x", entry))))
		}
	}
	require.NoError(t, w.AddAgent("a", ag, Environment{Harness: Harness{Name: "claude"}}))
	_, err := w.Close(Producer{Name: "test"})
	require.NoError(t, err)
	a, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), Options{})
	require.NoError(t, err)
	return a
}

func safeDecl() ExtensionDecl {
	return ExtensionDecl{Version: "1.0", System: "x", Spec: "https://example.invalid/x", Copy: "safe"}
}

func TestCollectExtensionsKeepsOnlyCopySafeData(t *testing.T) {
	unsafe, maybe := safeDecl(), safeDecl()
	unsafe.Copy, maybe.Copy = "", "maybe"
	a := foreignArchive(t, map[string]ExtensionDecl{"com.a": safeDecl(), "com.b": unsafe, "com.c": maybe}, 4)
	c, dropped, err := CollectExtensions(a, "a")
	require.NoError(t, err)
	assert.Empty(t, dropped, "dropping unsafe data is the rule, not a loss to report")
	assert.Len(t, c.Decls, 1)
	assert.Contains(t, c.Agent, "com.a")
	assert.NotContains(t, c.Agent, "com.b")
	require.Len(t, c.Entries, 1)
	assert.Equal(t, ExtDir+"com.a/data.txt", c.Entries[0].Path)
}

func TestCollectExtensionsDropsTheLargestOverBudget(t *testing.T) {
	a := foreignArchive(t, map[string]ExtensionDecl{"com.a": safeDecl(), "com.b": safeDecl()}, MaxCarriedExtensionBytes/2+16)
	c, dropped, err := CollectExtensions(a, "a")
	require.NoError(t, err)
	assert.Len(t, dropped, 1)
	require.NotNil(t, c)
	assert.Len(t, c.Decls, 1)
	assert.Len(t, c.Entries, 1, "the extension is dropped whole")
}

// A reader accepts an undocumented single-label id, but a writer must not mint
// one, so keeping it would make every later export fail.
func TestCollectExtensionsDropsAnIDNoWriterMayDeclare(t *testing.T) {
	a := foreignArchive(t, map[string]ExtensionDecl{"com.a": safeDecl()}, 0)
	a.Manifest.ExtensionsUsed["agentlab"] = safeDecl()
	c, dropped, err := CollectExtensions(a, "a")
	require.NoError(t, err)
	assert.Equal(t, []string{"agentlab"}, dropped)
	assert.NotContains(t, c.Decls, "agentlab")
	var buf bytes.Buffer
	w := NewWriter(&buf, KindAgent, Limits{})
	var root, ag, env Extensions
	assert.NoError(t, c.WriteTo(w, &root, &ag, &env), "what is kept can always be written")
}
