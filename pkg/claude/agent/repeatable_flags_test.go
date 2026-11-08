package agent

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repeatable flag whose value is free text (a choice) or a path is one
// value per flag: a comma inside it stays inside it. boa maps a []string
// field to pflag's StringSlice, which splits each value at commas, unless the
// field is tagged collection:"array".
func TestRepeatableFlags_ACommaStaysInsideItsValue(t *testing.T) {
	cases := []struct {
		name string
		cmd  func() *cobra.Command
		flag string
	}{
		{"ask-operator --option", taskboardAskOperatorCmd, "option"},
		{"ask-operator --attach", taskboardAskOperatorCmd, "attach"},
		{"taskboard add --attach", taskboardAddCmd, "attach"},
		{"taskboard ask --attach", taskboardAskCmd, "attach"},
		{"taskboard comment add --attach", taskboardCommentAddCmd, "attach"},
		{"notify-human --option", notifyHumanCmd, "option"},
		{"notify-human --attach", notifyHumanCmd, "attach"},
		{"message --attach", messageCmd, "attach"},
		{"reply --attach", replyCmd, "attach"},
		{"guide show --circle", guideShowCmd, "circle"},
		{"guide show --underline", guideShowCmd, "underline"},
		{"guide show --arrow", guideShowCmd, "arrow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Parsing only: running the command would reach a daemon.
			cmd := tc.cmd()
			require.NoError(t, cmd.ParseFlags([]string{"--" + tc.flag, "Ship it: tests, docs, done", "--" + tc.flag, "b,c.png"}))
			got, err := cmd.Flags().GetStringArray(tc.flag)
			require.NoError(t, err, "--%s must be a StringArray flag", tc.flag)
			assert.Equal(t, []string{"Ship it: tests, docs, done", "b,c.png"}, got)
		})
	}
}
