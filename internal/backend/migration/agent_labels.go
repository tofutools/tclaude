package migration

import (
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (t *translator) agentDisplayLabels(agent sourcev228.Row) model.AgentLabels {
	labels := model.AgentLabels{}
	key := sourcev228.String(agent.Values["agent_id"])
	current := sourcev228.String(agent.Values["current_conv_id"])
	for _, row := range t.inspection.Snapshot.Rows["agent_conversations"] {
		if sourcev228.String(row.Values["agent_id"]) == key && sourcev228.String(row.Values["conv_id"]) == current {
			labels.Role = sourcev228.String(row.Values["role"])
		}
	}
	var common *model.AgentDisplayLabels
	same := true
	for _, row := range t.inspection.Snapshot.Rows["agent_group_members"] {
		if sourcev228.String(row.Values["agent_id"]) != key {
			continue
		}
		value := model.AgentDisplayLabels{Role: sourcev228.String(row.Values["role"]), Description: sourcev228.String(row.Values["descr"])}
		if labels.Groups == nil {
			labels.Groups = map[model.GroupID]model.AgentDisplayLabels{}
		}
		labels.Groups[model.GroupID(t.id("agent_groups", sourcev228.String(row.Values["group_id"])))] = value
		if common == nil {
			copy := value
			common = &copy
		} else if *common != value {
			same = false
		}
	}
	// A shared label is also useful outside a group; distinct memberships remain distinct.
	if common != nil && same {
		labels.Role, labels.Description = common.Role, common.Description
		labels.Groups = nil
	}
	return labels
}
