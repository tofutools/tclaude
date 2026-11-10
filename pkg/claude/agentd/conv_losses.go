package agentd

import (
	"encoding/json"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// storedConvLosses is the loss report of the conversion that produced convID,
// or nil when it has none. An unreadable report only shortens the chain.
func storedConvLosses(convID string) *paf.Losses {
	raw, err := db.ConvLossReport(convID)
	if err != nil {
		slog.Warn("paf: read conversation loss report", "conv", short8(convID), "error", err)
		return nil
	}
	if raw == nil {
		return nil
	}
	l := &paf.Losses{}
	if err := json.Unmarshal(raw, l); err != nil {
		slog.Warn("paf: decode conversation loss report", "conv", short8(convID), "error", err)
		return nil
	}
	return l
}

// recordConvLosses stores what a conversion did to convID, on top of the
// report it arrived with. A nil report means convID holds the earlier
// conversation unchanged, so the earlier report stands. Best effort: the
// conversation is already in place, and the report only describes it.
func recordConvLosses(convID string, earlier *paf.Losses, report *transcript.LossReport) {
	var l paf.Losses
	switch {
	case report != nil:
		l = paf.ChainLosses(earlier, paf.LossesFromReport(report))
	case earlier != nil:
		l = *earlier
	default:
		return
	}
	raw, err := json.Marshal(l)
	if err == nil {
		err = db.SetConvLossReport(convID, raw)
	}
	if err != nil {
		slog.Warn("paf: record conversation loss report", "conv", short8(convID), "error", err)
	}
}
