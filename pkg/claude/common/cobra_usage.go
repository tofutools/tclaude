package common

import "github.com/spf13/cobra"

// usageSilencedAnnotation marks a command whose failures print the error alone,
// with no usage block.
//
// cobra already has SilenceUsage for this, but it cannot be read as intent
// here: boa sets it on every command it generates, so the field is true across
// the whole tree and says nothing about any one command. An annotation the
// author has to add says exactly as much as it is asked to.
const usageSilencedAnnotation = "tclaude.usage_silenced"

// SilenceUsageOnError declares that this command's failures are read by someone
// who did not choose its arguments, and returns it for chaining.
//
// That is true of every argv tclaude renders for itself — pane wrappers, launch
// helpers, probes. Their flags are the launch's choices, not an operator's, and
// the pane they fail in is where the operator reads why the launch died. A usage
// block there documents decisions nobody made and pushes the one line that
// explains the failure out of view.
//
// This covers every failure cobra attributes to the command, not only the ones
// its RunE returns: a flag it could not parse is a flag tclaude rendered wrong,
// which is a tclaude bug rather than a typo the reader can be shown how to
// correct. `--help` is untouched — declining a usage block on failure does not
// decline help someone asked for.
//
// Only `session resource-limit-exec` claims this so far. The other rendered-argv
// commands have the same case for it and can be marked as their failures are
// found worth reading; nothing here decides for them.
func SilenceUsageOnError(cmd *cobra.Command) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[usageSilencedAnnotation] = "true"
	return cmd
}

// UsageSilenced reports whether cmd declined its usage block.
func UsageSilenced(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Annotations[usageSilencedAnnotation] == "true"
}
