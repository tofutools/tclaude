package migration

import (
	"fmt"

	"github.com/tofutools/tclaude/internal/backend/app"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// Assignments follow source IDs, with names used only by older records that
// have no ID. An untranslatable assigned policy must not become an unconfined
// launch after migration, so refuse publication instead of dropping it.
func (t *translator) translateSandboxDefaults(batch *app.ImportBatch) error {
	resolve := t.sandboxImportProfileResolver(batch)
	defaults := model.SandboxDefaults{Groups: map[model.GroupID]model.SandboxProfileID{}, Revision: 1}
	for _, row := range t.inspection.Snapshot.Rows["sandbox_profile_global_assignment"] {
		id, err := resolve(sourcev228.String(row.Values["profile_id"]), sourcev228.String(row.Values["profile_name"]))
		if err != nil {
			return err
		}
		if defaults.Global != "" {
			return fmt.Errorf("multiple global sandbox assignments; offline import refused")
		}
		defaults.Global = id
	}
	groups := map[model.GroupID]bool{}
	for _, group := range batch.Groups {
		groups[group.ID] = true
	}
	for _, row := range t.inspection.Snapshot.Rows["agent_groups"] {
		profile, err := resolve(sourcev228.String(row.Values["sandbox_profile_id"]), sourcev228.String(row.Values["sandbox_profile"]))
		if err != nil {
			return err
		}
		if profile == "" {
			continue
		}
		group := model.GroupID(t.id("agent_groups", sourcev228.String(row.Values["id"])))
		if !groups[group] {
			return fmt.Errorf("sandbox assignment refers to unconverted group %s; offline import refused", group)
		}
		defaults.Groups[group] = profile
	}
	if defaults.Global != "" || len(defaults.Groups) > 0 {
		batch.SandboxDefaults = &defaults
		for i := range batch.SourceRecords {
			record := &batch.SourceRecords[i]
			if record.SourceTable == "sandbox_profile_global_assignment" {
				record.Conversion = string(ConversionReady)
				record.ReasonCode = "sandbox_default_imported_without_execution"
			}
		}
	}
	return nil
}

func (t *translator) sandboxImportProfileResolver(batch *app.ImportBatch) func(string, string) (model.SandboxProfileID, error) {
	available := map[model.SandboxProfileID]bool{}
	for _, result := range batch.SandboxProfiles {
		available[result.Profile.ID] = !result.Profile.Archived
	}
	return func(rawID, name string) (model.SandboxProfileID, error) {
		if rawID == "" || rawID == "0" {
			if name == "" {
				return "", nil
			}
			for _, row := range t.inspection.Snapshot.Rows["sandbox_profiles"] {
				if sourcev228.String(row.Values["name"]) == name {
					if rawID != "" && rawID != "0" {
						return "", fmt.Errorf("ambiguous sandbox assignment %q", name)
					}
					rawID = sourcev228.String(row.Values["id"])
				}
			}
		}
		id := model.SandboxProfileID(t.id("sandbox_profiles", rawID))
		if !available[id] {
			return "", fmt.Errorf("assigned sandbox profile %q (%s) cannot be converted; offline import refused", name, rawID)
		}
		return id, nil
	}
}
