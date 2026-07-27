package agent

import "errors"

// TCL-791 removed break-glass. These tombstones exist so the removal is LOUD.
//
// Deleting the flag outright would make Cobra emit a generic "unknown flag",
// which tells a script that something is wrong but not what replaced it or
// why. Keeping the flag declared and failing with the real reason is the
// difference between a scripted caller learning "break-glass is gone, turn the
// sandbox off instead" and learning "typo somewhere".
//
// boa has no hidden-flag support, so the tombstone stays listed in --help.
// That is loudness, not cost: an operator reading the flag list sees the
// removal at exactly the moment they would otherwise have reached for it. The
// help text is spelled out at each declaration rather than shared through a
// constant, because struct tags cannot interpolate one.

// breakGlassFlagRemoved is the error every command returns when the tombstoned
// flag is set. Same real reason and same "disable the sandbox" pointer as the
// daemon's wire rejection, so an operator gets one consistent explanation
// whichever surface they hit first.
func breakGlassFlagRemoved() error {
	return errors.New(
		"--i-understand-break-glass-risk no longer exists: the break-glass feature was removed. " +
			"Protected tclaude/harness state (~/.tclaude/data, ~/.claude/sessions) is unreachable from a " +
			"sandboxed agent, with no profile, include, acknowledgement, or flag that reopens it. " +
			"Remove the flag. To work without the protected-root wall, launch with the sandbox disabled")
}
