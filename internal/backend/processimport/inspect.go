// Package processimport converts retained process authoring sources into an
// unsaved replacement definition. It has no persistence or execution capability.
package processimport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/processimport/legacy"
)

const MaxSourceBytes = 1 << 20

type Requirement struct {
	Path       string
	Kind       string
	Profile    string
	Assignee   string
	Executable string
	Model      string
	Effort     string
	Decision   bool
}

type Inspection struct {
	SourceHash   string
	Name         string
	OriginalID   string
	Diagnostics  legacy.Diagnostics
	Requirements []Requirement
}

func parse(source string) (*legacy.ParsedTemplate, error) {
	if source == "" || len(source) > MaxSourceBytes || !utf8.ValidString(source) {
		return nil, fmt.Errorf("process source must be nonempty UTF-8, at most 1 MiB")
	}
	return legacy.ParseAuthoring([]byte(source))
}

func Inspect(source string) (Inspection, error) {
	parsed, err := parse(source)
	if err != nil {
		return Inspection{}, err
	}
	digest := sha256.Sum256([]byte(source))
	result := Inspection{SourceHash: hex.EncodeToString(digest[:]), Diagnostics: parsed.Diagnostics}
	if parsed.Template == nil {
		return result, nil
	}
	t := parsed.Template
	result.Name = t.Name
	result.OriginalID = t.ID
	add := func(path string, p *legacy.Performer, decision bool) {
		if p != nil {
			result.Requirements = append(result.Requirements, Requirement{Path: path, Kind: string(p.Kind), Profile: p.Profile, Assignee: p.Assignee, Executable: p.Run, Model: p.Model, Effort: p.Effort, Decision: decision})
		}
	}
	ids := make([]string, 0, len(t.Nodes))
	for id := range t.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := t.Nodes[id]
		path := "/nodes/" + strings.ReplaceAll(strings.ReplaceAll(id, "~", "~0"), "/", "~1")
		add(path+"/performer", node.Performer, node.Type == legacy.NodeTypeDecision)
		if node.Plan != nil {
			add(path+"/plan/performer", &node.Plan.Performer, false)
			if node.Plan.Approval == "human" {
				add(path+"/plan/approval", &legacy.Performer{Kind: legacy.PerformerHuman}, true)
			}
		}
		for i, check := range node.Checks {
			add(fmt.Sprintf("%s/checks/%d/performer", path, i), &check.Performer, false)
		}
		if node.Review != nil {
			add(path+"/review/performer", &node.Review.Performer, false)
		}
	}
	return result, nil
}
