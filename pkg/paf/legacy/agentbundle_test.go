package legacy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func sampleBundle(t *testing.T) *agentbundle.Bundle {
	t.Helper()
	dirty := true
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{
		Format:         agentbundle.Format,
		FormatVersion:  1,
		CreatedAt:      "2026-10-11T19:40:00Z",
		TclaudeVersion: "v0.0.2200",
		MailLedger:     true,
		Placeholders:   []configbundle.Placeholder{{Name: "PROJECT", Item: "profiles/worker", Field: "cwd", Original: "/home/me/git"}},
		Warnings:       []string{"copilot history skipped"},
		Findings:       []agentbundle.Finding{{Kind: "github_token", Count: 1, Locations: []string{"history line 3"}}},
		Agent: agentbundle.Definition{
			Identity:         &db.FederationIdentity{Agent: "agt_1", Home: "inst_a", Hops: 1, Proofs: map[string]string{}},
			Origin:           &agentbundle.Origin{Instance: "inst_a", Agent: "agt_1", Trigger: "teleport", Model: "opus", Commit: "abc123", Dirty: &dirty},
			CarryPermissions: true,
			Name:             "Fix Worker",
			Role:             "worker",
			Description:      "fixes things",
			Harness:          "claude",
			ProfileName:      "worker-opus",
			Profile:          json.RawMessage(`{"harness":"claude","model":"opus"}`),
			Groups:           []agentbundle.Group{{Name: "tclaude", Role: "worker"}},
			StartupContext:   "use worktrees",
			InitialMessage:   "fix the flake",
			TaskURL:          "https://example.com/42",
			TaskLabel:        "issue 42",
			Paths:            agentbundle.Paths{RepoURL: "https://github.com/x/y.git", Cwd: "/home/me/git/y", Branch: "fix"},
			Permissions:      []agentbundle.Permission{{Slug: "self.rename", Effect: "allow", Source: "default"}},
		},
	}}
	b.MailDeliveries = []db.FederationMailDelivery{{Sender: "agt_2", Envelope: "env1", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}}
	b.SetHistory("claude-jsonl", "conv-1", []byte("{\"type\":\"user\",\"sessionId\":\"conv-1\"}\n"))
	return b
}

func TestAgentBundleV1OpensAsPAF(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	b := sampleBundle(t)
	raw, err := b.Encode()
	require.NoError(t, err)
	path := filepath.Join(dir, "agent.zip")
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	o, err := OpenFile(path, dir, paf.Producer{Name: "tclaude", Version: "test"}, paf.Options{})
	require.NoError(t, err)
	defer o.Close()
	assert.Equal(t, SourceAgentBundleV1, o.Source)

	key := o.Manifest.Agents[0]
	assert.Equal(t, "fix-worker", key)
	ag, _ := o.Agent(key)
	assert.Equal(t, "Fix Worker", ag.Name)
	assert.Equal(t, "agt_1", ag.ID)
	assert.Equal(t, "inst_a", ag.Instance.ID)
	assert.Equal(t, "fix the flake", ag.Definition.InitialMessage)
	assert.Equal(t, &paf.Task{URL: "https://example.com/42", Label: "issue 42"}, ag.Definition.Task)
	assert.Equal(t, []paf.GroupRef{{Name: "tclaude", Role: "worker"}}, ag.Groups)

	var ext AgentExt
	ok, err := ag.Extensions.Decode(paf.ExtTclaude, &ext)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "worker-opus", ext.SpawnProfile.Name)
	assert.JSONEq(t, `{"harness":"claude","model":"opus"}`, string(ext.SpawnProfile.Profile))
	assert.Equal(t, "self.rename", ext.Permissions[0].Slug)
	assert.True(t, ext.Transfer.CarryPermissions)
	assert.Equal(t, "teleport", ext.Transfer.Trigger)
	assert.Equal(t, "agt_1", ext.Transfer.Identity.Agent)

	env, _ := o.Environment(key)
	assert.Equal(t, "claude", env.Harness.Name)
	assert.Equal(t, "opus", env.Model.ID)
	assert.Equal(t, "abc123", env.Workspace.Commit)
	assert.True(t, *env.Workspace.Dirty)
	var phx PlaceholderExt
	_, err = env.Placeholders[0].Extensions.Decode(paf.ExtTclaude, &phx)
	require.NoError(t, err)
	assert.Equal(t, PlaceholderExt{Item: "profiles/worker", Field: "cwd"}, phx)

	native, err := o.ReadEntry(ag.Native.Path, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, b.Transcript, native)
	assert.Equal(t, "conv-1", ag.Native.SourceConvID)

	ledger, err := o.ReadEntry(MailLedgerPath, 1<<20)
	require.NoError(t, err)
	var got []db.FederationMailDelivery
	require.NoError(t, json.Unmarshal(ledger, &got))
	assert.Equal(t, b.MailDeliveries, got)

	require.NotNil(t, o.Privacy())
	assert.Equal(t, "github_token", o.Privacy().Scan.Findings[0].Kind)
	assert.False(t, o.HasContext(key, paf.ScopeEffective), "v1 bundles carry no neutral context")

	var conv map[string]ConvertedFrom
	_, err = o.Manifest.Extensions.Decode(paf.ExtTclaude, &conv)
	require.NoError(t, err)
	assert.Equal(t, ConvertedFrom{Format: agentbundle.Format, FormatVersion: 1, TclaudeVersion: "v0.0.2200", CreatedAt: "2026-10-11T19:40:00Z"}, conv["converted_from"])

	require.NoError(t, o.Close())
	left, err := filepath.Glob(filepath.Join(dir, ".paf-legacy-*"))
	require.NoError(t, err)
	assert.Empty(t, left, "Close removes the converted temporary PAF")
}

func TestOpenFileReadsPAF(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	f, err := os.Create(filepath.Join(dir, "a.paf"))
	require.NoError(t, err)
	require.NoError(t, FromAgentBundle(sampleBundle(t), f, paf.Producer{Name: "tclaude"}))
	require.NoError(t, f.Close())
	o, err := OpenFile(f.Name(), dir, paf.Producer{Name: "tclaude"}, paf.Options{})
	require.NoError(t, err)
	defer o.Close()
	assert.Equal(t, paf.Format, o.Source)
	ag, _ := o.Agent(o.Manifest.Agents[0])
	native, err := o.ReadEntry(ag.Native.Path, 1<<20)
	require.NoError(t, err)
	assert.Contains(t, string(native), "conv-1")
}

func TestAgentKey(t *testing.T) {
	assert.Equal(t, "fix-worker", AgentKey("Fix Worker"))
	assert.Equal(t, "agent", AgentKey("!!!"))
	assert.Equal(t, "a-b", AgentKey("--a__b--"))
}

// The fallback that leaves the neutral context out must not bring the text back.
func TestWriteAgentBundleLossCountsOnlyWithoutContext(t *testing.T) {
	earlier := &paf.Losses{Source: "codex", ContextWindowSource: "text", Items: []paf.LossItem{{Kind: "unknown_record", Severity: "warn", Count: 1, Detail: "text"}}}
	var out bytes.Buffer
	require.NoError(t, WriteAgentBundle(sampleBundle(t), &out, WriteOptions{Producer: paf.Producer{Name: "tclaude"}, EarlierLosses: earlier, LossCountsOnly: true}))
	a, err := paf.Open(bytes.NewReader(out.Bytes()), int64(out.Len()), paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	got, ok := a.Losses(a.Manifest.Agents[0])
	require.True(t, ok)
	assert.Equal(t, []paf.LossItem{{Kind: "unknown_record", Severity: "warn", Count: 1}}, got.Items)
	assert.Empty(t, got.ContextWindowSource)
}
