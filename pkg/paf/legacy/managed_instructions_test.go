package legacy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/paf"
)

func TestManagedInstructionsPAFRoundTrip(t *testing.T) {
	b := sampleBundle(t)
	skill, err := json.Marshal(skillbundle.Bundle{Format: skillbundle.Format, Name: "review", Files: []skillbundle.File{{Path: "SKILL.md", Data: []byte("Review carefully")}, {Path: "check.sh", Data: []byte("#!/bin/sh\nexit 0\n"), Executable: true}}})
	require.NoError(t, err)
	b.ManagedInstructions = []db.ManagedInstruction{{Kind: "rule", Name: "operator", Content: []byte("Rule fixture"), Scope: "user"}, {Kind: "skill", Name: "review", Content: skill, Scope: "group:1"}}
	_, err = b.Encode()
	require.ErrorContains(t, err, "require PAF")
	var output bytes.Buffer
	require.NoError(t, WriteAgentBundle(b, &output, WriteOptions{Producer: paf.Producer{Name: "test"}}))
	archive, err := paf.Open(bytes.NewReader(output.Bytes()), int64(output.Len()), paf.Options{})
	require.NoError(t, err)
	require.Contains(t, archive.Manifest.ExtensionsRequired, ManagedInstructionsExtension)
	env, ok := archive.Environment(archive.Manifest.Agents[0])
	require.True(t, ok)
	require.Len(t, env.Skills, 1)
	require.Contains(t, env.Skills[0].Source, "attachment:")
	decoded, _, err := DecodeBundleBytes(output.Bytes(), DecodeLimits{}, "")
	require.NoError(t, err)
	defer decoded.Close()
	require.Len(t, decoded.ManagedInstructions, 2)
	require.Equal(t, b.ManagedInstructions[0].Content, decoded.ManagedInstructions[0].Content)
	require.Equal(t, skill, decoded.ManagedInstructions[1].Content)
	require.Equal(t, "carried", decoded.ManagedInstructions[0].Scope)
	require.NotEmpty(t, decoded.ManagedInstructions[1].Digest)
}

// Agents of one bundle can share a resource; its blob travels once.
func TestBundlePerformersShareAManagedInstruction(t *testing.T) {
	alice, bob := sampleBundle(t), sampleBundle(t)
	alice.Manifest.Agent.Name, bob.Manifest.Agent.Name = "alice", "bob"
	alice.Manifest.MailLedger, bob.Manifest.MailLedger = false, false // only a move carries one
	shared := []db.ManagedInstruction{{Kind: "rule", Name: "operator", Content: []byte("Rule fixture"), Scope: "user"}}
	alice.ManagedInstructions, bob.ManagedInstructions = shared, shared
	opts := WriteOptions{Producer: paf.Producer{Name: "test"}, AddContent: func(w *paf.Writer, _ []string) error {
		if err := w.AddWorkflow("flow", "flow@sha256:aa", nil); err != nil {
			return err
		}
		return w.WriteJSON(paf.WorkflowPath("flow", "workflow.json"), map[string]string{"root": "flow@sha256:aa"})
	}}
	var out bytes.Buffer
	require.NoError(t, WriteBundlePAF([]BundleAgent{{Bundle: alice}, {Bundle: bob}}, &out, opts, agentbundle.MaxBytes))
}
