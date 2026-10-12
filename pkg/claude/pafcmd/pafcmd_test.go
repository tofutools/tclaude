package pafcmd

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const claudeHistory = `{"type":"user","uuid":"u1","sessionId":"conv-1","timestamp":"2026-10-11T10:00:00Z","cwd":"/w","message":{"role":"user","content":"hello from native history"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"conv-1","timestamp":"2026-10-11T10:00:05Z","cwd":"/w","message":{"role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"native reply"}]}}
`

func legacyBundle(t *testing.T) *agentbundle.Bundle {
	t.Helper()
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), TclaudeVersion: "test",
		Agent: agentbundle.Definition{Name: "reviewer", Harness: "claude", Profile: json.RawMessage(`{"harness":"claude"}`), Paths: agentbundle.Paths{Cwd: "/w"}, StartupContext: "Review the changes."}}}
	b.SetHistory("claude-jsonl", "conv-1", []byte(claudeHistory))
	return b
}

func writeFile(t *testing.T, dir, name string, raw []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, raw, 0o600))
	return p
}

// isolate points tclaude's data directory, database and federation key at a
// fresh home for the test.
func isolate(t *testing.T) string {
	t.Helper()
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	return home
}

func TestValidateInspectAndRenderPAF(t *testing.T) {
	isolate(t)
	dir := testutil.CanonicalTempDir(t)
	var buf bytes.Buffer
	require.NoError(t, legacy.WriteAgentBundle(legacyBundle(t), &buf, legacy.WriteOptions{Producer: producer()}))
	file := writeFile(t, dir, "reviewer.paf", buf.Bytes())

	var out bytes.Buffer
	require.Equal(t, 0, runValidate(&out, &validateParams{File: file}), out.String())
	assert.Contains(t, out.String(), "valid:")
	assert.Contains(t, out.String(), "unsigned")

	out.Reset()
	require.NoError(t, runInspect(&out, &inspectParams{File: file, JSON: true}))
	var s struct {
		Source string `json:"source"`
		Agents []struct {
			Name    string `json:"name"`
			Harness string `json:"harness"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &s))
	assert.Equal(t, paf.Format, s.Source)
	require.Len(t, s.Agents, 1)
	assert.Equal(t, "reviewer", s.Agents[0].Name)

	// No neutral context: render decodes the native history through the
	// Claude Code codec, which needs no installed harness.
	out.Reset()
	require.NoError(t, runRender(&out, &renderParams{File: file}))
	assert.Contains(t, out.String(), "From the native claude-jsonl")
	assert.Contains(t, out.String(), "hello from native history")
	assert.Contains(t, out.String(), "native reply")

	htmlOut := filepath.Join(dir, "reviewer.html")
	require.NoError(t, runRender(&out, &renderParams{File: file, Out: htmlOut}))
	page, err := os.ReadFile(htmlOut)
	require.NoError(t, err)
	assert.Contains(t, string(page), "<!doctype html>")
	assert.Contains(t, string(page), "native reply")

	assert.Error(t, runRender(&out, &renderParams{File: file, Format: "pdf"}))
	assert.Error(t, runRender(&out, &renderParams{File: file, Scope: "partial"}))
}

func TestLegacyBundleReadsAsPAF(t *testing.T) {
	isolate(t)
	dir := testutil.CanonicalTempDir(t)
	raw, err := legacyBundle(t).Encode()
	require.NoError(t, err)
	file := writeFile(t, dir, "reviewer.zip", raw)
	var out bytes.Buffer
	require.Equal(t, 0, runValidate(&out, &validateParams{File: file}))
	assert.Contains(t, out.String(), legacy.SourceAgentBundleV1)
	out.Reset()
	require.NoError(t, runRender(&out, &renderParams{File: file}))
	assert.Contains(t, out.String(), "converted to PAF on read")
	assert.Contains(t, out.String(), "hello from native history")
}

func TestValidateRefusesTamperedArchive(t *testing.T) {
	isolate(t)
	dir := testutil.CanonicalTempDir(t)
	var buf bytes.Buffer
	require.NoError(t, legacy.WriteAgentBundle(legacyBundle(t), &buf, legacy.WriteOptions{Producer: producer()}))
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	var tampered bytes.Buffer
	zw := zip.NewWriter(&tampered)
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		var data bytes.Buffer
		_, err = data.ReadFrom(rc)
		require.NoError(t, err)
		_ = rc.Close()
		content := data.Bytes()
		if f.Name != paf.ManifestPath && bytes.Contains(content, []byte("hello from native history")) {
			content = bytes.Replace(content, []byte("hello"), []byte("HELLO"), 1)
		}
		w, err := zw.Create(f.Name)
		require.NoError(t, err)
		_, err = w.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	file := writeFile(t, dir, "tampered.paf", tampered.Bytes())
	var out bytes.Buffer
	assert.Equal(t, 1, runValidate(&out, &validateParams{File: file}))
	assert.Contains(t, out.String(), "invalid:")

	out.Reset()
	assert.Equal(t, 1, runValidate(&out, &validateParams{File: filepath.Join(dir, "missing.paf")}))
}

func TestSignerIsThisNodeOrUnknown(t *testing.T) {
	home := isolate(t)
	dir := testutil.CanonicalTempDir(t)
	self, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, proto.SaveIdentity(filepath.Join(home, ".tclaude", "data", "federation", "instance.key"), self))
	other, err := proto.NewIdentity()
	require.NoError(t, err)
	for _, tc := range []struct {
		id   *proto.Identity
		want string
	}{{self, "this node"}, {other, "unknown key"}} {
		var buf bytes.Buffer
		require.NoError(t, legacy.WriteAgentBundle(legacyBundle(t), &buf, legacy.WriteOptions{Producer: producer(),
			Keys: []paf.SigningKey{{Key: tc.id.Priv, KeyID: self.ID(), Signer: &paf.Signer{Kind: "instance"}}}}))
		file := writeFile(t, dir, tc.want+".paf", buf.Bytes())
		var out bytes.Buffer
		require.Equal(t, 0, runValidate(&out, &validateParams{File: file}), out.String())
		assert.Contains(t, out.String(), "valid ed25519, "+tc.want+" (claims key_id \""+self.ID()+"\"")
	}
}

// rewriteEntry copies a zip archive, passing every entry through edit.
func rewriteEntry(t *testing.T, raw []byte, edit func(name string, data []byte) []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		var data bytes.Buffer
		_, err = data.ReadFrom(rc)
		require.NoError(t, err)
		_ = rc.Close()
		w, err := zw.Create(f.Name)
		require.NoError(t, err)
		_, err = w.Write(edit(f.Name, data.Bytes()))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// key_id, signer and alg sit outside what a signature covers. Editing them
// after signing must not let a file present itself as trusted.
func TestUnsignedSignatureFieldsCannotSpoofTrust(t *testing.T) {
	isolate(t)
	dir := testutil.CanonicalTempDir(t)
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, legacy.WriteAgentBundle(legacyBundle(t), &buf, legacy.WriteOptions{Producer: producer(),
		Keys: []paf.SigningKey{{Key: id.Priv, KeyID: "k1", Signer: &paf.Signer{Kind: "instance"}}}}))
	editSig := func(field string, value any) []byte {
		return rewriteEntry(t, buf.Bytes(), func(name string, data []byte) []byte {
			if filepath.Dir(name) != "signatures" {
				return data
			}
			var m map[string]any
			require.NoError(t, json.Unmarshal(data, &m))
			m[field] = value
			out, err := json.Marshal(m)
			require.NoError(t, err)
			return out
		})
	}
	spoof := "TRUSTED, this node\nsignature x: valid ed25519 \x1b[2K"
	var out bytes.Buffer
	file := writeFile(t, dir, "keyid.paf", editSig("key_id", spoof))
	assert.Equal(t, 1, runValidate(&out, &validateParams{File: file}))
	assert.Contains(t, out.String(), "key_id is not a valid identifier")
	assert.NotContains(t, out.String(), "\x1b")

	out.Reset()
	file = writeFile(t, dir, "alg.paf", editSig("alg", "ed25519 key TRUSTED, this node\n"))
	require.Equal(t, 0, runValidate(&out, &validateParams{File: file}), out.String())
	assert.Contains(t, out.String(), "unsigned: no signature uses an algorithm this tclaude can verify")
	assert.Contains(t, out.String(), "unsupported unrecognised")
	assert.NotContains(t, out.String(), "TRUSTED")
}

func TestTermSafe(t *testing.T) {
	assert.Equal(t, "a\uFFFD[2Kb\uFFFDc\uFFFDd", termSafe("a\x1b[2Kb\nc\u202ed"))
}

func TestRenderRefusesToOverwriteItsInput(t *testing.T) {
	isolate(t)
	dir := testutil.CanonicalTempDir(t)
	var buf bytes.Buffer
	require.NoError(t, legacy.WriteAgentBundle(legacyBundle(t), &buf, legacy.WriteOptions{Producer: producer()}))
	file := writeFile(t, dir, "in.paf", buf.Bytes())
	var out bytes.Buffer
	require.ErrorContains(t, runRender(&out, &renderParams{File: file, Out: file, Format: "md"}), "--out is the input file")
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, buf.Bytes(), after)
}

func TestInspectAndRenderGroupExport(t *testing.T) {
	isolate(t)
	dir := testutil.CanonicalTempDir(t)
	exp := &groupexport.Export{FormatVersion: groupexport.FormatVersion, ExportedAt: "2026-10-11T10:00:00Z", SourceGroup: "team",
		Members: []groupexport.Member{{ConvID: "conv-1", Role: "lead"}},
		Convs:   []groupexport.Conv{{ConvID: "conv-1", Title: "reviewer", SourceCwd: "/w", Content: []byte(claudeHistory)}}}
	raw, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	file := writeFile(t, dir, "team.zip", raw)

	var out bytes.Buffer
	require.Equal(t, 0, runValidate(&out, &validateParams{File: file}), out.String())
	assert.Contains(t, out.String(), legacy.SourceGroupExport)
	out.Reset()
	require.NoError(t, runInspect(&out, &inspectParams{File: file, JSON: true}))
	var s struct {
		Kind  string `json:"kind"`
		Group struct {
			Name    string   `json:"name"`
			Members []string `json:"members"`
		} `json:"group"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &s))
	assert.Equal(t, paf.KindGroup, s.Kind)
	assert.Equal(t, "team", s.Group.Name)
	assert.Equal(t, []string{"reviewer"}, s.Group.Members)
	out.Reset()
	require.NoError(t, runRender(&out, &renderParams{File: file}))
	assert.Contains(t, out.String(), "hello from native history")
}

func TestInspectScanRescans(t *testing.T) {
	isolate(t)
	dir := testutil.CanonicalTempDir(t)
	b := legacyBundle(t)
	b.SetHistory("claude-jsonl", "conv-1", []byte(`{"type":"user","message":{"role":"user","content":"AKIAIOSFODNN7EXAMPLE"}}`+"\n"))
	var buf bytes.Buffer
	require.NoError(t, legacy.WriteAgentBundle(b, &buf, legacy.WriteOptions{Producer: producer()}))
	file := writeFile(t, dir, "x.paf", buf.Bytes())
	var out bytes.Buffer
	require.NoError(t, runInspect(&out, &inspectParams{File: file, Scan: true}))
	assert.Contains(t, out.String(), "Scan (this node):")
	assert.Contains(t, out.String(), "aws-access-key: 1")
	assert.NotContains(t, out.String(), "AKIAIOSFODNN7EXAMPLE")
}
