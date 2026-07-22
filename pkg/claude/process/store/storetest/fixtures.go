package storetest

import "github.com/tofutools/tclaude/pkg/claude/process/model"

// Template is the shared authoring fixture used by the filesystem-store tests.
func Template() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "demo",
		Start:      "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"done": "end"}},
			"end":       {Type: model.NodeTypeEnd},
		},
	}
}
