package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestSetNewGroupAttachmentRequiresExactlyOneGroup(t *testing.T) {
	testharness.New(t)

	err := setNewGroupAttachment("missing", "https://example.com/project", "Project")
	require.ErrorContains(t, err, "updated 0 rows, want 1")
}
