package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func prepareManagedInstructions(g *db.AgentGroup, p *spawnParams) (func(bool), *spawnFailure) {
	noop := func(bool) {}
	if p.ManagedInstructionsRoot != "" || p.Harness == harness.ShellName {
		return noop, nil
	}
	groups := []*db.AgentGroup{}
	seen := map[int64]bool{}
	if g != nil {
		groups = append(groups, g)
		seen[g.ID] = true
	}
	if p.AgentID != "" {
		memberships, err := db.ListAgentGroupMembershipsByJoin(p.AgentID)
		if err != nil {
			return noop, &spawnFailure{Status: 500, Kind: "managed_instructions", Msg: err.Error()}
		}
		for _, m := range memberships {
			if !seen[m.Group.ID] {
				groups = append(groups, m.Group)
				seen[m.Group.ID] = true
			}
		}
	}
	resources, err := managedinstructions.Resolve(groups)
	if err != nil {
		return noop, &spawnFailure{Status: 409, Kind: "managed_instructions", Msg: err.Error()}
	}
	previous, err := db.GetManagedInstructionLaunch(p.AgentID)
	if err != nil {
		return noop, &spawnFailure{Status: 500, Kind: "managed_instructions", Msg: err.Error()}
	}
	carried := append([]db.ManagedInstruction(nil), p.ManagedInstructionResources...)
	if previous != nil && previous.Resources != "" {
		var priorItems []db.ManagedInstruction
		if err = json.Unmarshal([]byte(previous.Resources), &priorItems); err != nil {
			return noop, &spawnFailure{Status: 500, Kind: "managed_instructions", Msg: err.Error()}
		}
		for _, item := range priorItems {
			if item.Scope == "carried" || item.Carried {
				carried = append(carried, item)
			}
		}
	}
	for i := range carried {
		carried[i].Scope = "carried"
		carried[i].GroupID = 0
		carried[i].Carried = true
	}
	resources, err = managedinstructions.Merge(resources, carried)
	if err != nil {
		return noop, &spawnFailure{Status: 409, Kind: "managed_instructions", Msg: err.Error()}
	}
	if len(resources) == 0 {
		if previous == nil {
			return noop, nil
		}
		if err = db.SaveManagedInstructionLaunch(db.ManagedInstructionLaunch{AgentID: p.AgentID, Resources: "[]"}); err != nil {
			return noop, &spawnFailure{Status: 500, Kind: "managed_instructions", Msg: err.Error()}
		}
		return func(success bool) {
			if success {
				removeSupersededManagedRoot(previous.Root, p.ManagedPreviousConv)
			} else {
				_ = db.SaveManagedInstructionLaunch(*previous)
			}
		}, nil
	}
	launch, err := managedinstructions.Materialize(resources)
	if err != nil {
		return noop, &spawnFailure{Status: 500, Kind: "managed_instructions", Msg: err.Error()}
	}
	if p.AgentID == "" {
		p.AgentID = db.NewAgentID()
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		_ = os.RemoveAll(launch.Root)
		return noop, &spawnFailure{Status: 500, Kind: "managed_instructions", Msg: err.Error()}
	}
	row := db.ManagedInstructionLaunch{AgentID: p.AgentID, Root: launch.Root, Body: launch.Body, Resources: string(raw)}
	if err = db.SaveManagedInstructionLaunch(row); err != nil {
		_ = os.RemoveAll(launch.Root)
		return noop, &spawnFailure{Status: 500, Kind: "managed_instructions", Msg: err.Error()}
	}
	p.ManagedInstructionsRoot = launch.Root
	p.ManagedInstructionsBody = launch.Body
	return func(success bool) {
		if success {
			if previous != nil {
				removeSupersededManagedRoot(previous.Root, p.ManagedPreviousConv)
			}
			return
		}
		_ = os.RemoveAll(launch.Root)
		if previous != nil {
			_ = db.SaveManagedInstructionLaunch(*previous)
		} else {
			_ = db.DeleteManagedInstructionLaunch(p.AgentID)
		}
	}, nil
}

func managedInstructionResumePrompt(p spawnParams) string {
	if p.ManagedInstructionsBody == "" {
		return ""
	}
	if len(p.ManagedInstructionsBody) <= spawnInlineMaxChars() {
		return p.ManagedInstructionsBody
	}
	return "Standing guidance for this launch: read " + filepath.Join(p.ManagedInstructionsRoot, "INSTRUCTIONS.md") + " before continuing. These operator rules apply throughout the launch; the file also lists on-demand skills."
}

func removeSupersededManagedRoot(root, previousConv string) {
	if previousConv == "" {
		_ = managedinstructions.RemoveLaunchRoot(root)
		return
	}
	goBackground(func() {
		if waitForConvOffline(previousConv, retireWorktreeExitGrace) {
			_ = managedinstructions.RemoveLaunchRoot(root)
		}
	})
}
