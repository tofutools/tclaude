package agent

import (
	"fmt"
	"io"
	"os"
)

// resolveBodyInput resolves a free-text body that the caller may supply
// either inline (a string flag or positional argument) or load from a
// file via --file. The two sources are mutually exclusive.
//
//   - file == ""             → inline is returned unchanged; behaviour is
//     exactly as it was before --file existed.
//   - file set, inline empty → the file is read and its content returned.
//   - both set               → usage error (rcInvalidArg).
//
// A file path of "-" reads the body from stdin, so a brief can be piped
// in (e.g. `generate-brief | tclaude agent spawn … --file -`). A missing
// or unreadable file is surfaced as a clear error here, before the
// caller spawns anything or hits the daemon.
//
// Loading the body from a file also sidesteps shell quoting entirely —
// notably backticks, which the shell would otherwise eat from a body
// passed inline on the command line.
//
// inlineName is the human-facing name of the inline source (e.g.
// "--initial-message", "--body", or "the follow-up argument") and is
// used only to phrase the mutual-exclusion error.
//
// On error the message is already written to stderr; the returned int is
// the process exit code (rcOK on success).
func resolveBodyInput(inline, file, inlineName string, stdin io.Reader, stderr io.Writer) (string, int) {
	if file == "" {
		return inline, rcOK
	}
	// Any non-empty inline value counts as "given" — even whitespace —
	// so a caller that supplied both sources gets a clear error rather
	// than a silent pick. An empty string is an unset flag / omitted
	// positional and does not conflict.
	if inline != "" {
		fmt.Fprintf(stderr, "Error: pass %s OR --file, not both\n", inlineName)
		return "", rcInvalidArg
	}
	if file == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "Error: reading body from stdin: %v\n", err)
			return "", rcIOFailure
		}
		return string(data), rcOK
	}
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "Error: reading --file %q: %v\n", file, err)
		return "", rcIOFailure
	}
	return string(data), rcOK
}
