// Package builtin is tclaude's built-in ways of working (tk_d02b0f4c):
// process templates compiled into the binary, offered in a group's New
// process Start from strip and in the template library. A group that uses
// one gets its own copy, a stored template pinned to the group, so its edits
// stay with the group and the built-in never changes under it.
package builtin

import (
	"embed"
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

//go:embed templates/*.yaml
var files embed.FS

// order is how the built-ins are offered: the operator's full way of working
// first, then the lighter ones.
var order = []string{"plan-vet-build", "build-review-ship", "bug-fix", "quick-change", "plan-fan-out", "ask-me-first", "check-fix"}

// Builtin is one built-in way of working.
type Builtin struct {
	ID          string
	Name        string
	Description string
	// Source is its YAML, as a group's copy starts from.
	Source []byte
	// Parsed is its parsed template, edges and hashes.
	Parsed *model.ParsedTemplate
}

// List is every built-in, in the order they are offered.
func List() ([]Builtin, error) {
	out := make([]Builtin, 0, len(order))
	for _, id := range order {
		b, err := Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, nil
}

// IDs are the built-ins' ids, in order.
func IDs() []string { return slices.Clone(order) }

// ErrNotFound is Get's error for an id that is not a built-in.
var ErrNotFound = fmt.Errorf("no such built-in way of working")

// Get is the built-in with this id.
func Get(id string) (*Builtin, error) {
	if !slices.Contains(order, id) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	source, err := files.ReadFile("templates/" + id + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("built-in %s: %w", id, err)
	}
	parsed, err := model.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("built-in %s: %w", id, err)
	}
	if parsed.Template == nil || parsed.Diagnostics.HasErrors() {
		return nil, fmt.Errorf("built-in %s does not validate: %v", id, parsed.Diagnostics)
	}
	return &Builtin{ID: id, Name: parsed.Template.Name, Description: parsed.Template.Description, Source: source, Parsed: parsed}, nil
}
