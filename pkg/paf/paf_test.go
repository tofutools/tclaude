//ci:reads docs/paf/

package paf

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

const docsDir = "../../docs/paf"

// zipFiles builds an archive from name->content in name order.
func zipFiles(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate})
		require.NoError(t, err)
		_, err = w.Write(files[n])
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	}))
	return files
}

func open(t *testing.T, raw []byte, purpose Purpose) (*Archive, error) {
	t.Helper()
	return Open(bytes.NewReader(raw), int64(len(raw)), Options{Purpose: purpose})
}

// exampleKey is the published example signing key of docs/paf/examples.
func exampleKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
}

func TestBundledSchemasMatchDocs(t *testing.T) {
	docs := readDir(t, filepath.Join(docsDir, "schema/v0.1"))
	bundled := readDir(t, "schema/v0.1")
	assert.Equal(t, docs, bundled, "pkg/paf/schema/v0.1 must be a byte-for-byte copy of docs/paf/schema/v0.1")
}

func TestSpecExamplesOpen(t *testing.T) {
	for _, name := range []string{"definition-only", "full-agent"} {
		t.Run(name, func(t *testing.T) {
			a, err := open(t, zipFiles(t, readDir(t, filepath.Join(docsDir, "examples", name))), PurposeImport)
			require.NoError(t, err)
			assert.Empty(t, a.Unsupported)
			require.Len(t, a.Manifest.Agents, 1)
			key := a.Manifest.Agents[0]
			ag, ok := a.Agent(key)
			require.True(t, ok)
			var tc struct {
				SpawnProfile struct {
					Profile map[string]any `json:"profile"`
				} `json:"spawn_profile"`
			}
			found, err := ag.Extensions.Decode(ExtTclaude, &tc)
			require.NoError(t, err)
			assert.True(t, found)
			assert.NotEmpty(t, tc.SpawnProfile.Profile)
		})
	}
}

func TestFullExampleContentAndSignature(t *testing.T) {
	a, err := open(t, zipFiles(t, readDir(t, filepath.Join(docsDir, "examples/full-agent"))), PurposeImport)
	require.NoError(t, err)
	require.Len(t, a.Signatures, 1)
	assert.Equal(t, SignatureValid, a.Signatures[0].Status)
	assert.True(t, SignedBy(a.Signatures, exampleKey().Public().(ed25519.PublicKey)))
	p := a.Privacy()
	require.NotNil(t, p)
	require.Len(t, p.Consent, 1)
	assert.Equal(t, ContentDigest(a.Manifest.Entries), p.Consent[0].ContentDigest)

	tr, err := ReadTranscript(a, "fixer", ScopeEffective, ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "claude", tr.Source.Harness)
	require.Len(t, tr.Entries, 5)
	var media *transcript.Media
	for _, b := range tr.Entries[1].Blocks {
		if b.Kind == transcript.BlockMedia {
			media = &b.Media[0]
		}
	}
	require.NotNil(t, media)
	assert.True(t, bytes.HasPrefix(media.Data, []byte("\x89PNG")))
	assert.Equal(t, transcript.BlockToolCall, tr.Entries[2].Blocks[2].Kind)
	assert.Equal(t, "anthropic", tr.Entries[2].Blocks[0].Opaque.Vendor)
}

func sampleTranscript() *transcript.Transcript {
	t := transcript.New(transcript.Source{Harness: "codex", Format: "codex-rollout-jsonl", ConvID: "c1", Model: "gpt-5.5", Cwd: "/w", Title: "t"})
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	png := []byte("\x89PNG fake")
	t.Entries = []transcript.Entry{
		{ID: "1", Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at, Blocks: []transcript.Block{transcript.Text("look"), {Kind: transcript.BlockMedia, Media: []transcript.Media{{MediaType: "image/png", Data: png, Name: "a.png"}}}}},
		{ID: "2", Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Model: "gpt-5.5", Blocks: []transcript.Block{
			{Kind: transcript.BlockThinking, Opaque: &transcript.Opaque{Vendor: "openai", Data: "enc"}},
			{Kind: transcript.BlockToolCall, CallID: "k1", Name: "shell", Input: json.RawMessage(`{"cmd":"ls"}`)},
			{Kind: transcript.BlockToolCall, CallID: "k2", Name: "apply_patch", Text: "*** Begin Patch", Freeform: true},
		}},
		{ID: "3", Role: transcript.RoleUser, Origin: transcript.OriginTool, Blocks: []transcript.Block{
			{Kind: transcript.BlockToolResult, CallID: "k1", Text: "a\nb", Media: []transcript.Media{{MediaType: "image/png", Data: png}}},
			{Kind: transcript.BlockToolResult, CallID: "k2", Text: "failed", IsError: true},
		}},
		{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{{Kind: transcript.BlockSummary, Text: "so far"}, transcript.Text("")}},
	}
	t.Losses.Add(transcript.LossReasoningDropped, 2, "")
	return t
}

func writeSample(t *testing.T, mutate func(w *Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, KindAgent, Limits{})
	require.NoError(t, w.DeclareExtension(ExtTclaude, ExtensionDecl{Version: "0.1", System: "tclaude", Spec: "https://example.com/spec"}, false))
	ag := Agent{Name: "worker", Definition: Definition{Role: "worker"}, Native: &Native{Format: "codex-rollout-jsonl", Path: AgentPath("w", "native/codex-rollout-jsonl/history.jsonl"), Harness: "codex"}}
	require.NoError(t, ag.Extensions.Set(ExtTclaude, map[string]any{"spawn_profile": map[string]any{"profile": map[string]any{"harness": "codex"}}}))
	require.NoError(t, w.AddAgent("w", ag, Environment{Harness: Harness{Name: "codex"}}))
	tr := sampleTranscript()
	require.NoError(t, WriteTranscript(w, "w", ScopeEffective, tr))
	require.NoError(t, w.WriteJSON(AgentPath("w", "context/losses.json"), LossesFromReport(tr.Losses)))
	require.NoError(t, w.WriteFile(ag.Native.Path, "application/jsonl", []byte("{\"type\":\"session_meta\"}\n")))
	if mutate != nil {
		mutate(w)
	}
	_, err := w.Close(Producer{Name: "test"}, SigningKey{Key: exampleKey(), KeyID: "k1"})
	require.NoError(t, err)
	return buf.Bytes()
}

func TestWriteReadRoundTrip(t *testing.T) {
	raw := writeSample(t, nil)
	a, err := open(t, raw, PurposeImport)
	require.NoError(t, err)
	assert.Equal(t, SignatureValid, a.Signatures[0].Status)
	got, err := ReadTranscript(a, "w", ScopeEffective, ReadOptions{})
	require.NoError(t, err)
	want := sampleTranscript()
	// The writer drops the empty text block; attachments come back inline.
	want.Entries[3].Blocks = want.Entries[3].Blocks[:1]
	assert.Equal(t, want.Source, got.Source)
	assert.Equal(t, want.Entries, got.Entries)
	l, ok := a.Losses("w")
	require.True(t, ok)
	assert.Equal(t, 2, ReportFromLosses(l).Count(transcript.LossReasoningDropped))
	// One attachment for the two identical images.
	n := 0
	for _, e := range a.Manifest.Entries {
		if strings.HasPrefix(e.Path, AttachmentsDir) {
			n++
		}
	}
	assert.Equal(t, 1, n)
}

// rewrite returns the archive with one file changed (or removed when nil),
// keeping every other file byte for byte.
func rewrite(t *testing.T, raw []byte, edit func(files map[string][]byte)) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		var b bytes.Buffer
		_, err = b.ReadFrom(rc)
		require.NoError(t, err)
		files[f.Name] = b.Bytes()
	}
	edit(files)
	for k, v := range files {
		if v == nil {
			delete(files, k)
		}
	}
	return zipFiles(t, files)
}

// remanifest recomputes the manifest entries after an edit, so a test
// reaches the rule it targets rather than the hash check.
func remanifest(t *testing.T, files map[string][]byte, edit func(m map[string]any)) {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(files[ManifestPath], &m))
	var entries []any
	var names []string
	for n := range files {
		if n != ManifestPath && !strings.HasPrefix(n, SignaturesDir) && files[n] != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		entries = append(entries, map[string]any{"path": n, "bytes": len(files[n]), "sha256": hexSHA256(files[n])})
	}
	m["entries"] = entries
	if edit != nil {
		edit(m)
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	files[ManifestPath] = b
}

func TestOpenRefuses(t *testing.T) {
	base := writeSample(t, nil)
	cases := map[string]func(files map[string][]byte){
		"unlisted entry": func(f map[string][]byte) { f["extra.txt"] = []byte("x") },
		"hash mismatch": func(f map[string][]byte) {
			f[AgentPath("w", "native/codex-rollout-jsonl/history.jsonl")] = []byte("{\"type\":\"session_metX\"}\n")
		},
		"missing listed entry": func(f map[string][]byte) { f[AgentPath("w", "environment.json")] = nil },
		"duplicate manifest key": func(f map[string][]byte) {
			f[ManifestPath] = append([]byte(`{"kind":"agent",`), f[ManifestPath][1:]...)
		},
		"undeclared extension": func(f map[string][]byte) {
			f[AgentPath("w", "environment.json")] = []byte(`{"harness":{"name":"codex"},"extensions":{"com.example.x":{}}}`)
			remanifest(t, f, nil)
		},
		"invalid tclaude extension payload": func(f map[string][]byte) {
			f[AgentPath("w", "environment.json")] = []byte(`{"harness":{"name":"codex"},"extensions":{"tclaude":{"harness":"not-an-object"}}}`)
			remanifest(t, f, nil)
		},
		// An undocumented single-label id is an unknown extension: refused
		// only when required.
		"required unknown single-label extension": func(f map[string][]byte) {
			remanifest(t, f, func(m map[string]any) {
				m["extensions_used"].(map[string]any)["agentlab"] = map[string]any{"version": "1.0", "system": "x", "spec": "x"}
				m["extensions_required"] = []any{"agentlab"}
			})
		},
		// The extension walk is structural, so an escaped key is still seen.
		"undeclared extension behind an escaped key": func(f map[string][]byte) {
			p := AgentPath("w", "context/transcript.jsonl")
			f[p] = bytes.Replace(f[p], []byte(`"role":"assistant"`), []byte(`"role":"assistant","ext\u0065nsions":{"com.example.x":{}}`), 1)
			remanifest(t, f, nil)
		},
		"native outside its agent": func(f map[string][]byte) {
			var a map[string]any
			require.NoError(t, json.Unmarshal(f[AgentPath("w", "agent.json")], &a))
			a["native"].(map[string]any)["path"] = PrivacyPath
			f[AgentPath("w", "agent.json")], _ = json.Marshal(a)
			f[PrivacyPath] = []byte(`{}`)
			remanifest(t, f, nil)
		},
		"attachment not in archive": func(f map[string][]byte) {
			for n := range f {
				if strings.HasPrefix(n, AttachmentsDir) {
					f[n] = nil
				}
			}
			remanifest(t, f, nil)
		},
		"closed enum value": func(f map[string][]byte) {
			p := AgentPath("w", "context/transcript.jsonl")
			f[p] = bytes.Replace(f[p], []byte(`"role":"assistant"`), []byte(`"role":"system"`), 1)
			remanifest(t, f, nil)
		},
		"context without losses": func(f map[string][]byte) {
			f[AgentPath("w", "context/losses.json")] = nil
			remanifest(t, f, nil)
		},
		"unknown major version": func(f map[string][]byte) {
			f[ManifestPath] = bytes.Replace(f[ManifestPath], []byte(`"paf_version": "0.1"`), []byte(`"paf_version": "1.0"`), 1)
		},
		// A deliberate break is marked with min_reader_version.
		"needs a newer reader": func(f map[string][]byte) {
			f[ManifestPath] = bytes.Replace(f[ManifestPath], []byte(`"paf_version": "0.1"`), []byte(`"paf_version": "0.3", "min_reader_version": "0.3"`), 1)
		},
	}
	want := map[string]string{
		"unlisted entry":                             "not listed in the manifest",
		"hash mismatch":                              "hash does not match",
		"missing listed entry":                       "is missing",
		"duplicate manifest key":                     "duplicate key",
		"undeclared extension":                       "used but not declared",
		"required unknown single-label extension":    "requires features this reader does not support: ext:agentlab",
		"undeclared extension behind an escaped key": "used but not declared",
		"invalid tclaude extension payload":          `extension "tclaude"`,
		"native outside its agent":                   "outside its native directory",
		"attachment not in archive":                  "is not in the archive",
		"closed enum value":                          `unknown role "system"`,
		"context without losses":                     "no losses.json",
		"unknown major version":                      "unsupported PAF version 1.0",
		"needs a newer reader":                       "this file needs PAF >= 0.3 (written by test as PAF 0.3); this reader supports PAF 0.2",
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := open(t, rewrite(t, base, edit), PurposeImport)
			assert.ErrorContains(t, err, want[name])
		})
	}
	t.Run("unknown single-label extension is ignored", func(t *testing.T) {
		raw := rewrite(t, base, func(f map[string][]byte) {
			f[AgentPath("w", "environment.json")] = []byte(`{"harness":{"name":"codex"},"extensions":{"agentlab":{"x":1}}}`)
			remanifest(t, f, func(m map[string]any) {
				m["extensions_used"].(map[string]any)["agentlab"] = map[string]any{"version": "1.0", "system": "x", "spec": "x"}
			})
		})
		_, err := open(t, raw, PurposeImport)
		assert.NoError(t, err)
	})
	t.Run("version error names producer", func(t *testing.T) {
		_, err := open(t, rewrite(t, base, cases["unknown major version"]), PurposeImport)
		var ve *VersionError
		require.True(t, errors.As(err, &ve))
		assert.Equal(t, "test", ve.Producer.Name)
	})
	// Minor versions only add, from 0.1 on: a newer 0.y with fields this
	// reader does not know imports.
	t.Run("newer minor version imports", func(t *testing.T) {
		raw := rewrite(t, base, func(f map[string][]byte) {
			f[ManifestPath] = bytes.Replace(f[ManifestPath], []byte(`"paf_version": "0.1"`), []byte(`"paf_version": "0.7", "added_in_0_7": {"x": 1}`), 1)
		})
		a, err := open(t, raw, PurposeImport)
		require.NoError(t, err)
		assert.Empty(t, a.Warnings)
	})
	// Inspection reads a file that needs a newer reader best effort, and
	// says so.
	t.Run("inspect reads a newer file best effort", func(t *testing.T) {
		for _, name := range []string{"needs a newer reader", "unknown major version"} {
			a, err := open(t, rewrite(t, base, cases[name]), PurposeInspect)
			require.NoError(t, err, name)
			require.NotEmpty(t, a.Warnings, name)
			assert.Contains(t, a.Warnings[0], "read best effort", name)
		}
	})
}

func TestBadEntryNames(t *testing.T) {
	for _, name := range []string{"../evil", "a/../b", "/abs", "a//b", `a\b`, "./x"} {
		raw := zipFiles(t, map[string][]byte{name: []byte("x"), ManifestPath: []byte(`{}`)})
		_, err := open(t, raw, PurposeInspect)
		assert.ErrorContains(t, err, "invalid entry name", name)
	}
}

func TestLocalHeaderMismatchRefused(t *testing.T) {
	raw := writeSample(t, nil)
	// Rename the manifest in its local header only.
	i := bytes.Index(raw, []byte("PK\x03\x04"))
	for i >= 0 {
		n := int(raw[i+26]) | int(raw[i+27])<<8
		if string(raw[i+30:i+30+n]) == ManifestPath {
			copy(raw[i+30:], "manifesT.json")
			break
		}
		j := bytes.Index(raw[i+4:], []byte("PK\x03\x04"))
		if j < 0 {
			t.Fatal("manifest local header not found")
		}
		i += 4 + j
	}
	_, err := open(t, raw, PurposeInspect)
	assert.ErrorContains(t, err, "local header")
}

func TestRequiredFeaturesRefuseImportDegradeInspect(t *testing.T) {
	for _, edit := range []func(m map[string]any){
		func(m map[string]any) { m["requires"] = []any{"context-delta"} },
		func(m map[string]any) {
			m["extensions_used"].(map[string]any)["com.example.future"] = map[string]any{"version": "1.0", "system": "x", "spec": "x"}
			m["extensions_required"] = []any{"com.example.future"}
		},
	} {
		raw := rewrite(t, writeSample(t, nil), func(f map[string][]byte) { remanifest(t, f, edit) })
		_, err := open(t, raw, PurposeImport)
		assert.ErrorContains(t, err, "requires features")
		a, err := open(t, raw, PurposeInspect)
		require.NoError(t, err)
		assert.Len(t, a.Unsupported, 1)
		// The manifest changed, so the old signature no longer verifies.
		assert.Equal(t, SignatureInvalid, a.Signatures[0].Status)
	}
}

func TestUnknownContentIsKeptVisible(t *testing.T) {
	raw := rewrite(t, writeSample(t, nil), func(f map[string][]byte) {
		p := AgentPath("w", "context/transcript.jsonl")
		f[p] = append(f[p], []byte(`{"type":"entry","role":"assistant","origin":"agent","blocks":[{"kind":"citation","text":"see RFC 9110"}]}`+"\n"+`{"type":"checkpoint","id":"x"}`+"\n")...)
		f[SignaturesDir+"future.json"] = []byte(`{"alg":"ml-dsa-65","public_key":"???","key_id":"future","manifest_sha256":"x","signature":"???"}`)
		remanifest(t, f, nil)
	})
	a, err := open(t, raw, PurposeImport)
	require.NoError(t, err)
	statuses := map[string]SignatureStatus{}
	for _, s := range a.Signatures {
		statuses[s.Path] = s.Status
	}
	assert.Equal(t, SignatureUnsupported, statuses[SignaturesDir+"future.json"])
	tr, err := ReadTranscript(a, "w", ScopeEffective, ReadOptions{})
	require.NoError(t, err)
	last := tr.Entries[len(tr.Entries)-1]
	assert.Contains(t, last.Blocks[0].Text, "[unsupported content: citation block]")
	assert.Contains(t, last.Blocks[0].Text, "see RFC 9110")
	assert.Equal(t, 2, tr.Losses.Count(transcript.LossUnknownRecord))
}

func TestWriterRefusesUndeclaredExtension(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, KindAgent, Limits{})
	ag := Agent{Name: "a"}
	require.NoError(t, ag.Extensions.Set("com.example.x", map[string]any{"k": 1}))
	require.NoError(t, w.AddAgent("a", ag, Environment{Harness: Harness{Name: "claude"}}))
	_, err := w.Close(Producer{Name: "test"})
	assert.ErrorContains(t, err, "not declared")
}

func TestContentDigestOrderIndependent(t *testing.T) {
	e := []Entry{{Path: "b", SHA256: "2"}, {Path: "a", SHA256: "1"}, {Path: PrivacyPath, SHA256: "3"}}
	assert.Equal(t, ContentDigest(e), ContentDigest([]Entry{e[1], e[0]}))
	assert.Equal(t, hexSHA256([]byte("1  a\n2  b\n")), ContentDigest(e))
}

func TestLandingRungFromFallback(t *testing.T) {
	assert.Equal(t, RungTranslated, LandingRungFromFallback("native"))
	assert.Equal(t, RungContextFile, LandingRungFromFallback("handoff"))
	assert.Equal(t, "", LandingRungFromFallback("other"))
}

// rezip rebuilds an archive from its entries, letting edit adjust each
// header (and so the local header Go writes).
func rezip(t *testing.T, raw []byte, edit func(h *zip.FileHeader)) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		var b bytes.Buffer
		_, err = b.ReadFrom(rc)
		require.NoError(t, err)
		h := &zip.FileHeader{Name: f.Name, Method: f.Method}
		edit(h)
		w, err := zw.CreateHeader(h)
		require.NoError(t, err)
		_, err = w.Write(b.Bytes())
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// A forged local header planted at the end of a large extra field must not
// stand in for the real one: the reader reads the header at its real offset.
func TestLocalHeaderForgedInsideLargeExtraRefused(t *testing.T) {
	fake := make([]byte, 30+len(ManifestPath))
	binary.LittleEndian.PutUint32(fake, sigLocal)
	binary.LittleEndian.PutUint16(fake[6:], 0x8)
	binary.LittleEndian.PutUint16(fake[8:], zip.Deflate)
	binary.LittleEndian.PutUint16(fake[26:], uint16(len(ManifestPath)))
	copy(fake[30:], ManifestPath)
	body := append(make([]byte, 5000-len(fake)), fake...)
	extra := append([]byte{0xfe, 0xca, 0, 0}, body...)
	binary.LittleEndian.PutUint16(extra[2:], uint16(len(body)))
	raw := rezip(t, writeSample(t, nil), func(h *zip.FileHeader) {
		if h.Name == ManifestPath {
			h.Extra = extra
		}
	})
	// Unchanged, the large extra field is legal.
	_, err := open(t, raw, PurposeImport)
	require.NoError(t, err)
	// Rename the real local header; only the planted one still says manifest.json.
	real := -1
	for i := 0; i+30+len(ManifestPath) <= len(raw); i++ {
		if binary.LittleEndian.Uint32(raw[i:]) == sigLocal && string(raw[i+30:i+30+len(ManifestPath)]) == ManifestPath && int(binary.LittleEndian.Uint16(raw[i+28:])) == len(extra) {
			real = i
			break
		}
	}
	require.GreaterOrEqual(t, real, 0)
	copy(raw[real+30:], "evil_____.txt")
	_, err = open(t, raw, PurposeInspect)
	assert.ErrorContains(t, err, "local header does not match")
}

func TestDataDescriptorMismatchRefused(t *testing.T) {
	raw := writeSample(t, nil)
	i := bytes.Index(raw, []byte{0x50, 0x4b, 7, 8})
	require.Positive(t, i, "Go writes data descriptors")
	raw[i+4] ^= 0xff // corrupt the descriptor CRC
	_, err := open(t, raw, PurposeInspect)
	assert.ErrorContains(t, err, "local header does not match")
}

func TestReadTranscriptSharesAttachmentsWithinBudget(t *testing.T) {
	png := bytes.Repeat([]byte{7}, 1000)
	tr := transcript.New(transcript.Source{Harness: "claude"})
	for range 50 {
		tr.Entries = append(tr.Entries, transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{{Kind: transcript.BlockMedia, Media: []transcript.Media{{MediaType: "image/png", Data: png}}}}})
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, KindAgent, Limits{})
	require.NoError(t, w.AddAgent("a", Agent{Name: "a"}, Environment{Harness: Harness{Name: "claude"}}))
	require.NoError(t, WriteTranscript(w, "a", ScopeEffective, tr))
	require.NoError(t, w.WriteJSON(AgentPath("a", "context/losses.json"), LossesFromReport(tr.Losses)))
	_, err := w.Close(Producer{Name: "test"})
	require.NoError(t, err)
	a, err := open(t, buf.Bytes(), PurposeImport)
	require.NoError(t, err)

	got, err := ReadTranscript(a, "a", ScopeEffective, ReadOptions{})
	require.NoError(t, err)
	first := got.Entries[0].Blocks[0].Media[0].Data
	last := got.Entries[49].Blocks[0].Media[0].Data
	assert.Equal(t, png, last)
	assert.Same(t, &first[0], &last[0], "one attachment is loaded once and shared")

	got, err = ReadTranscript(a, "a", ScopeEffective, ReadOptions{MaxMediaBytes: 999})
	require.NoError(t, err)
	assert.Equal(t, 50, got.Losses.Count(transcript.LossMediaDropped), "over the per-item budget")
}

func TestWriteTranscriptRecordsDrops(t *testing.T) {
	tr := transcript.New(transcript.Source{Harness: "claude"})
	tr.Entries = []transcript.Entry{
		{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("hi"), {Kind: transcript.BlockMedia, Media: []transcript.Media{{MediaType: "image/png"}}}}},
		{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{transcript.Text("")}},
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, KindAgent, Limits{})
	require.NoError(t, w.AddAgent("a", Agent{Name: "a"}, Environment{Harness: Harness{Name: "claude"}}))
	require.NoError(t, WriteTranscript(w, "a", ScopeEffective, tr))
	assert.Equal(t, 1, tr.Losses.Count(transcript.LossMediaDropped))
	assert.Equal(t, 1, tr.Losses.Count(transcript.LossUnknownRecord))
}

func TestWriterRefusesWhatReaderRejects(t *testing.T) {
	newW := func() (*Writer, *bytes.Buffer) {
		var buf bytes.Buffer
		return NewWriter(&buf, KindAgent, Limits{}), &buf
	}
	t.Run("context without losses", func(t *testing.T) {
		w, _ := newW()
		require.NoError(t, w.AddAgent("a", Agent{Name: "a"}, Environment{Harness: Harness{Name: "claude"}}))
		require.NoError(t, WriteTranscript(w, "a", ScopeEffective, sampleTranscript()))
		_, err := w.Close(Producer{Name: "t"})
		assert.ErrorContains(t, err, "no losses.json")
	})
	t.Run("native declared but not written", func(t *testing.T) {
		w, _ := newW()
		require.NoError(t, w.AddAgent("a", Agent{Name: "a", Native: &Native{Format: "claude-jsonl", Path: AgentPath("a", "native/claude-jsonl/h.jsonl")}}, Environment{Harness: Harness{Name: "claude"}}))
		_, err := w.Close(Producer{Name: "t"})
		assert.ErrorContains(t, err, "not written")
	})
	t.Run("schema-invalid metadata", func(t *testing.T) {
		w, _ := newW()
		err := w.WriteJSON(PrivacyPath, map[string]any{"consent": []any{map[string]any{"by": "x"}}})
		assert.Error(t, err)
	})
	t.Run("group member missing", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewWriter(&buf, KindGroup, Limits{})
		require.NoError(t, w.AddAgent("a", Agent{Name: "a"}, Environment{Harness: Harness{Name: "claude"}}))
		require.NoError(t, w.WriteJSON(GroupPath, Group{Name: "g", Members: []GroupMember{{Agent: "b"}}}))
		_, err := w.Close(Producer{Name: "t"})
		assert.ErrorContains(t, err, `group member "b"`)
	})
}

func TestWriteContextFileStoresMediaByPath(t *testing.T) {
	tr := sampleTranscript()
	var stored [][]byte
	var buf bytes.Buffer
	require.NoError(t, WriteContextFile(&buf, ScopeFull, tr, func(m *transcript.Media) (string, error) {
		stored = append(stored, m.Data)
		return "/artifacts/media-" + string(rune('0'+len(stored))), nil
	}))
	require.NotEmpty(t, stored)
	cr, err := NewContextReader(&buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "codex", cr.Header.Source.Harness)
	l, err := cr.Next()
	require.NoError(t, err)
	require.Len(t, l.Blocks, 2)
	require.Len(t, l.Blocks[1].Media, 1)
	m := l.Blocks[1].Media[0]
	assert.Equal(t, "/artifacts/media-1", m.Path)
	assert.Equal(t, "image/png", m.MediaType)
	assert.Equal(t, "a.png", m.Name)
	assert.Empty(t, m.Attachment)
}

func TestWriterRefusesContextLineOverRecordLimit(t *testing.T) {
	tr := transcript.New(transcript.Source{Harness: "claude"})
	tr.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text(strings.Repeat("x", 4096))}}}
	var buf bytes.Buffer
	w := NewWriter(&buf, KindAgent, Limits{MaxRecordBytes: 1024})
	require.NoError(t, w.AddAgent("a", Agent{Name: "a"}, Environment{Harness: Harness{Name: "claude"}}))
	require.ErrorIs(t, WriteTranscript(w, "a", ScopeEffective, tr), ErrRecordTooLarge)
}

// Minor versions only add, from 0.1 on, so a reader reads every minor of
// its own major.
func TestVersionReadable(t *testing.T) {
	for v, want := range map[string]bool{"0.1": true, "0.0": true, "0.2": true, "1.0": false, "x": false} {
		assert.Equal(t, want, versionReadable(v, 0), v)
	}
	assert.True(t, versionReadable("1.7", 1))
	assert.False(t, versionReadable("2.0", 1))
	assert.True(t, versionBefore("0.1", "0.2"))
	assert.True(t, versionBefore("0.9", "1.0"))
	assert.False(t, versionBefore("0.2", "0.2"))
	assert.False(t, versionBefore("0.3", "0.2"))
}

func TestNegotiateVersion(t *testing.T) {
	assert.Equal(t, Version, NegotiateVersion([]string{"9.0", Version}))
	assert.Empty(t, NegotiateVersion([]string{"9.0"}))
	assert.Empty(t, NegotiateVersion(nil))
}

// Earlier loss reports travel in prior rather than being overwritten.
func TestLossesKeepPrior(t *testing.T) {
	raw := rewrite(t, writeSample(t, nil), func(f map[string][]byte) {
		f[AgentPath("w", "context/losses.json")] = []byte(`{"source":"codex","target":"claude","items":[],"prior":[{"source":"claude","target":"codex","dropped_percent":40,"kept_percent":60,"items":[{"kind":"truncated_for_budget","severity":"warn","count":3}]}]}`)
		remanifest(t, f, nil)
	})
	a, err := open(t, raw, PurposeImport)
	require.NoError(t, err)
	l, ok := a.Losses("w")
	require.True(t, ok)
	require.Len(t, l.Prior, 1)
	assert.Equal(t, 40, l.Prior[0].DroppedPercent)
}

func TestManagedInstructionsOlderReaderRefusesOnlyRequiredArchives(t *testing.T) {
	const extension = "org.tclaude.managed-instructions"
	var out bytes.Buffer
	writer := NewWriter(&out, KindAgent, Limits{})
	require.NoError(t, writer.DeclareExtension(extension, ExtensionDecl{Version: "1.0", System: "tclaude", Spec: "https://example.invalid/managed", Copy: "unsafe"}, true))
	require.NoError(t, writer.AddAgent("worker", Agent{Name: "worker"}, Environment{Harness: Harness{Name: "claude"}}))
	_, closeErr := writer.Close(Producer{Name: "test"})
	require.NoError(t, closeErr)
	_, err := Open(bytes.NewReader(out.Bytes()), int64(out.Len()), Options{})
	require.NoError(t, err)
	capability := knownExtensions[extension]
	delete(knownExtensions, extension)
	t.Cleanup(func() { knownExtensions[extension] = capability })
	_, err = Open(bytes.NewReader(out.Bytes()), int64(out.Len()), Options{Purpose: PurposeImport})
	require.ErrorContains(t, err, "ext:"+extension)
	inspect, err := Open(bytes.NewReader(out.Bytes()), int64(out.Len()), Options{Purpose: PurposeInspect})
	require.NoError(t, err)
	require.Contains(t, inspect.Unsupported, "ext:"+extension)
}
