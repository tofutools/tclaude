package migration

import (
	"encoding/json"
	"fmt"

	"github.com/tofutools/tclaude/internal/backend/app"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// The old snapshot supplies selection provenance, not frozen policy content.
// Its remaining fields stay in source evidence. Fresh launches read current
// registry values, as v1 resolveCurrentSandboxChainForConv does.
func (t *translator) translateAgentSandboxChoices(batch *app.ImportBatch) error {
	resolve := t.sandboxImportProfileResolver(batch)
	agents := map[model.AgentID]*model.Agent{}
	for i := range batch.Agents {
		agents[batch.Agents[i].ID] = &batch.Agents[i]
	}
	for _, row := range t.inspection.Snapshot.Rows["agents"] {
		var snapshot struct {
			Version int         `json:"version"`
			Omitted bool        `json:"profiles_omitted"`
			Group   json.Number `json:"resolution_group_id"`
			Applied []struct {
				Scope string      `json:"scope"`
				ID    json.Number `json:"id"`
				Name  string      `json:"name"`
			} `json:"applied"`
		}
		raw := sourcev228.String(row.Values["effective_sandbox_config"])
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
				return fmt.Errorf("agent %s sandbox selection cannot be read: %w", row.Key, err)
			}
		}
		var selected model.SandboxSelection
		if snapshot.Version < 0 || (snapshot.Version == 0 && (snapshot.Omitted || len(snapshot.Applied) > 0 || (snapshot.Group != "" && snapshot.Group != "0"))) {
			return fmt.Errorf("agent %s has invalid sandbox snapshot provenance", row.Key)
		}
		if snapshot.Version > 0 {
			if snapshot.Omitted {
				if len(snapshot.Applied) > 0 || (snapshot.Group != "" && snapshot.Group != "0") {
					return fmt.Errorf("agent %s has conflicting sandbox omission provenance", row.Key)
				}
				selected.OmitProfiles = true
			} else {
				if snapshot.Group != "" && snapshot.Group != "0" {
					selected.GroupID = model.GroupID(t.id("agent_groups", snapshot.Group.String()))
					if selected.GroupID == "" {
						return fmt.Errorf("agent %s sandbox launch group cannot be mapped", row.Key)
					}
				}
				for _, applied := range snapshot.Applied {
					if applied.Scope != "global" && applied.Scope != "group" && applied.Scope != "explicit" {
						return fmt.Errorf("agent %s has unknown sandbox profile scope", row.Key)
					}
					if applied.Scope != "explicit" {
						continue
					}
					if len(selected.Scopes) > 0 {
						return fmt.Errorf("agent %s has multiple explicit sandbox profiles", row.Key)
					}
					sourceID := applied.ID.String()
					// v1 resume recovers a deleted explicit profile by its
					// recorded name when that name has been recreated.
					if sourceID != "" && t.id("sandbox_profiles", sourceID) == "" && applied.Name != "" {
						sourceID = ""
					}
					id, err := resolve(sourceID, applied.Name)
					if err != nil {
						return fmt.Errorf("agent %s: %w", row.Key, err)
					}
					if id == "" {
						return fmt.Errorf("agent %s has an empty explicit sandbox profile", row.Key)
					}
					selected.Scopes = append(selected.Scopes, model.SandboxScopeSelection{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: id}})
				}
			}
		} else {
			var initial struct {
				Profile string `json:"sandbox_profile"`
				Omit    bool   `json:"omit_sandbox_profiles"`
			}
			raw = sourcev228.String(row.Values["initial_spawn_config"])
			if raw != "" {
				if err := json.Unmarshal([]byte(raw), &initial); err != nil {
					return fmt.Errorf("agent %s initial sandbox choice cannot be read: %w", row.Key, err)
				}
			}
			if initial.Omit {
				if initial.Profile != "" {
					return fmt.Errorf("agent %s has conflicting initial sandbox choices", row.Key)
				}
				selected.OmitProfiles = true
			} else if initial.Profile != "" {
				id, err := resolve("", initial.Profile)
				if err != nil {
					return fmt.Errorf("agent %s: %w", row.Key, err)
				}
				selected.Scopes = []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: id}}}
			}
		}
		if selected.OmitProfiles || selected.GroupID != "" || len(selected.Scopes) > 0 {
			if err := selected.Validate(); err != nil {
				return err
			}
			id := model.AgentID(t.id("agents", sourcev228.String(row.Values["agent_id"])))
			if agent := agents[id]; agent != nil {
				agent.Desired.HostSandbox = &selected
			}
		}
	}
	return nil
}
