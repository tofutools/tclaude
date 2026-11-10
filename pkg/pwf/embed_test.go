package pwf_test

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/pwf"
)

// envelopeBundle is a workflow, for carrying in a PAF.
func envelopeBundle(source string) pwf.Bundle {
	_, ref, err := pwf.ParseTemplate([]byte(source))
	if err != nil {
		panic(err)
	}
	return pwf.Bundle{Root: ref, Templates: []pwf.Template{{Ref: ref, Head: true, Source: []byte(source)}}}
}

type envelopeOpts struct {
	kind   string
	bundle pwf.Bundle
	links  func(w *paf.Writer)
	keys   []paf.SigningKey
}

func writeEnvelope(t *testing.T, o envelopeOpts) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, o.kind, paf.Limits{})
	require.NoError(t, w.AddAgent("reviewer", paf.Agent{Name: "reviewer"}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	if o.bundle.Root != "" {
		require.NoError(t, pwf.WriteEmbedded(w, "review-loop", o.bundle))
	}
	if o.links != nil {
		o.links(w)
	}
	_, err := w.Close(paf.Producer{Name: "test"}, o.keys...)
	require.NoError(t, err)
	return buf.Bytes()
}

func performerLink(w *paf.Writer) {
	w.AddLink(paf.Link{Type: paf.LinkPerformer, Workflow: "review-loop", Agent: "reviewer", Role: "reviewer"})
}

func openEnvelope(t *testing.T, raw []byte) (*paf.Archive, error) {
	t.Helper()
	return paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{Purpose: paf.PurposeImport})
}

// A file with workflows says so in its version, and one signature covers both
// sides. A file with none stays exactly what it was.
func TestEnvelopeRoundTrip(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	key := paf.SigningKey{Key: ed25519.NewKeyFromSeed(seed), KeyID: "node", Signer: &paf.Signer{Kind: "instance"}}
	raw := writeEnvelope(t, envelopeOpts{kind: paf.KindBundle, bundle: envelopeBundle(childSource), links: performerLink, keys: []paf.SigningKey{key}})

	env, err := openEnvelope(t, raw)
	require.NoError(t, err)
	assert.Equal(t, paf.EnvelopeVersion, env.Manifest.PafVersion)
	assert.Equal(t, paf.EnvelopeVersion, env.Manifest.MinReaderVersion)
	require.Len(t, env.Manifest.Workflows, 1)
	require.Len(t, env.Performers("review-loop"), 1)
	require.Len(t, env.Signatures, 1)
	assert.Equal(t, paf.SignatureValid, env.Signatures[0].Status)

	a, key2, err := pwf.OpenEnvelope(env, "")
	require.NoError(t, err)
	assert.Equal(t, "review-loop", key2)
	assert.Equal(t, "Review loop", a.Workflow.Title)
	assert.Equal(t, env.Signatures, a.Signatures, "the envelope's signature is the workflow's")

	_, err = pwf.OpenEmbedded(env, "nope")
	require.ErrorContains(t, err, "no workflow")

	plain := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent})
	env, err = openEnvelope(t, plain)
	require.NoError(t, err)
	assert.Equal(t, paf.Version, env.Manifest.PafVersion, "agents alone keep the lowest version")
	assert.Empty(t, env.Manifest.MinReaderVersion)
}

// rewriteEnvelope changes a file's manifest or entries, keeping the entry
// hashes true, the way a forger would.
func rewriteEnvelope(t *testing.T, raw []byte, edit func(m map[string]any, files map[string][]byte)) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		files[f.Name], err = io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(files["manifest.json"], &m))
	edit(m, files)
	files["manifest.json"], err = json.Marshal(m)
	require.NoError(t, err)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for name, data := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return out.Bytes()
}

func TestEnvelopeRefuses(t *testing.T) {
	good := writeEnvelope(t, envelopeOpts{kind: paf.KindBundle, bundle: envelopeBundle(childSource), links: performerLink})
	workflows := func(m map[string]any) map[string]any { return m["workflows"].([]any)[0].(map[string]any) }
	links := func(m map[string]any) map[string]any { return m["links"].([]any)[0].(map[string]any) }
	for name, tc := range map[string]struct {
		edit func(m map[string]any, files map[string][]byte)
		want string
	}{
		"workflow digest": {func(m map[string]any, _ map[string][]byte) { workflows(m)["digest"] = hash0 }, "digest does not match its entries"},
		"link digest":     {func(m map[string]any, _ map[string][]byte) { links(m)["digest"] = hash0 }, "link digest does not match"},
		"link to nobody":  {func(m map[string]any, _ map[string][]byte) { links(m)["agent"] = "ghost" }, `unknown agent "ghost"`},
		"link to nothing": {func(m map[string]any, _ map[string][]byte) { links(m)["workflow"] = "ghost" }, `unknown workflow "ghost"`},
		"role-less link":  {func(m map[string]any, _ map[string][]byte) { delete(links(m), "role") }, "has no role"},
		"two for a role":  {func(m map[string]any, _ map[string][]byte) { m["links"] = append(m["links"].([]any), links(m)) }, "two performers"},
		"duplicate key": {func(m map[string]any, _ map[string][]byte) {
			m["workflows"] = append(m["workflows"].([]any), workflows(m))
		}, "listed twice"},
		"bundle of none":  {func(m map[string]any, _ map[string][]byte) { delete(m, "workflows"); delete(m, "links") }, "bundle PAF has no workflow"},
		"links, no items": {func(m map[string]any, _ map[string][]byte) { delete(m, "workflows"); m["kind"] = "agent" }, "links in a file with no workflows"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openEnvelope(t, rewriteEnvelope(t, good, tc.edit))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const hash0 = "0000000000000000000000000000000000000000000000000000000000000000"

// What a workflow file refuses, the envelope refuses: the workflow reader
// checks the embedded copy the same way.
func TestEnvelopeWorkflowIsCheckedLikeAFile(t *testing.T) {
	raw := writeEnvelope(t, envelopeOpts{kind: paf.KindBundle, bundle: envelopeBundle(childSource), links: performerLink})
	forged := rewriteEnvelope(t, raw, func(m map[string]any, files map[string][]byte) {
		prefix := paf.WorkflowPath("review-loop", pwf.WorkflowPath)
		var wf map[string]any
		require.NoError(t, json.Unmarshal(files[prefix], &wf))
		// Hide the command review a reader would show.
		wf["roles"] = []any{}
		data, err := json.Marshal(wf)
		require.NoError(t, err)
		files[prefix] = data
		entries := m["entries"].([]any)
		for _, e := range entries {
			row := e.(map[string]any)
			if row["path"] == prefix {
				row["bytes"], row["sha256"] = len(data), sha256Hex(data)
			}
		}
		parsed := make([]paf.Entry, 0, len(entries))
		rawEntries, err := json.Marshal(entries)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawEntries, &parsed))
		digest := paf.SectionDigest(parsed, paf.WorkflowPath("review-loop", ""))
		m["workflows"].([]any)[0].(map[string]any)["digest"] = digest
	})
	env, err := openEnvelope(t, forged)
	require.NoError(t, err, "the container checks hold; only the workflow reader knows better")
	_, err = pwf.OpenEmbedded(env, "review-loop")
	require.ErrorContains(t, err, "does not match its templates")
}

// An unknown feature in a workflow refuses that workflow by name.
func TestEnvelopeRefusesUnknownWorkflowFeature(t *testing.T) {
	raw := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent, bundle: envelopeBundle(childSource)})
	forged := rewriteEnvelope(t, raw, func(m map[string]any, _ map[string][]byte) {
		m["workflows"].([]any)[0].(map[string]any)["requires"] = []any{"time-travel"}
	})
	env, err := openEnvelope(t, forged)
	require.NoError(t, err)
	_, err = pwf.OpenEmbedded(env, "review-loop")
	require.ErrorContains(t, err, "time-travel")
}

// A file an older tool wrote with a listed entry under workflows/ and no
// envelope keeps opening: a reader ignores a listed entry it does not know.
func TestUnknownWorkflowsDirInAPlainFileStillOpens(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.AddAgent("reviewer", paf.Agent{Name: "reviewer"}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	require.NoError(t, w.WriteFile("workflows/old/notes.txt", "text/plain", []byte("hi")))
	_, err := w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	_, err = openEnvelope(t, buf.Bytes())
	require.NoError(t, err)
}

// Consent copies a file byte for byte, envelope included.
func TestConsentKeepsTheEnvelope(t *testing.T) {
	raw := writeEnvelope(t, envelopeOpts{kind: paf.KindBundle, bundle: envelopeBundle(childSource), links: performerLink})
	env, err := openEnvelope(t, raw)
	require.NoError(t, err)
	var out bytes.Buffer
	_, err = privacy.AddConsent(env, &out, privacy.ConsentRequest{By: "operator", Audience: privacy.AudienceSelf}, paf.Limits{}, paf.Producer{Name: "test"})
	require.NoError(t, err)
	copied, err := openEnvelope(t, out.Bytes())
	require.NoError(t, err)
	assert.Equal(t, env.Manifest.Workflows, copied.Manifest.Workflows)
	require.Len(t, copied.Performers("review-loop"), 1)
	assert.Equal(t, paf.EnvelopeVersion, copied.Manifest.MinReaderVersion)
}

// The scan sees what a workflow carries, so an import's rescan does too.
func TestScanSeesEmbeddedTemplates(t *testing.T) {
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	source := bytes.Replace([]byte(childSource), []byte("Review {{ params.target }}"), []byte("Use token "+secret), 1)
	raw := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent, bundle: envelopeBundle(string(source))})
	env, err := openEnvelope(t, raw)
	require.NoError(t, err)
	findings, err := privacy.ScanArchive(env, 1<<20)
	require.NoError(t, err)
	assert.True(t, privacy.Blocking(findings), "%+v", findings)
}

func redactEnvelope(t *testing.T, raw []byte, opts privacy.RedactOptions) (*paf.Archive, *privacy.RedactReport, error) {
	t.Helper()
	env, err := openEnvelope(t, raw)
	require.NoError(t, err)
	var out bytes.Buffer
	rep, err := privacy.Redact(env, &out, opts, paf.Producer{Name: "test"})
	if err != nil {
		return nil, nil, err
	}
	copied, err := openEnvelope(t, out.Bytes())
	require.NoError(t, err)
	return copied, rep, nil
}

// A template is addressed by its hash, so a redaction keeps a workflow as it is
// or drops it, and says so. It never leaves one unredacted behind a mask.
func TestRedactionKeepsOrDropsWorkflows(t *testing.T) {
	clean := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent, bundle: envelopeBundle(childSource), links: func(w *paf.Writer) {
		w.AddLink(paf.Link{Type: paf.LinkCarries, Workflow: "review-loop", Agent: "reviewer"})
	}})
	bundle := writeEnvelope(t, envelopeOpts{kind: paf.KindBundle, bundle: envelopeBundle(childSource), links: performerLink})
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	dirty := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent, bundle: envelopeBundle(string(bytes.Replace([]byte(childSource), []byte("Review {{ params.target }}"), []byte("Use "+secret), 1)))})

	copied, _, err := redactEnvelope(t, clean, privacy.RedactOptions{DropNative: true})
	require.NoError(t, err)
	require.Len(t, copied.Manifest.Workflows, 1, "a conversation redaction leaves the workflow")
	require.Len(t, copied.Manifest.Links, 1)
	_, err = pwf.OpenEmbedded(copied, "review-loop")
	require.NoError(t, err)

	copied, _, err = redactEnvelope(t, clean, privacy.RedactOptions{MaskFlagged: true})
	require.NoError(t, err)
	require.Len(t, copied.Manifest.Workflows, 1, "masking leaves a workflow the scan finds nothing in")

	copied, rep, err := redactEnvelope(t, dirty, privacy.RedactOptions{MaskFlagged: true})
	require.NoError(t, err)
	assert.Empty(t, copied.Manifest.Workflows, "a flagged template cannot be masked, so it goes")
	assert.Empty(t, rep.Findings, "the copy holds no credential")
	findings, err := privacy.ScanArchive(copied, 1<<20)
	require.NoError(t, err)
	assert.Empty(t, findings)

	copied, rep, err = redactEnvelope(t, clean, privacy.RedactOptions{PathsToPlaceholders: true})
	require.NoError(t, err)
	assert.Empty(t, copied.Manifest.Workflows, "paths may be anywhere in a template")
	assert.Empty(t, copied.Manifest.Links)
	assert.Equal(t, paf.Version, copied.Manifest.PafVersion, "the copy no longer needs a newer reader")
	require.NotEmpty(t, rep.Redactions)
	assert.Contains(t, rep.Redactions[len(rep.Redactions)-1].Detail, "workflow")

	copied, _, err = redactEnvelope(t, bundle, privacy.RedactOptions{PathsToPlaceholders: true})
	require.NoError(t, err)
	assert.Equal(t, paf.KindAgent, copied.Manifest.Kind, "one agent with no workflow is an agent file")
}

// Redacting or consenting vouches for nothing, so a key that never signed the
// file cannot sign a workflow into passing the unsigned-import policy.
func TestRewritesDoNotVouchForWorkflows(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	key := paf.SigningKey{Key: priv, KeyID: "instance", Signer: &paf.Signer{Kind: "instance"}}
	rewrite := func(t *testing.T, raw []byte) (redacted, consented *paf.Archive) {
		t.Helper()
		a, err := openEnvelope(t, raw)
		require.NoError(t, err)
		var buf bytes.Buffer
		_, err = privacy.Redact(a, &buf, privacy.RedactOptions{DropReasoning: true}, paf.Producer{Name: "test"}, key)
		require.NoError(t, err)
		redacted, err = openEnvelope(t, buf.Bytes())
		require.NoError(t, err)
		var out bytes.Buffer
		_, err = privacy.AddConsent(a, &out, privacy.ConsentRequest{By: "human", Audience: privacy.AudienceSelf}, paf.Limits{}, paf.Producer{Name: "test"}, key)
		require.NoError(t, err)
		consented, err = openEnvelope(t, out.Bytes())
		require.NoError(t, err)
		return redacted, consented
	}

	foreign := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent, bundle: envelopeBundle(childSource)})
	red, con := rewrite(t, foreign)
	require.Len(t, red.Manifest.Workflows, 1)
	assert.Empty(t, red.Signatures)
	assert.Empty(t, con.Signatures)

	own := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent, bundle: envelopeBundle(childSource), keys: []paf.SigningKey{key}})
	red, con = rewrite(t, own)
	assert.True(t, paf.SignedBy(red.Signatures, pub), "a key that signed the file keeps signing it")
	assert.True(t, paf.SignedBy(con.Signatures, pub))

	plain := writeEnvelope(t, envelopeOpts{kind: paf.KindAgent})
	red, _ = rewrite(t, plain)
	assert.True(t, paf.SignedBy(red.Signatures, pub), "without workflows a rewrite signs as before")
}
