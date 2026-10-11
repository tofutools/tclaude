package transcript

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestPriorMorphNoticeKeepsAdjacentInstructionsAndPointers(t *testing.T) {
	input := "Latest operator instruction\n⟦tclaude morph⟧ History carried over from claude to codex. Tool records are history; use your own tools.\n⟦tool result · shell · completed⟧\nRetained tool text\n⟦Summary of the earlier conversation, written when claude compacted it⟧\nRemembered decision\n⟦attachment image saved to /history/image.png⟧\nQuoted marker: ⟦tclaude morph⟧ History carried over from a to b."
	want := "Latest operator instruction\n⟦tool result · shell · completed⟧\nRetained tool text\n⟦Summary of the earlier conversation, written when claude compacted it⟧\nRemembered decision\n⟦attachment image saved to /history/image.png⟧\nQuoted marker: ⟦tclaude morph⟧ History carried over from a to b."
	assert.Equal(t, want, withoutPriorMorphNotices(input))
	assert.Equal(t, want, withoutPriorMorphNotices(want), "normalization is idempotent")
}

func TestPriorMorphNoticePreservesFencedLiteral(t *testing.T) {
	for _, fence := range []string{"```", "~~~"} {
		input := "Please preserve this exact fixture:\n" + fence + "text\n⟦tclaude morph⟧ History carried over from claude to codex. Tool records are history; use your own tools.\n" + fence + "\nUse that literal line in the regression test."
		assert.Equal(t, input, withoutPriorMorphNotices(input))
	}
}
