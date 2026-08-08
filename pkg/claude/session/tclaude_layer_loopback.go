package session

import (
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// TclaudeLayerSharesHostLoopback reports whether a launch under this
// implementation and resolved profile leaves the harness in the HOST network
// namespace — so a loopback port chosen outside the sandbox is the same
// loopback port the harness binds inside it.
//
// This is the question a tclaude-owned TCP channel to an in-pane harness stands
// on. tclaude picks the port before the process exists, and the process binds
// it one exec deeper inside the wrap; if the wrap created its own network
// namespace, those are two different 127.0.0.1s and nothing tclaude allocates
// is ever reachable. See TCL-1054.
//
// The answer is the FLOOR posture, not the authored one, and it is asked of the
// same helpers the launch renderer asks (TclaudeLayerNetworkPosture,
// TclaudeLayerNetworkEngine, TclaudeLayerFloorPosture). A filtered profile under
// the proxy engine builds the isolated posture's namespace, so reading the
// authored posture alone would answer for a launch that does not happen. Two
// ways of deciding this would eventually disagree, and the disagreement would
// surface as a channel that is refused when it would have worked, or — far
// worse — accepted when it cannot.
//
// A non-tclaude-layer implementation shares host loopback trivially: tclaude
// creates no network namespace, so there is only one. That arm is stated rather
// than assumed because it is the answer for `--sandbox-impl off` and for the
// harness-builtin tier, neither of which has a posture to read.
//
// The returned posture name is for the caller's message. It is the floor the
// decision was made on, so a refusal names the namespace the launch would
// actually have built.
func TclaudeLayerSharesHostLoopback(
	implementation sandboxpolicy.Implementation,
	effective sandboxpolicy.EffectiveProfile,
) (bool, string, error) {
	if !implementation.UsesTclaudeLayer() {
		return true, sandboxpolicy.NetworkHostOpen.String(), nil
	}
	posture, err := TclaudeLayerNetworkPosture(effective)
	if err != nil {
		return false, "", err
	}
	engine, err := TclaudeLayerNetworkEngine(effective)
	if err != nil {
		return false, "", err
	}
	floor := TclaudeLayerFloorPosture(posture, engine)
	return floor == sandboxpolicy.NetworkHostOpen, floor.String(), nil
}
