//go:build linux

package session

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// tclaudeLayerRefusalPrefix is the marker every tclaude-layer capability
// refusal carries, matching the vocabulary the stacked tier already uses
// ("stacked requested — refused: …"). The pane's dying output is the only
// channel a relay-side refusal has, so the phrase has to be recognisable as a
// REFUSAL by a human reading `docs/sandboxing.md` rather than as one more
// process error.
const tclaudeLayerRefusalPrefix = "tclaude-layer requested — refused:"

// tclaudeLayerConfinementHint explains, in one sentence, why a capability the
// pre-flight probe reported healthy can be missing here. It is stated at the
// point of failure rather than left to the reader because the two processes
// look identical — same binary, same arguments, same host — and differ only in
// an ancestry nothing in the message would otherwise mention.
const tclaudeLayerConfinementHint = "this process inherits its confinement from the tmux server, " +
	"which the pre-flight capability probe cannot always observe " +
	"(AppArmor, SELinux, seccomp, or a differing no_new_privs)"

// tclaudeLayerStartRefusal names a failure to exec bubblewrap for what it is: a
// missing host capability, and therefore a refusal of the tclaude-layer
// boundary — not an opaque process error.
//
// TCL-1204. `probeBwrap` runs in the process that PREPARES the launch, while
// this exec happens inside the pane, several process hops away and under
// whatever confinement the tmux server carries. When the two disagree, the
// probe passes and the pane dies at exit 125 with a bare `fork/exec …:
// operation not permitted`. Reporting the denial as the refusal the probe would
// have produced does not restore the pre-flight contract — the pane still dies
// first — but it makes the surviving evidence name the cause, and it is the
// only backstop for a confinement that changes between probe and launch.
func tclaudeLayerStartRefusal(binary string, err error) error {
	switch {
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return fmt.Errorf("%s the host denied this process permission to execute bubblewrap (%s): %w; %s",
			tclaudeLayerRefusalPrefix, binary, err, tclaudeLayerConfinementHint)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s bubblewrap is no longer present at %s: %w",
			tclaudeLayerRefusalPrefix, binary, err)
	default:
		return fmt.Errorf("start bubblewrap: %w", err)
	}
}
