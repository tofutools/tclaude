package session

import (
	"strings"
)

// envRedactionPlaceholder stands in for the whole environment prefix.
const envRedactionPlaceholder = "export <environment redacted>; "

// maxShellQuoteNesting bounds how many times the environment prefix may have
// been re-quoted on its way into the final pane command. Each wrapper layer
// that embeds the harness command as a single-quoted argument (the resource
// cgroup's --command, bubblewrap's inner shell, the exit gate) adds one level,
// and each level re-escapes every quote in what it wraps. Six covers the
// deepest chain the launch path builds, with room to spare.
const maxShellQuoteNesting = 6

// RedactPaneCommand removes environment VALUES from a pane command so its
// SHAPE can be logged.
//
// The shape is what diagnoses a failed launch: which wrapper layers applied,
// which binary ran, with which flags. The values are never the answer, and
// ~/.tclaude/data/output.log — which the dashboard's Logs tab renders — is no
// place for the operator's API keys.
//
// It works in two steps, because values reach the command through more than
// one channel:
//
//  1. Remove the clcommon.BuildEnvExports prefix by LOCATING it, not by parsing
//     shell syntax. An earlier version scanned for assignments and skipped each
//     value by reading its quoting; that silently under-redacted as soon as the
//     command was quoted twice, because a nested value no longer opens with a
//     bare quote. Locating a known string cannot make that class of mistake.
//
//  2. Refuse to return anything in which a known secret value still appears.
//     Step 1 alone was not enough: a Codex launch renders the sandbox profile's
//     authored Environment entries a SECOND time, as
//     shell_environment_policy.set overrides on its own argv, so removing the
//     prefix left the same secrets in the command and reported success. Pass
//     every authored value in secretValues and this catches any such channel,
//     including ones added later that this function knows nothing about.
//
// ok is false when redaction cannot be proved, and the caller must then log NO
// command at all. Failing closed is the point: a redactor that cannot show it
// removed the environment has to be treated as having failed, not as having
// found nothing to remove.
//
// Two limits worth stating. Values inherited from the operator's environment
// are covered by step 1 only — they cannot be passed to step 2, because values
// like HOME and PWD are legitimate substrings of ordinary command text and
// would withhold every command. And SpawnSpec.PreLaunchScript, operator-
// authored shell from the profile's pre_launch blocks, is rendered verbatim and
// is not scanned; an operator who exports a secret there is logging it.
func RedactPaneCommand(command, envExports string, secretValues []string) (string, bool) {
	redacted := command
	if envExports != "" {
		located := false
		form := envExports
		for depth := 0; depth < maxShellQuoteNesting; depth++ {
			if i := strings.Index(redacted, form); i >= 0 {
				redacted = redacted[:i] + envRedactionPlaceholder + redacted[i+len(form):]
				located = true
				break
			}
			form = nestShellQuoting(form)
		}
		if !located {
			return "", false
		}
		// A wrapper that restates the command would leave a second copy. Check
		// EVERY depth, not just the one that matched: the copies need not be at
		// the same nesting level, and a same-depth-only check would pass while
		// a re-quoted environment dump survived.
		form = envExports
		for depth := 0; depth < maxShellQuoteNesting; depth++ {
			if strings.Contains(redacted, form) {
				return "", false
			}
			form = nestShellQuoting(form)
		}
	}
	for _, value := range secretValues {
		if value == "" {
			continue
		}
		form := value
		for depth := 0; depth < maxShellQuoteNesting; depth++ {
			if strings.Contains(redacted, form) {
				return "", false
			}
			form = nestShellQuoting(form)
		}
	}
	return redacted, true
}

// nestShellQuoting re-encodes a string the way ShellQuoteArg does when the
// command carrying it is itself embedded as a single-quoted argument: every
// quote becomes close-quote, backslash-quote, open-quote.
func nestShellQuoting(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}
