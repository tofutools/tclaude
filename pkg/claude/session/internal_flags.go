package session

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// internalFlagPrefix marks a NewParams flag as a daemon-to-session seam rather
// than an operator option. Such flags keep working but stay out of --help.
const internalFlagPrefix = "Internal:"

// HideInternalFlags hides every flag whose help text starts with "Internal:".
// Keying on the help prefix keeps a newly added internal seam out of --help
// without a second list to maintain.
func HideInternalFlags(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if strings.HasPrefix(f.Usage, internalFlagPrefix) {
			f.Hidden = true
		}
	})
}
