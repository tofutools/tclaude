package agentd

import (
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// postInitDriveSelection reads which API drive, if any, a just-launched
// conversation took, for the post-init deliveries of op ("spawn", "clone").
// ok is false when the posture cannot be read; post-init is then abandoned,
// never sent to the pane.
func postInitDriveSelection(op, convID string, h *harness.Harness) (codexSelected, piSelected, ok bool) {
	switch h.Name {
	case harness.CodexName:
		selected, err := codexAppServerSelected(convID)
		if err != nil {
			slog.Error(op+": Codex app-server posture unreadable; post-init delivery abandoned without pane fallback",
				"conv", convID, "error", err)
			return false, false, false
		}
		return selected, false, true
	case harness.PiName:
		selected, err := piDriveSelected(convID)
		if err != nil {
			slog.Error(op+": pi drive posture unreadable; post-init delivery abandoned without pane fallback",
				"conv", convID, "error", err)
			return false, false, false
		}
		return false, selected, true
	}
	return false, false, true
}

// awaitPostInitDrive waits until an API-driven launch's drive can carry its
// post-init deliveries (the rename, the welcome or handoff). A live pane is
// not enough: the drive comes up after it, and a delivery sent before then
// is lost, not retried.
//
// An API-driven Copilot launch is not ready for post-init the moment its
// pane is alive. Its bootstrap creates a session under the conversation id
// and foregrounds it, so everything after it must wait for that, or it lands
// in the startup session the bootstrap is about to replace and is lost. See
// awaitCopilotAPISession, which waits on the CONNECTION and not on the
// launch posture, and explains why the posture cannot answer this. pi's
// extension and Codex's app-server likewise connect after the pane starts.
//
// false means the drive never came up within its budget; post-init is then
// abandoned, because API posture never falls back to pane input.
func awaitPostInitDrive(op, convID string, h *harness.Harness, codexSelected, piSelected bool) bool {
	if h.Name == harness.CopilotName && copilotLaunchIntentForConv(convID).API &&
		!awaitCopilotAPISession(convID) {
		why := "API posture never falls back to pane input"
		if op == "spawn" {
			why = "the failed launch is being shut down as crashed and " + why
		}
		slog.Error(op+": the Copilot API channel never came up within the bootstrap "+
			"budget; post-init delivery is abandoned because "+why,
			"conv", convID, "budget", copilotAPIBootstrapTimeout())
		return false
	}
	if piSelected && !awaitPiDrive(convID) {
		slog.Error(op+": the pi drive channel never connected; post-init delivery is abandoned without pane fallback",
			"conv", convID, "budget", piDriveConnectTimeout)
		return false
	}
	if codexSelected && !awaitCodexAppServerReady(convID) {
		slog.Error(op+": the Codex app-server channel never became ready; post-init delivery is abandoned without pane fallback",
			"conv", convID, "budget", codexAppServerStartupTimeout)
		return false
	}
	return true
}
