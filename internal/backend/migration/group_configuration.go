package migration

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (t *translator) translateGroupConfiguration(batch *app.ImportBatch, row sourcev228.Row, group model.Group) {
	sourceID := sourcev228.String(row.Values["default_profile_id"])
	if sourceID == "" {
		return
	}
	// This is a source foreign key, never a profile name or alias. Names may
	// equal another profile's source ID without changing the selected identity.
	id := model.ConfigurationProfileID(t.id("spawn_profiles", sourceID))
	for _, profile := range batch.ConfigurationProfiles {
		if id != "" && profile.Profile.ID == id {
			ref := profile.Revision.Ref
			batch.GroupConfigurations = append(batch.GroupConfigurations, model.GroupConfiguration{GroupID: group.ID, Profile: &ref, Revision: 1, UpdatedAt: group.UpdatedAt})
			return
		}
	}
	t.launchMetadataDiagnostic(batch, "agent_groups", row.Key, "group_default_retained_unmapped", "group default profile identity cannot be translated; exact selection remains in retained source evidence")
}
