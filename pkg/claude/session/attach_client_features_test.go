//go:build !windows

package session

import (
	"slices"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// The dashboard's web terminals reach tmux through `tclaude session attach`
// rather than forking tmux themselves, so the "this client renders OSC 8" fact
// has to cross a process boundary as an env var and come back out as tmux's -T.
// Without it tmux keeps every hyperlink target in its grid and the browser sees
// only dead label text.
func TestAttachPassesClientFeaturesFromEnv(t *testing.T) {
	rec := withRecordingTmux(t)
	t.Setenv(clcommon.TmuxClientFeaturesEnv, clcommon.TmuxHyperlinksFeature)

	_ = attachToSessionWithFlags("spwn-abc123", false)

	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly one tmux invocation, got %v", rec.calls)
	}
	args := rec.calls[0]
	// tmux parses client flags only BEFORE the command word; -T after
	// `attach-session` would be read as one of its options and fail the client.
	want := []string{"-T", clcommon.TmuxHyperlinksFeature, "attach-session"}
	if len(args) < 3 || !slices.Equal(args[:3], want) {
		t.Fatalf("client features must precede the command word: got %v, want prefix %v", args, want)
	}
	if !slices.Contains(args, "spwn-abc123") && !slices.Contains(args, clcommon.ExactTarget("spwn-abc123")) {
		t.Fatalf("attach lost its target session: %v", args)
	}
}

// A native terminal attach shares this code path, and an arbitrary local
// terminal may not handle OSC 8. With no request in the environment the attach
// must stay byte-identical to what it was, leaving tmux's own detection alone.
func TestAttachWithoutClientFeaturesEnvIsUnchanged(t *testing.T) {
	rec := withRecordingTmux(t)
	t.Setenv(clcommon.TmuxClientFeaturesEnv, "")

	_ = attachToSessionWithFlags("spwn-abc123", true)

	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly one tmux invocation, got %v", rec.calls)
	}
	args := rec.calls[0]
	if slices.Contains(args, "-T") {
		t.Fatalf("unrequested client features leaked into a native attach: %v", args)
	}
	// The force flag is what makes the web "open window" path displace a stale
	// client; it must survive the feature-prefix insertion.
	if len(args) < 2 || args[0] != "attach-session" || args[1] != "-d" {
		t.Fatalf("forced attach shape changed: %v", args)
	}
}

// A malformed environment value must degrade to "no hyperlinks", never to a
// broken client: tmux treats an unparsable -T as fatal, which would replace a
// working terminal with a blank one.
func TestAttachIgnoresMalformedClientFeatures(t *testing.T) {
	rec := withRecordingTmux(t)
	t.Setenv(clcommon.TmuxClientFeaturesEnv, "hyperlinks; rm -rf /")

	_ = attachToSessionWithFlags("spwn-abc123", false)

	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly one tmux invocation, got %v", rec.calls)
	}
	if slices.Contains(rec.calls[0], "-T") {
		t.Fatalf("malformed feature list must be dropped, not forwarded: %v", rec.calls[0])
	}
}
