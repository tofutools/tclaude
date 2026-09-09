package claude

import (
	"encoding/json"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
	"time"
)

type nativeContextUsage struct {
	EffectiveWindow *observedCompactionWindow `json:"tclaude_compaction_window"`
	SessionID       string                    `json:"session_id"`
	AgentID         string                    `json:"agent_id"`
	Window          *struct {
		Size    int64    `json:"context_window_size"`
		Percent *float64 `json:"used_percentage"`
	} `json:"context_window"`
}

func parseContextUsage(raw []byte, nativeID string, window model.AutoCompactWindow, at time.Time) *model.ContextUsage {
	var value nativeContextUsage
	if json.Unmarshal(raw, &value) != nil || value.SessionID != nativeID || value.AgentID != "" || value.Window == nil || value.Window.Percent == nil || value.Window.Size <= 0 {
		return nil
	}
	percent := *value.Window.Percent
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		return nil
	}
	tokens := model.AutoCompactWindowTokens(string(window))
	if observed := value.EffectiveWindow; observed != nil {
		if !observed.Known {
			return nil
		}
		if observed.Tokens != 0 && (observed.Tokens < 10000 || observed.Tokens > 10000000) {
			return nil
		}
		tokens = observed.Tokens
	}
	effective := model.EffectiveContextWindow(value.Window.Size, tokens)
	return &model.ContextUsage{ModelWindow: value.Window.Size, EffectiveWindow: effective, NativePercent: percent, UsedPercent: model.RebaseContextPercentage(percent, value.Window.Size, effective), ObservedAt: at}
}

func (*Provider) ProjectContextUsage(execution model.Execution) *model.ContextUsage {
	recorded, err := decodeEvidence(execution.Evidence)
	if err != nil || recorded.ExecutionID != string(execution.ID) || !recorded.ContextReady || recorded.ContextUsage == nil || execution.ContextReadiness != model.ContextReadinessReady {
		return nil
	}
	if execution.NativeConversation == nil || recorded.NativeID != execution.NativeConversation.Reference || execution.NativeConversation.Namespace != NativeNamespace {
		return nil
	}
	copy := *recorded.ContextUsage
	return &copy
}
