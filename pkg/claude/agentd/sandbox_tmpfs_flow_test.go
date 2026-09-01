package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type wireTmpfsProfile struct {
	Name  string `json:"name"`
	Tmpfs []struct {
		Path      string `json:"path"`
		Size      string `json:"size"`
		SizeBytes uint64 `json:"size_bytes"`
	} `json:"tmpfs"`
}

// A profile field that exists in the policy package but not in the wire type,
// the DB row, or any of the conversions between them is silently discarded on
// the way in and absent on the way out. This walks the real API round trip so
// that gap cannot open: POST a profile with tmpfs rows, read it back, and
// confirm the stored row carries them with the derived byte count intact.
func TestSandboxProfileTmpfsSurvivesTheAPIRoundTrip(t *testing.T) {
	f := newFlow(t)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "with-scratch",
		"tmpfs": []map[string]any{
			{"path": "/scratch", "size": "512MiB"},
			{"path": "/build"},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/with-scratch", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got wireTmpfsProfile
	testharness.DecodeJSON(t, rec, &got)

	require.Len(t, got.Tmpfs, 2, "the API must return the mounts it accepted")
	// Normalization sorts by path, so /build precedes /scratch regardless of
	// the order they were authored in.
	assert.Equal(t, "/build", got.Tmpfs[0].Path)
	assert.Empty(t, got.Tmpfs[0].Size, "an omitted size stays omitted, not zero-filled")
	assert.Equal(t, "/scratch", got.Tmpfs[1].Path)
	assert.Equal(t, "512MiB", got.Tmpfs[1].Size, "the operator's spelling must survive verbatim")
	assert.Equal(t, uint64(512<<20), got.Tmpfs[1].SizeBytes)

	// …and the same through the storage layer, which is where the mounts have
	// to be for a launch to ever see them.
	stored, err := db.GetSandboxProfile("with-scratch")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Len(t, stored.Tmpfs, 2)
	assert.Equal(t, uint64(512<<20), stored.Tmpfs[1].SizeBytes)
}

// Validation must be enforced at the daemon boundary, not merely in the policy
// package: the daemon is the authority for profile documents.
func TestSandboxProfileTmpfsValidationIsEnforcedAtTheAPI(t *testing.T) {
	f := newFlow(t)
	for name, mount := range map[string]map[string]any{
		"relative path": {"path": "scratch"},
		"sandbox root":  {"path": "/"},
		"missing path":  {"size": "1GiB"},
		"bad size":      {"path": "/scratch", "size": "plenty"},
		"derived only":  {"path": "/scratch", "size_bytes": 4096},
	} {
		t.Run(name, func(t *testing.T) {
			rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
				"name":  "invalid-" + name,
				"tmpfs": []map[string]any{mount},
			})
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// Export/import is how a profile moves between machines, so the mounts have to
// ride along — and the bundle has to declare the version that carries them, or
// an older reader would import a profile whose scratch space silently vanished.
func TestSandboxProfileTmpfsSurvivesExportImport(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":  "exported-scratch",
		"tmpfs": []map[string]any{{"path": "/scratch", "size": "64MiB"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/export", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var envelope struct {
		Format        string             `json:"format"`
		FormatVersion int                `json:"format_version"`
		Profiles      []wireTmpfsProfile `json:"profiles"`
	}
	testharness.DecodeJSON(t, rec, &envelope)
	assert.GreaterOrEqual(t, envelope.FormatVersion, 17,
		"a bundle carrying temporary filesystems must declare the version that has them")
	var exported *wireTmpfsProfile
	for i := range envelope.Profiles {
		if envelope.Profiles[i].Name == "exported-scratch" {
			exported = &envelope.Profiles[i]
		}
	}
	require.NotNil(t, exported, "the exported bundle must contain the profile")
	require.Len(t, exported.Tmpfs, 1, "export must carry temporary filesystems")
	assert.Equal(t, "64MiB", exported.Tmpfs[0].Size)

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format":         envelope.Format,
		"format_version": envelope.FormatVersion,
		"profiles": []map[string]any{{
			"name":  "imported-scratch",
			"tmpfs": []map[string]any{{"path": "/scratch", "size": "64MiB"}},
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	stored, err := db.GetSandboxProfile("imported-scratch")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Len(t, stored.Tmpfs, 1)
	assert.Equal(t, uint64(64<<20), stored.Tmpfs[0].SizeBytes)
}

// An older envelope cannot smuggle the newer field in: accepting it would mean
// two installations disagree about what "version 16" contains.
func TestSandboxProfileTmpfsRefusesAnOlderExportVersion(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format":         "tclaude-sandbox-profiles",
		"format_version": 16,
		"profiles": []map[string]any{{
			"name":  "too-old",
			"tmpfs": []map[string]any{{"path": "/scratch"}},
		}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "export format version 17")
}

// A client that edits a profile without resending tmpfs must not silently
// unmount the agent's scratch space. The dashboard builds its save body from an
// explicit field whitelist, so a field its editor does not know about is simply
// absent on the wire — the same failure the pre_launch guard exists to prevent.
func TestSandboxProfileUpdateWithoutTmpfsKeepsTheMounts(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":  "keeps-scratch",
		"tmpfs": []map[string]any{{"path": "/scratch", "size": "32MiB"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// A client that knows about every other field and nothing about this one.
	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/keeps-scratch", map[string]any{
		"name":              "keeps-scratch",
		"filesystem":        []map[string]any{},
		"environment":       []map[string]any{},
		"includes":          []string{},
		"agent_directories": []string{},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	stored, err := db.GetSandboxProfile("keeps-scratch")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Len(t, stored.Tmpfs, 1,
		"an update that never mentioned tmpfs must not delete it")
	assert.Equal(t, "/scratch", stored.Tmpfs[0].Path)
}

// …but a client that means it must still be able to clear them, so absence and
// an explicit empty list cannot be the same thing.
func TestSandboxProfileUpdateWithEmptyTmpfsClearsTheMounts(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":  "clears-scratch",
		"tmpfs": []map[string]any{{"path": "/scratch"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/clears-scratch", map[string]any{
		"name":  "clears-scratch",
		"tmpfs": []map[string]any{},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	stored, err := db.GetSandboxProfile("clears-scratch")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.Tmpfs, "an explicit empty list must clear the mounts")
}

// The dashboard never commits its own body: it previews, shows the operator a
// diff, then saves the preview's rendering. So a dry run that dropped the
// mounts would show a diff that silently deletes them.
func TestSandboxProfilePreviewWithoutTmpfsStillCarriesTheMounts(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":  "previewed-scratch",
		"tmpfs": []map[string]any{{"path": "/scratch", "size": "16MiB"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/previewed-scratch?dry_run=1",
		map[string]any{
			"name":        "previewed-scratch",
			"filesystem":  []map[string]any{},
			"environment": []map[string]any{},
		})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var preview struct {
		After wireTmpfsProfile `json:"after"`
	}
	testharness.DecodeJSON(t, rec, &preview)
	require.Len(t, preview.After.Tmpfs, 1,
		"the preview the operator confirms must still carry the mounts")
	assert.Equal(t, "16MiB", preview.After.Tmpfs[0].Size)
}

// The last link in the chain: a stored profile must resolve into the effective
// snapshot the launch path reads, and that snapshot must render a plan entry.
// Without both, the mounts would sit in the registry and never reach a pane.
func TestStoredTmpfsReachesTheEffectiveProfileAndMountPlan(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":  "resolvable-scratch",
		"tmpfs": []map[string]any{{"path": "/scratch", "size": "128MiB"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, "resolvable-scratch")
	require.NoError(t, err)
	require.Len(t, snapshot.Effective.Tmpfs, 1,
		"a stored mount must survive resolution into the launch snapshot")

	plan, err := sandboxpolicy.RenderMountPlan(snapshot.Effective)
	require.NoError(t, err)
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{
		Path: "/scratch", Mode: sandboxpolicy.MountTmpfs, SizeBytes: 128 << 20,
	})
}

// The enforcement preview must be capability-honest: the cell may claim
// enforcement only where a mount namespace tclaude wholly owns exists, and
// every other surface is bucketed Refused with the mount named — including
// `stacked`, whose inner harness-native wall would block writes to a mount the
// outer layer created.
func TestSandboxProfileTmpfsEnforcementPreviewIsCapabilityHonest(t *testing.T) {
	f := newFlow(t)
	draft := map[string]any{
		"name":  "scratch-preview",
		"tmpfs": []any{map[string]any{"path": "/scratch", "size": "256MiB"}},
	}

	for _, tc := range []struct {
		name           string
		implementation string
		platform       string
		outcome        string
	}{
		{"linux tclaude-layer", "tclaude-layer", "linux", harness.AccessPredictionEnforced},
		{"linux stacked", "stacked", "linux", harness.AccessPredictionRefused},
		{"darwin tclaude-layer", "tclaude-layer", "darwin", harness.AccessPredictionRefused},
		{"linux harness-builtin", "harness-builtin", "linux", harness.AccessPredictionRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
				"draft": draft,
				"targets": []any{map[string]any{
					"implementation": tc.implementation,
					"harness":        harness.DefaultName,
					"platform":       tc.platform,
				}},
			})
			require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			var got struct {
				Targets []struct {
					Axes harness.PredictedAccessAxes `json:"axes"`
				} `json:"targets"`
			}
			testharness.DecodeJSON(t, rec, &got)
			require.Len(t, got.Targets, 1)
			assert.Equal(t, tc.outcome, got.Targets[0].Axes.Filesystem.Outcome)
			assert.Contains(t, got.Targets[0].Axes.Filesystem.Tier, "1 temporary filesystem",
				"a tmpfs-only profile still has a filesystem opinion and must say so")
			if tc.outcome == harness.AccessPredictionRefused {
				assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, "/scratch",
					"the refusal must name the mount it cannot apply")
				assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, "mount namespace",
					"the refusal must name the missing capability")
			}
		})
	}
}
