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
// That is the case for every argv tclaude renders for itself — a pane wrapper,
// a launch helper, a probe. Their flags are the launch's choices, not an
// operator's, and the pane they fail in is where the operator reads why the
// launch died. A usage block there documents decisions nobody made and pushes
// the one line that explains the failure out of view.
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
