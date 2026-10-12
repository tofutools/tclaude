package privacy

import (
	"bytes"
	"encoding/json"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func identifiedSample(t *testing.T) *paf.Archive {
	t.Helper()
	tr := transcript.New(transcript.Source{Harness: "claude", Format: "claude-jsonl", ConvID: "conv-7f3a", Cwd: project, Title: "Fix the billing bug"})
	tr.Entries = []transcript.Entry{
		{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("use " + ghToken + " in " + project)}},
		{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{transcript.Text("done; I am agt_5e1f on alice-laptop, conversation conv-7f3a, for payments-team, fix-acme-billing-outage")}},
	}
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.DeclareExtension(paf.ExtTclaude, paf.ExtensionDecl{Version: "0.1", System: "tclaude", Spec: "https://example.invalid/tclaude"}, false))
	ag := paf.Agent{ID: "agt_5e1f", Name: "fix-acme-billing-outage", Title: "Billing reviewer",
		Instance:     &paf.Instance{ID: "inst_9c2d", Name: "alice-laptop"},
		Conversation: &paf.Conversation{ID: "conv-7f3a", Title: "Fix the billing bug", Harness: "claude"},
		Groups:       []paf.GroupRef{{Name: "payments-team"}},
		Lineage:      []paf.LineageStep{{Op: paf.OpSnapshot, At: time.Now().UTC(), From: &paf.Ref{Instance: "inst_9c2d", Agent: "agt_5e1f", Conversation: "conv-7f3a"}}},
		Definition:   paf.Definition{Role: "reviewer", License: "CC-BY-4.0", Task: &paf.Task{URL: "https://linear.example/acme/issue/ACM-123", Label: "ACM-123"}}}
	require.NoError(t, ag.Extensions.Set(paf.ExtTclaude, map[string]any{"agent_id": "agt_5e1f"}))
	require.NoError(t, w.AddAgent("fix-acme-billing-outage", ag, paf.Environment{Harness: paf.Harness{Name: "claude"}, Workspace: &paf.Workspace{Cwd: project, RepoURL: "git@git.corp.example:team/private-svc.git", Branch: "jdoe/hotfix", Commit: "0123456789abcdef"}}))
	require.NoError(t, paf.WriteTranscript(w, "fix-acme-billing-outage", paf.ScopeEffective, tr))
	require.NoError(t, w.WriteJSON(paf.AgentPath("fix-acme-billing-outage", "context/losses.json"), paf.LossesFromReport(tr.Losses)))
	_, err := w.Close(paf.Producer{Name: "test"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{})
	require.NoError(t, err)
	return a
}

func TestPublishStripsIdentityAndSignsAsPublisher(t *testing.T) {
	scratch, err := os.CreateTemp(testutil.CanonicalTempDir(t), "scratch-")
	require.NoError(t, err)
	defer func() { _ = scratch.Close() }()
	p, err := PreparePublication(identifiedSample(t), scratch, PublishOptions{}, paf.Producer{Name: "test"}, 1<<20)
	require.NoError(t, err)
	assert.Contains(t, redactionKinds(p.Report.Redactions), RedactStripIdentity)
	assert.NotEmpty(t, p.Summary.Digest)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key := paf.SigningKey{Key: priv, KeyID: "pub_test", Signer: &paf.Signer{Kind: "publisher"}}
	assert.Error(t, FinishPublication(p, &bytes.Buffer{}, paf.Producer{Name: "test"}, paf.SigningKey{Key: priv, KeyID: "x", Signer: &paf.Signer{Kind: "instance"}}, paf.Limits{}))
	var out bytes.Buffer
	require.NoError(t, FinishPublication(p, &out, paf.Producer{Name: "test"}, key, paf.Limits{}))

	b, err := paf.Open(bytes.NewReader(out.Bytes()), int64(out.Len()), paf.Options{})
	require.NoError(t, err)
	assert.True(t, paf.SignedBy(b.Signatures, pub))
	assert.Len(t, b.Signatures, 1, "signed by the publisher key only")
	text := allText(t, b)
	for _, leaked := range []string{"agt_5e1f", "inst_9c2d", "alice-laptop", "conv-7f3a", "Fix the billing bug", "Billing reviewer", "payments-team", "fix-acme-billing-outage", "private-svc", "jdoe", "0123456789abcdef", "ACM-123", ghToken, home} {
		assert.NotContains(t, text, leaked)
	}
	manifest, err := json.Marshal(b.Manifest)
	require.NoError(t, err)
	assert.NotContains(t, string(manifest), "fix-acme-billing-outage", "the agent key is not the conversation title")
	assert.Equal(t, []string{"agent"}, b.Manifest.Agents)
	assert.True(t, b.HasContext("agent", paf.ScopeEffective), "the conversation is kept under the new key")
	assert.Contains(t, text, "CC-BY-4.0", "the licence is kept")
	assert.NotContains(t, b.Manifest.ExtensionsUsed, paf.ExtTclaude, "extension data is dropped")
	pr := b.Privacy()
	require.NotNil(t, pr)
	require.Len(t, pr.Consent, 1)
	assert.Equal(t, PublisherConsentBy, pr.Consent[0].By)
	assert.Equal(t, AudiencePublic, pr.Consent[0].Audience)
	assert.Equal(t, p.Summary.Digest, pr.Consent[0].ContentDigest)
	for _, c := range CheckConsent(b) {
		assert.True(t, c.Current)
	}
}
