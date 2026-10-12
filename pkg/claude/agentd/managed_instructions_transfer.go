package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
)

type managedInstructionLaunchContextKey struct{}
type managedInstructionPreview struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	SHA256    string            `json:"sha256"`
	Files     []string          `json:"files,omitempty"`
	CodeFiles []string          `json:"code_files,omitempty"`
	CodeText  map[string]string `json:"code_text,omitempty"`
}

func managedInstructionPreviews(items []db.ManagedInstruction) ([]managedInstructionPreview, string, bool, error) {
	previews := []managedInstructionPreview{}
	needsApproval := false
	total := 0
	for _, item := range items {
		if err := managedinstructions.Validate(item.Kind, item.Name, item.Content); err != nil {
			return nil, "", false, err
		}
		total += len(item.Content)
		if total > db.ManagedInstructionAgentLimit {
			return nil, "", false, fmt.Errorf("managed instructions exceed 16 MiB")
		}
		sum := sha256.Sum256(item.Content)
		preview := managedInstructionPreview{Kind: item.Kind, Name: item.Name, SHA256: hex.EncodeToString(sum[:])}
		if item.Kind == "skill" {
			bundle, err := skillbundle.Decode(item.Content)
			if err != nil {
				return nil, "", false, err
			}
			for _, file := range bundle.Files {
				preview.Files = append(preview.Files, file.Path)
				if managedinstructions.RequiresCodeApproval(file) {
					preview.CodeFiles = append(preview.CodeFiles, file.Path)
					if preview.CodeText == nil {
						preview.CodeText = map[string]string{}
					}
					preview.CodeText[file.Path] = string(file.Data)
					needsApproval = true
				}
			}
		}
		previews = append(previews, preview)
	}
	raw, _ := json.Marshal(previews)
	digest := sha256.Sum256(raw)
	return previews, hex.EncodeToString(digest[:]), needsApproval, nil
}
func managedInstructionsForExport(conv string) ([]db.ManagedInstruction, error) {
	id, err := db.AgentIDForConv(conv)
	if err != nil {
		return nil, err
	}
	launch, err := db.GetManagedInstructionLaunch(id)
	if err != nil || launch == nil || launch.Resources == "" {
		return nil, err
	}
	var resources []db.ManagedInstruction
	if err = json.Unmarshal([]byte(launch.Resources), &resources); err != nil {
		return nil, err
	}
	if _, _, _, err = managedInstructionPreviews(resources); err != nil {
		return nil, err
	}
	return resources, nil
}
func omitManagedInstructions(w http.ResponseWriter, r *http.Request, items *[]db.ManagedInstruction, omit bool) bool {
	if !omit || len(*items) == 0 {
		return true
	}
	if !requireHuman(w, r, "managed instruction script approval or omission requires the receiving human") {
		return false
	}
	*items = nil
	return true
}

func managedCloneResources(source string) ([]db.ManagedInstruction, error) {
	carried, err := managedInstructionsForExport(source)
	if err != nil {
		return nil, err
	}
	groups, err := db.ListGroupsForConv(source)
	if err != nil {
		return nil, err
	}
	receiver, err := managedinstructions.Resolve(groups)
	if err != nil {
		return nil, err
	}
	for i := range carried {
		carried[i].Scope = "carried"
		carried[i].GroupID = 0
	}
	return managedinstructions.Merge(receiver, carried)
}
