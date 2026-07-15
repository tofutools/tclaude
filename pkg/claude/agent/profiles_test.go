package agent

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool { return &b }

func TestRunProfilesDefault_ShowSetClear(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		switch method {
		case "GET":
			return 200, "", `{"name":"gpt5.6-sol-high"}`
		case "PUT":
			return 200, "", `{"name":"gpt5.6-sol-high"}`
		case "DELETE":
			return 200, "", ""
		default:
			return 405, "method", ""
		}
	})

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runProfilesDefaultShow(&stdout, &stderr), "stderr=%s", stderr.String())
	assert.Equal(t, "gpt5.6-sol-high\n", stdout.String())

	stdout.Reset()
	require.Equal(t, rcOK, runProfilesDefaultSet(
		&profilesDefaultSetParams{Name: " gpt5.6-sol-high "}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "Global default profile set to gpt5.6-sol-high")

	stdout.Reset()
	require.Equal(t, rcOK, runProfilesDefaultClear(&stdout, &stderr))
	assert.Contains(t, stdout.String(), "Global default profile cleared")

	require.Len(t, calls, 3)
	for _, call := range calls {
		assert.Equal(t, "/v1/spawn-profile-default", call.path)
	}
	assert.Equal(t, "GET", calls[0].method)
	assert.Equal(t, "PUT", calls[1].method)
	assert.Equal(t, map[string]string{"name": "gpt5.6-sol-high"}, calls[1].body)
	assert.Equal(t, "DELETE", calls[2].method)
}

func TestRunProfilesDefaultShow_Unset(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":""}`))
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runProfilesDefaultShow(&stdout, &stderr))
	assert.Contains(t, stdout.String(), "no global default spawn profile")
}

func TestRunProfilesDisableAndEnable(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		switch method {
		case http.MethodGet:
			return 200, "", `{"name":"paused","model":"sonnet"}`
		case http.MethodPatch:
			return 200, "", `{"id":1,"name":"paused"}`
		default:
			return 405, "method", ""
		}
	})

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runProfilesDisable(&profilesDisableParams{
		Name: "paused", Reason: "provider maintenance",
	}, &stdout, &stderr), "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), `Disabled profile "paused": provider maintenance`)
	require.Len(t, calls, 2)
	disabled, ok := calls[1].body.(*profileJSON)
	require.True(t, ok)
	assert.Equal(t, "provider maintenance", disabled.DisabledReason)
	assert.Equal(t, "sonnet", disabled.Model, "disable preserves the complete profile")

	calls = nil
	stdout.Reset()
	require.Equal(t, rcOK, runProfilesEnable(&profilesEnableParams{Name: "paused"}, &stdout, &stderr))
	require.Len(t, calls, 2)
	enabled, ok := calls[1].body.(*profileJSON)
	require.True(t, ok)
	assert.Empty(t, enabled.DisabledReason)
	assert.Contains(t, stdout.String(), `Enabled profile "paused"`)
}

func TestRunProfilesDisableRequiresReason(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runProfilesDisable(&profilesDisableParams{Name: "paused"}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "--reason is required")
}

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
		DisabledReason:      "provider quota exhausted",
		Aliases:             []string{"codex-reviewer", "cold-reviewer"},
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
	assert.Contains(t, out, "status:  disabled")
	assert.Contains(t, out, "reason:  provider quota exhausted")
	assert.Contains(t, out, "aliases: codex-reviewer, cold-reviewer")
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

// loadProfileFile round-trips the `profiles show --json` shape that `create` /
// `edit` accept — the file-based input the mutating verbs share.
func TestLoadProfileFile_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	body := `{"name":"team","aliases":["codex-reviewer"],"harness":"codex","model":"gpt-5-codex","effort":"high",` +
		`"is_owner":true,"permission_overrides":{"human.notify":"grant"}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	prof, rc := loadProfileFile(path, nil, new(bytes.Buffer))
	require.Equal(t, rcOK, rc)
	require.NotNil(t, prof)
	assert.Equal(t, []string{"codex-reviewer"}, prof.Aliases)
	assert.Equal(t, "team", prof.Name)
	assert.Equal(t, "codex", prof.Harness)
	assert.Equal(t, "gpt-5-codex", prof.Model)
	assert.Equal(t, "high", prof.Effort)
	if assert.NotNil(t, prof.IsOwner) {
		assert.True(t, *prof.IsOwner)
	}
	assert.Equal(t, map[string]string{"human.notify": "grant"}, prof.PermissionOverrides)
}

// "-" reads the profile JSON from stdin (sidesteps shell quoting for long bodies).
func TestLoadProfileFile_Stdin(t *testing.T) {
	prof, rc := loadProfileFile("-", strings.NewReader(`{"name":"from-stdin"}`), new(bytes.Buffer))
	require.Equal(t, rcOK, rc)
	require.NotNil(t, prof)
	assert.Equal(t, "from-stdin", prof.Name)
}

// A missing --file, an unreadable path, and malformed / wrong-shape JSON all
// fail fast before any daemon call.
func TestLoadProfileFile_Errors(t *testing.T) {
	_, rc := loadProfileFile("", nil, new(bytes.Buffer))
	assert.Equal(t, rcInvalidArg, rc, "missing --file")

	// A path that does not exist surfaces as an IO failure, naming the file.
	missing := filepath.Join(t.TempDir(), "nope.json")
	stderr := new(bytes.Buffer)
	_, rc = loadProfileFile(missing, nil, stderr)
	assert.Equal(t, rcIOFailure, rc, "unreadable file")
	assert.Contains(t, stderr.String(), missing)

	// Malformed JSON.
	badPath := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(badPath, []byte("{not json"), 0o600))
	stderr = new(bytes.Buffer)
	_, rc = loadProfileFile(badPath, nil, stderr)
	assert.Equal(t, rcInvalidArg, rc, "malformed JSON")
	assert.Contains(t, stderr.String(), "not valid profile JSON")

	// Valid JSON of the wrong shape — e.g. the ARRAY that `profiles ls --json`
	// would emit — is not a single profile object and is rejected, not silently
	// coerced into an empty profile.
	arrPath := filepath.Join(t.TempDir(), "arr.json")
	require.NoError(t, os.WriteFile(arrPath, []byte(`[{"name":"a"}]`), 0o600))
	stderr = new(bytes.Buffer)
	_, rc = loadProfileFile(arrPath, nil, stderr)
	assert.Equal(t, rcInvalidArg, rc, "array instead of object")
	assert.Contains(t, stderr.String(), "not valid profile JSON")
}

// IsOwner is tri-state: an explicit false renders "owner: no" (distinct from
// unset, which renders no owner line at all), matching --json and boolFlags.
func TestPrintProfileHuman_OwnerTristate(t *testing.T) {
	var no bytes.Buffer
	printProfileHuman(&no, profileJSON{Name: "p", IsOwner: boolPtr(false)})
	assert.Contains(t, no.String(), "owner:   no", "explicit false must render, not vanish")

	var unset bytes.Buffer
	printProfileHuman(&unset, profileJSON{Name: "p"})
	assert.NotContains(t, unset.String(), "owner:", "unset owner renders no line")
}
