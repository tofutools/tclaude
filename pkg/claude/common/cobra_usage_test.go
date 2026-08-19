package common

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestSilenceUsageOnErrorMarksOnlyTheCommandItIsGiven(t *testing.T) {
	parent := &cobra.Command{Use: "parent"}
	child := SilenceUsageOnError(&cobra.Command{Use: "child"})
	parent.AddCommand(child)

	assert.True(t, UsageSilenced(child))
	assert.False(t, UsageSilenced(parent),
		"declining a usage block is a statement about one command, not a tree")
	assert.False(t, UsageSilenced(nil))
	assert.False(t, UsageSilenced(&cobra.Command{Use: "unmarked"}))
}

func TestSilenceUsageOnErrorKeepsOtherAnnotations(t *testing.T) {
	cmd := &cobra.Command{Use: "cmd", Annotations: map[string]string{"group": "session"}}

	assert.Same(t, cmd, SilenceUsageOnError(cmd), "the command is returned for chaining")
	assert.Equal(t, "session", cmd.Annotations["group"])
	assert.True(t, UsageSilenced(cmd))
}
