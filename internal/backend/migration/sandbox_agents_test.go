package migration

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestImportedAgentSandboxChoicePreservesExplicitOmissionAndGroup(t *testing.T) {
	for _, test := range []struct {
		name, snapshot, initial  string
		omitted, explicit, group bool
	}{
		{name: "snapshot explicit follows ID after rename", snapshot: `{"version":7,"resolution_group_id":1,"applied":[{"scope":"global","id":1,"name":"Parent"},{"scope":"explicit","id":2,"name":"old child name"}],"effective":{"environment":[{"name":"OLD","value":"do not copy"}]}}`, explicit: true, group: true},
		{name: "snapshot omission overrides original choice", snapshot: `{"version":7,"profiles_omitted":true,"applied":[]}`, initial: `{"sandbox_profile":"Child"}`, omitted: true},
		{name: "recreated explicit profile name", snapshot: `{"version":7,"applied":[{"scope":"explicit","id":999,"name":"Child"}]}`, explicit: true},
		{name: "first supported version", snapshot: `{"version":1,"applied":[{"scope":"explicit","id":2,"name":"Child"}]}`, explicit: true},
		{name: "current supported version", snapshot: `{"version":13,"profiles_omitted":true}`, omitted: true},
		{name: "initial explicit name", initial: `{"sandbox_profile":"Child"}`, explicit: true},
		{name: "initial explicit omission", initial: `{"omit_sandbox_profiles":true}`, omitted: true},
		{name: "group only recorded before assignment", snapshot: `{"version":7,"resolution_group_id":1,"applied":[]}`, group: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := assignedSandboxFixture(t)
			alterFixture(t, bundle, fmt.Sprintf(`ALTER TABLE agents ADD COLUMN effective_sandbox_config TEXT; UPDATE agents SET effective_sandbox_config='%s',initial_spawn_config='%s' WHERE agent_id='agt_fixture'`, test.snapshot, test.initial))
			destination := filepath.Join(t.TempDir(), "backend.sqlite")
			_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			store, err := backendsqlite.Open(destination)
			require.NoError(t, err)
			agent, err := store.Agent(context.Background(), "agt_fixture")
			require.NoError(t, err)
			choice := agent.Desired.HostSandbox
			require.NotNil(t, choice)
			require.Equal(t, test.omitted, choice.OmitProfiles)
			require.Equal(t, test.group, choice.GroupID != "")
			require.Empty(t, choice.PolicyHash)
			defaults, err := store.SandboxDefaults(context.Background())
			require.NoError(t, err)
			if test.explicit {
				require.Len(t, choice.Scopes, 1)
				require.Equal(t, model.SandboxScopeExplicit, choice.Scopes[0].Scope)
				profile, err := store.SandboxProfile(context.Background(), choice.Scopes[0].Ref.ProfileID)
				require.NoError(t, err)
				require.Equal(t, "Child", profile.Profile.Name)
				require.Empty(t, choice.Scopes[0].Ref.RevisionID)
			}
			if test.omitted {
				require.Nil(t, defaults.Resolve(choice))
			}
			require.NoError(t, store.Close())
			repeated, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			require.True(t, repeated.Repeated)
		})
	}
}

func TestImportedAgentSandboxChoiceRefusesUnmappedExplicitProfile(t *testing.T) {
	bundle := assignedSandboxFixture(t)
	alterFixture(t, bundle, `ALTER TABLE agents ADD COLUMN effective_sandbox_config TEXT; UPDATE agents SET effective_sandbox_config='{"version":7,"applied":[{"scope":"explicit","id":999,"name":"missing"}]}'`)
	destination := filepath.Join(t.TempDir(), "refused.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.ErrorContains(t, err, "assigned sandbox profile")
	require.NoFileExists(t, destination)
}

func TestImportedAgentSandboxChoiceRefusesUnsupportedSnapshotBeforePublication(t *testing.T) {
	for _, snapshot := range []string{`{}`, `null`, `{"version":0}`, `{"version":-1}`, `{"version":14,"profiles_omitted":true,"applied":[]}`} {
		t.Run(snapshot, func(t *testing.T) {
			bundle := assignedSandboxFixture(t)
			alterFixture(t, bundle, fmt.Sprintf(`ALTER TABLE agents ADD COLUMN effective_sandbox_config TEXT; UPDATE agents SET effective_sandbox_config='%s',initial_spawn_config='{"sandbox_profile":"Child"}' WHERE agent_id='agt_fixture'`, snapshot))
			destination := filepath.Join(t.TempDir(), "refused.sqlite")
			_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
			require.ErrorContains(t, err, "unsupported sandbox snapshot version")
			require.NoFileExists(t, destination)
		})
	}
}
