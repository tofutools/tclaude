package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func boolPtr(b bool) *bool { return &b }

// mergeProfileIntoSpawn is the CLI-side flatten of a spawn profile under the
// explicit flags (JOH-210). These cover the precedence contract the operator
// asked for — explicit flag > --profile > blank — and the harness-match gate
// that keeps a foreign-harness profile's launch config off a pinned spawn.

// A nil profile is a faithful pass-through of the flags — the no-`--profile`
// path must behave exactly as before the feature.
func TestMergeProfileIntoSpawn_NilProfilePassthrough(t *testing.T) {
	p := &SpawnParams{
		Name:       "worker",
		Role:       "dev",
		Descr:      "a worker",
		Harness:    "codex",
		Model:      "gpt-5-codex",
		Effort:     "high",
		Sandbox:    "read-only",
		AutoReview: true,
		AutoFocus:  true,
	}
	got := mergeProfileIntoSpawn(p, "explicit brief", nil)

	assert.Equal(t, "worker", got.Name)
	assert.Equal(t, "dev", got.Role)
	assert.Equal(t, "a worker", got.Descr)
	assert.Equal(t, "codex", got.Harness)
	assert.Equal(t, "gpt-5-codex", got.Model)
	assert.Equal(t, "high", got.Effort)
	assert.Equal(t, "read-only", got.Sandbox)
	assert.Equal(t, "explicit brief", got.InitialMessage)
	assert.True(t, got.AutoReview)
	assert.True(t, got.AutoFocus)
	assert.False(t, got.TrustDir, "no profile ⇒ no trust_dir")
	assert.False(t, got.IsOwner)
	assert.Nil(t, got.PermissionOverrides)
	assert.Nil(t, got.IncludeGroupContext, "no flag, no profile ⇒ default include")
}

// With no flags set, the profile fills every field.
func TestMergeProfileIntoSpawn_ProfileFillsBlanks(t *testing.T) {
	prof := &profileJSON{
		Harness:             "codex",
		Model:               "gpt-5-codex",
		Effort:              "medium",
		Sandbox:             "workspace-write",
		Approval:            "never",
		AutoReview:          boolPtr(true),
		TrustDir:            boolPtr(true),
		AgentName:           "reviewer",
		Role:                "qa",
		Descr:               "from profile",
		InitialMessage:      "profile brief",
		AutoFocus:           boolPtr(true),
		IsOwner:             boolPtr(true),
		PermissionOverrides: map[string]string{"human.notify": "grant"},
	}
	got := mergeProfileIntoSpawn(&SpawnParams{}, "", prof)

	assert.Equal(t, "codex", got.Harness)
	assert.Equal(t, "gpt-5-codex", got.Model)
	assert.Equal(t, "medium", got.Effort)
	assert.Equal(t, "workspace-write", got.Sandbox)
	assert.Equal(t, "never", got.Approval)
	assert.True(t, got.AutoReview)
	assert.True(t, got.TrustDir)
	assert.Equal(t, "reviewer", got.Name, "agent_name pre-fills --name")
	assert.Equal(t, "qa", got.Role)
	assert.Equal(t, "from profile", got.Descr)
	assert.Equal(t, "profile brief", got.InitialMessage)
	assert.True(t, got.AutoFocus)
	assert.True(t, got.IsOwner)
	assert.Equal(t, map[string]string{"human.notify": "grant"}, got.PermissionOverrides)
}

// Explicit flags override the profile — the core precedence the operator wants.
func TestMergeProfileIntoSpawn_FlagsOverrideProfile(t *testing.T) {
	prof := &profileJSON{
		Harness:        "claude",
		Model:          "sonnet",
		Effort:         "medium",
		AgentName:      "profile-name",
		Role:           "profile-role",
		InitialMessage: "profile brief",
	}
	p := &SpawnParams{
		Name:   "flag-name",
		Role:   "flag-role",
		Model:  "opus",
		Effort: "high",
	}
	got := mergeProfileIntoSpawn(p, "flag brief", prof)

	assert.Equal(t, "flag-name", got.Name, "explicit --name wins")
	assert.Equal(t, "flag-role", got.Role, "explicit --role wins")
	assert.Equal(t, "opus", got.Model, "explicit --model wins")
	assert.Equal(t, "high", got.Effort, "explicit --effort wins")
	assert.Equal(t, "flag brief", got.InitialMessage, "explicit brief wins")
	// harness left blank on the flag ⇒ adopts the (matching) profile harness.
	assert.Equal(t, "claude", got.Harness)
}

// A blank --harness adopts the profile's harness and its launch fields.
func TestMergeProfileIntoSpawn_BlankHarnessAdoptsProfile(t *testing.T) {
	prof := &profileJSON{Harness: "codex", Model: "gpt-5-codex", Sandbox: "read-only"}
	got := mergeProfileIntoSpawn(&SpawnParams{}, "", prof)
	assert.Equal(t, "codex", got.Harness)
	assert.Equal(t, "gpt-5-codex", got.Model)
	assert.Equal(t, "read-only", got.Sandbox)
}

// A spawn that pins a DIFFERENT --harness than the profile does NOT inherit the
// profile's launch fields (they belong to the other harness) — but identity
// fields, which are harness-agnostic, still come from the profile.
func TestMergeProfileIntoSpawn_HarnessMismatchSkipsLaunch(t *testing.T) {
	prof := &profileJSON{
		Harness:             "codex",
		Model:               "gpt-5-codex",
		Sandbox:             "read-only",
		Approval:            "never",
		AutoReview:          boolPtr(true),
		TrustDir:            boolPtr(true),
		AgentName:           "reviewer",
		Role:                "qa",
		AutoFocus:           boolPtr(true),
		IsOwner:             boolPtr(true),
		PermissionOverrides: map[string]string{"human.notify": "grant"},
	}
	// Pin claude explicitly — a different harness than the codex profile.
	p := &SpawnParams{Harness: "claude"}
	got := mergeProfileIntoSpawn(p, "", prof)

	// Launch fields belong to the profile's (codex) harness — dropped.
	assert.Equal(t, "claude", got.Harness, "explicit harness kept")
	assert.Empty(t, got.Model, "codex profile's model must NOT leak onto a claude spawn")
	assert.Empty(t, got.Sandbox)
	assert.Empty(t, got.Approval)
	assert.False(t, got.AutoReview, "launch toggle not inherited across harness")
	assert.False(t, got.TrustDir)
	// Identity + harness-agnostic access/toggles are still inherited across the
	// mismatch — is_owner / permission_overrides are authority, not launch shape.
	assert.Equal(t, "reviewer", got.Name)
	assert.Equal(t, "qa", got.Role)
	assert.True(t, got.AutoFocus, "auto_focus is harness-agnostic")
	assert.True(t, got.IsOwner, "is_owner inherited regardless of harness")
	assert.Equal(t, map[string]string{"human.notify": "grant"}, got.PermissionOverrides,
		"permission_overrides inherited regardless of harness")
}

// --no-group-context forces exclude even when the profile says include; a
// profile's explicit include/exclude applies when no flag is given.
func TestMergeProfileIntoSpawn_GroupContext(t *testing.T) {
	// Flag forces exclude over a profile that says include.
	got := mergeProfileIntoSpawn(
		&SpawnParams{NoGroupContext: true},
		"",
		&profileJSON{IncludeGroupDefaultContext: boolPtr(true)},
	)
	if assert.NotNil(t, got.IncludeGroupContext) {
		assert.False(t, *got.IncludeGroupContext, "--no-group-context wins")
	}

	// Profile's exclude applies with no flag.
	got = mergeProfileIntoSpawn(
		&SpawnParams{},
		"",
		&profileJSON{IncludeGroupDefaultContext: boolPtr(false)},
	)
	if assert.NotNil(t, got.IncludeGroupContext) {
		assert.False(t, *got.IncludeGroupContext)
	}

	// Profile's include applies with no flag.
	got = mergeProfileIntoSpawn(
		&SpawnParams{},
		"",
		&profileJSON{IncludeGroupDefaultContext: boolPtr(true)},
	)
	if assert.NotNil(t, got.IncludeGroupContext) {
		assert.True(t, *got.IncludeGroupContext)
	}
}

// An explicit brief wins over the profile's; the profile fills a blank brief.
func TestMergeProfileIntoSpawn_InitialMessage(t *testing.T) {
	prof := &profileJSON{InitialMessage: "profile brief"}
	assert.Equal(t, "flag brief", mergeProfileIntoSpawn(&SpawnParams{}, "flag brief", prof).InitialMessage)
	assert.Equal(t, "profile brief", mergeProfileIntoSpawn(&SpawnParams{}, "", prof).InitialMessage)
}

// remote_control is deliberately not surfaced by the merge — the CLI's
// --remote-control flag path owns it (the group policy must win over a profile).
func TestMergeProfileIntoSpawn_RemoteControlNotInMerge(t *testing.T) {
	prof := &profileJSON{RemoteControl: boolPtr(true)}
	got := mergeProfileIntoSpawn(&SpawnParams{}, "", prof)
	// resolvedSpawnFields carries no remote-control field; the profile's value
	// is intentionally dropped here. Assert the harness-agnostic fields still
	// merged (so this isn't a no-op) but remote control is left to the flag.
	assert.Equal(t, resolvedSpawnFields{}, got, "a remote-control-only profile changes nothing in the merge")
}

func TestPrintProfileHuman(t *testing.T) {
	var buf bytes.Buffer
	printProfileHuman(&buf, profileJSON{
		Name:                "team",
		Descr:               "the team default",
		Harness:             "codex",
		Model:               "gpt-5-codex",
		Effort:              "high",
		AutoReview:          boolPtr(true),
		AgentName:           "reviewer",
		Role:                "qa",
		IsOwner:             boolPtr(true),
		PermissionOverrides: map[string]string{"human.notify": "grant"},
		InitialMessage:      "line one\nline two",
	})
	out := buf.String()
	assert.Contains(t, out, "Profile: team")
	assert.Contains(t, out, "the team default")
	assert.Contains(t, out, "harness=codex")
	assert.Contains(t, out, "model=gpt-5-codex")
	assert.Contains(t, out, "auto_review=on")
	assert.Contains(t, out, "name=reviewer")
	assert.Contains(t, out, "role=qa")
	assert.Contains(t, out, "owner:   yes")
	assert.Contains(t, out, "human.notify=grant")
	assert.Contains(t, out, "line one")
	assert.Contains(t, out, "line two")
}

// An empty/sparse profile renders without panicking and shows only the name.
func TestPrintProfileHuman_Sparse(t *testing.T) {
	var buf bytes.Buffer
	printProfileHuman(&buf, profileJSON{Name: "bare"})
	assert.Equal(t, "Profile: bare\n", buf.String())
}
