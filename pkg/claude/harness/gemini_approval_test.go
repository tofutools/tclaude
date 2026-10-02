package harness

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiApprovalCatalog(t *testing.T) {
	h, err := Resolve(GeminiName)
	require.NoError(t, err)
	require.True(t, h.SupportsApproval())
	// The one posture a detached pane cannot deadlock in.
	assert.Equal(t, GeminiApprovalYolo, h.Approval.DefaultPolicy())
	got, err := h.Approval.ValidatePolicy(h.Approval.DefaultPolicy())
	require.NoError(t, err)
	assert.Equal(t, GeminiApprovalYolo, got)

	modes := h.Approval.Modes()
	assert.Equal(t, []string{GeminiApprovalYolo, GeminiApprovalAutoEdit, GeminiApprovalDefault,
		GeminiApprovalPlan, GeminiApprovalInherit}, modes)
	modes[0] = "tampered"
	assert.Equal(t, GeminiApprovalYolo, h.Approval.Modes()[0], "Modes() must not share its backing array")

	for _, mode := range h.Approval.Modes() {
		help := h.Approval.ModeHelp(mode)
		assert.NotEmptyf(t, strings.TrimSpace(help), "ModeHelp(%q)", mode)
		assert.Equalf(t, 1, strings.Count(help, "⚠"), "ModeHelp(%q) needs exactly one ⚠ caveat", mode)
	}
	assert.Empty(t, h.Approval.ModeHelp("no-such-mode"))

	// Gemini's CLI spells it auto_edit; the camelCase settings value is not a
	// flag value.
	_, err = h.Approval.ValidatePolicy("autoEdit")
	assert.Error(t, err)
	_, err = h.Approval.ValidatePolicy("never")
	assert.Error(t, err)
}

func TestGeminiSpawnerRendersTheApprovalMode(t *testing.T) {
	for policy, want := range map[string]string{
		GeminiApprovalYolo:     " --approval-mode=yolo",
		GeminiApprovalAutoEdit: " --approval-mode=auto_edit",
		GeminiApprovalDefault:  " --approval-mode=default",
		GeminiApprovalPlan:     " --approval-mode=plan",
	} {
		cmd := geminiSpawner{}.BuildCommand(SpawnSpec{ApprovalPolicy: policy})
		assert.Containsf(t, cmd, want, "policy %q", policy)
	}
	for _, policy := range []string{"", GeminiApprovalInherit} {
		cmd := geminiSpawner{}.BuildCommand(SpawnSpec{ApprovalPolicy: policy})
		assert.NotContainsf(t, cmd, "approval-mode", "policy %q must emit nothing", policy)
	}
}

func TestGeminiApprovalDenialHintPointsAtAProvableMode(t *testing.T) {
	hint := ApprovalLineageDenialHint(DefaultName, claudePermAuto, false, GeminiName, GeminiApprovalInherit)
	assert.Contains(t, hint, `pass "yolo"`)
	hint = ApprovalLineageDenialHint(GeminiName, GeminiApprovalAutoEdit, false, GeminiName, GeminiApprovalInherit)
	assert.Contains(t, hint, `pass "auto_edit"`)
	assert.Empty(t, ApprovalLineageDenialHint(DefaultName, claudePermAuto, false, GeminiName, GeminiApprovalPlan))
}
