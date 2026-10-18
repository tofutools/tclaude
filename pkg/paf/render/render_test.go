//ci:reads docs/paf/examples/

package render

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// zipDir packs an unpacked example archive, manifest first.
func zipDir(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string) {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		require.NoError(t, err)
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		require.NoError(t, err)
		_, err = w.Write(raw)
		require.NoError(t, err)
	}
	add(paf.ManifestPath)
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() || filepath.ToSlash(rel) == paf.ManifestPath {
			return nil
		}
		add(filepath.ToSlash(rel))
		return nil
	}))
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func openExample(t *testing.T) *paf.Archive {
	t.Helper()
	raw := zipDir(t, "../../../docs/paf/examples/full-agent")
	a, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	return a
}

func TestSummarizeExample(t *testing.T) {
	s := Summarize(openExample(t), paf.Format)
	require.Len(t, s.Agents, 1)
	ag := s.Agents[0]
	assert.Equal(t, "fixer", ag.Name)
	assert.Equal(t, "claude", ag.Harness)
	require.NotNil(t, ag.Native)
	require.Len(t, ag.Context, 1)
	assert.Equal(t, paf.ScopeEffective, ag.Context[0].Scope)
	require.Len(t, s.Signatures, 1)
	assert.Equal(t, string(paf.SignatureValid), s.Signatures[0].Status)
}

func TestMarkdownAndHTMLShowTheConversation(t *testing.T) {
	a := openExample(t)
	var md, page bytes.Buffer
	require.NoError(t, Markdown(&md, a, paf.Format, Options{}))
	require.NoError(t, HTML(&page, a, paf.Format, Options{}))
	for _, out := range []string{md.String(), page.String()} {
		assert.Contains(t, out, "TestCloneCooldown is flaky")
		assert.Contains(t, out, "Root cause")
		assert.Contains(t, out, "pre_compaction_history")
	}
	assert.Contains(t, page.String(), "default-src 'none'")
	assert.NotContains(t, page.String(), "<script")
}

func TestHTMLEscapesArchiveText(t *testing.T) {
	tr := transcript.New(transcript.Source{Harness: "claude"})
	tr.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{
		transcript.Text(`<script>alert(1)</script>`),
		{Kind: transcript.BlockMedia, Media: []transcript.Media{{MediaType: "image/svg+xml", Data: []byte(`<svg onload="alert(1)"/>`)}}},
	}}}
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.AddAgent("a", paf.Agent{Name: `<img src=x onerror=alert(1)>`}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	require.NoError(t, paf.WriteTranscript(w, "a", paf.ScopeEffective, tr))
	require.NoError(t, w.WriteJSON(paf.AgentPath("a", "context/losses.json"), paf.LossesFromReport(tr.Losses)))
	_, err := w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	var page bytes.Buffer
	require.NoError(t, HTML(&page, a, paf.Format, Options{}))
	out := page.String()
	assert.NotContains(t, out, "<script>alert")
	assert.NotContains(t, out, "<img src=x")
	assert.NotContains(t, out, "data:image/svg")
	assert.Contains(t, out, "&lt;script&gt;alert(1)&lt;/script&gt;")
}

func TestTranscriptFallsBackToNativeDecoder(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	native := paf.AgentPath("a", "native/claude.jsonl")
	require.NoError(t, w.WriteFile(native, "application/jsonl", []byte("{\"x\":1}\n")))
	require.NoError(t, w.AddAgent("a", paf.Agent{Name: "a", Native: &paf.Native{Format: "claude-jsonl", Path: native, Harness: "claude"}}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	_, err := w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)

	_, _, err = Transcript(a, "a", Options{})
	assert.True(t, errors.Is(err, ErrNoTranscript))
	_, _, err = Transcript(a, "a", Options{Scope: paf.ScopeFull})
	assert.True(t, errors.Is(err, ErrNoTranscript))

	decoded := false
	tr, from, err := Transcript(a, "a", Options{Native: func(n paf.Native, r io.Reader) (*transcript.Transcript, error) {
		raw, _ := io.ReadAll(r)
		decoded = strings.Contains(string(raw), `"x":1`)
		t := transcript.New(transcript.Source{Harness: n.Harness})
		t.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("from native")}}}
		return t, nil
	}})
	require.NoError(t, err)
	assert.True(t, decoded)
	assert.Equal(t, "native claude-jsonl", from)
	require.Len(t, tr.Entries, 1)

	var md bytes.Buffer
	require.NoError(t, Markdown(&md, a, paf.Format, Options{}))
	assert.Contains(t, md.String(), "Not shown: no readable conversation")
}

func TestMarkdownMakesArchiveProseInert(t *testing.T) {
	tr := transcript.New(transcript.Source{Harness: "claude"})
	tr.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Author: "<b>x</b>", Blocks: []transcript.Block{
		transcript.Text("hi <img src=http://example.com/t.png>\n#### 9. assistant\nok"),
	}}}
	out := inertMarkdown(tr)
	text := out.Entries[0].Blocks[0].Text
	assert.NotContains(t, text, "<img")
	assert.Contains(t, text, "\\#### 9. assistant")
	assert.Equal(t, "&lt;b>x&lt;/b>", out.Entries[0].Author)
	assert.Contains(t, tr.Entries[0].Blocks[0].Text, "<img", "the original transcript is left alone")
}
