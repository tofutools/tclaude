package agentd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/drawingboard"
)

// A drawing's reference rides on every board view, so a big pick keeps only
// its count and digest, and still matches a re-export of the same part.
func TestTaskboardDrawingRefCapsTheIDsItLists(t *testing.T) {
	var many []string
	for i := range taskboardDrawingRefMaxElements + 1 {
		many = append(many, fmt.Sprintf("el-%d", i))
	}
	crop := drawingboard.Box{Width: 10, Height: 10}
	big := newTaskboardDrawingRef("arch", 3, many, nil, crop)
	assert.Empty(t, big.Elements, "past the cap the ids are not listed")
	assert.Equal(t, len(many), big.ElementCount)

	reversed := make([]string, len(many))
	for i, id := range many {
		reversed[len(many)-1-i] = id
	}
	assert.True(t, big.sameSelection(newTaskboardDrawingRef("arch", 4, reversed, nil, crop)), "the same pick, in any order, is the same part")
	assert.False(t, big.sameSelection(newTaskboardDrawingRef("arch", 4, many[1:], nil, crop)))

	small := newTaskboardDrawingRef("arch", 3, []string{"b", "a"}, nil, crop)
	assert.Equal(t, []string{"b", "a"}, small.Elements)
	assert.True(t, small.sameSelection(newTaskboardDrawingRef("arch", 5, []string{"a", "b"}, nil, crop)))
	assert.False(t, small.sameSelection(newTaskboardDrawingRef("arch", 5, nil, nil, crop)), "a part is not the whole board")
	region := &drawingboard.Box{X: 1, Width: 5, Height: 5}
	assert.False(t, newTaskboardDrawingRef("arch", 1, nil, region, crop).sameSelection(newTaskboardDrawingRef("arch", 1, nil, nil, crop)))
}
