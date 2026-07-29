package standingorders

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeMatcherInputs(t *testing.T) {
	assert.Equal(t, "Bash", NormalizeToolName("shell"))
	assert.Equal(t, "Bash", NormalizeToolName(" bash "))
	assert.Equal(t, "Edit", NormalizeToolName("Edit"))

	assert.Equal(t, `{"command":"git push","timeout":30}`,
		NormalizeToolInput([]byte(`{ "command": "git push", "timeout": 30 }`)))
	assert.Equal(t, "not json", NormalizeToolInput([]byte(" not json ")))
}
