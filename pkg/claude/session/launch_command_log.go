package session

import (
	"strings"
)

// envRedactionPlaceholder stands in for the whole environment prefix.
const envRedactionPlaceholder = "export <environment redacted>; "

// maxShellQuoteNesting bounds how many times the environment prefix may have
// been re-quoted on its way into the final pane command. Each wrapper layer
// that embeds the harness command as a single-quoted argument (the resource
// cgroup's --command, bubblewrap's `sh -c`, the exit gate) adds one level, and
// each level rewrites every `'` as `'\''`. Four covers the deepest chain the
// launch path builds, with room to spare.
const maxShellQuoteNesting = 6

// RedactPaneCommand removes the environment prefix from a pane command so the
// command's SHAPE can be logged without its values.
//
// The prefix is clcommon.BuildEnvExports's `export K=V; …`, which forwards the
// operator's whole environment plus the sandbox profile's authored Environment
// entries — API keys among them. The shape is what diagnoses a failed launch
// (which wrapper layers applied, which binary ran, with which flags); the
// values are never the answer and must not reach ~/.tclaude/data/output.log.
//
// It works by LOCATING the exact prefix the caller built rather than by parsing
// shell syntax. An earlier version scanned for `export NAME=` and tried to skip
// each value by reading its quoting; that silently under-redacted as soon as
// the command was quoted twice, because an inner value then opens with the
// four-byte sequence '\'' instead of a bare quote and the scanner mistook the
// third byte for the closing quote. Locating a known string cannot make that
// class of mistake: either the prefix is found and removed whole, or it is not.
//
// ok is false when the prefix cannot be located, and the caller must then log
// NO command at all. Failing closed is the point: a redactor that cannot prove
// it removed the environment has to be treated as having failed, not as having
// found nothing to remove.
func RedactPaneCommand(command, envExports string) (string, bool) {
	if strings.TrimSpace(envExports) == "" {
		// A launch with no environment prefix has nothing to hide.
		return command, true
	}
	form := envExports
	for depth := 0; depth < maxShellQuoteNesting; depth++ {
		if i := strings.Index(command, form); i >= 0 {
			redacted := command[:i] + envRedactionPlaceholder + command[i+len(form):]
			// The prefix can appear more than once (a wrapper that re-states
			// the command). Removing one occurrence while leaving another would
			// still write the values, so require that none survives.
			if strings.Contains(redacted, form) {
				return "", false
			}
			return redacted, true
		}
		// How ShellQuoteArg re-encodes the prefix when the command that carries
		// it is itself embedded as a single-quoted argument.
		form = strings.ReplaceAll(form, "'", `'\''`)
	}
	return "", false
}
