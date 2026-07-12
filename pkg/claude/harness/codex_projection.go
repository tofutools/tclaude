package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// CodexRolloutProjection is the latest-oriented subset hook callbacks lift
// from a rollout. Each value is a snapshot carried by one record rather than
// an accumulator over the event stream, so a plain rollout can be projected
// by reading records newest-first. The scan stops once every field is found;
// when a field is absent, it degrades to one full backward pass.
type CodexRolloutProjection struct {
	Context    ContextTelemetry
	HasContext bool
	Effort     string
	HasEffort  bool
	Usage      *CodexUsage
	Cost       CodexTokenCost
	HasCost    bool
}

// CodexHookProjection prefers the exact rollout path carried by a hook. Older
// payloads without transcript_path fall back to normal by-id discovery.
func CodexHookProjection(home, convID, transcriptPath, modelHint string) (CodexRolloutProjection, string, error) {
	path := transcriptPath
	if !IsCodexRolloutPath(path) {
		var err error
		path, err = findCodexRollout(home, convID)
		if err != nil || path == "" {
			return CodexRolloutProjection{}, path, err
		}
	}
	projection, err := CodexHookProjectionFromRollout(path, modelHint)
	return projection, path, err
}

// CodexHookProjectionFromRollout derives context, reasoning effort,
// subscription usage, and virtual cost in one scan. Live .jsonl rollouts are
// scanned from the tail; archived .zst rollouts cannot be sought in compressed
// form and use one combined forward scan instead.
func CodexHookProjectionFromRollout(path, modelHint string) (CodexRolloutProjection, error) {
	state := codexProjectionScanState{modelHint: strings.TrimSpace(modelHint)}
	if strings.HasSuffix(path, ".zst") {
		rc, err := openCodexRollout(path)
		if err != nil {
			return CodexRolloutProjection{}, err
		}
		defer func() { _ = rc.Close() }()
		if err := scanCodexRolloutLines(rc, path, func(line []byte) bool {
			state.consumeForward(line)
			return true
		}); err != nil {
			return CodexRolloutProjection{}, fmt.Errorf("scan codex rollout %s: %w", path, err)
		}
	} else if err := scanCodexRolloutLinesReverse(path, func(line []byte) bool {
		state.consumeReverse(line)
		return !state.complete()
	}); err != nil {
		return CodexRolloutProjection{}, fmt.Errorf("scan codex rollout %s: %w", path, err)
	}
	return state.projection(), nil
}

type codexProjectionScanState struct {
	modelHint string
	model     string
	info      *codexTokenCountInfo
	observed  string
	effort    string
	usage     *CodexUsage
}

func (s *codexProjectionScanState) consumeForward(line []byte) {
	env, ok := decodeCodexProjectionEnvelope(line)
	if !ok {
		return
	}
	switch env.Type {
	case "turn_context":
		model, effort := projectCodexTurnContext(env.Payload)
		if model != "" {
			s.model = model
		}
		if effort != "" {
			s.effort = effort
		}
	case "event_msg":
		info, usage, ok := projectCodexTokenCount(env)
		if !ok {
			return
		}
		s.info = &info
		s.observed = env.Timestamp
		if usage != nil {
			s.usage = usage
		}
	}
}

func (s *codexProjectionScanState) consumeReverse(line []byte) {
	env, ok := decodeCodexProjectionEnvelope(line)
	if !ok {
		return
	}
	switch env.Type {
	case "turn_context":
		model, effort := projectCodexTurnContext(env.Payload)
		if s.model == "" {
			s.model = model
		}
		if s.effort == "" {
			s.effort = effort
		}
	case "event_msg":
		info, usage, ok := projectCodexTokenCount(env)
		if !ok {
			return
		}
		if s.info == nil {
			s.info = &info
			s.observed = env.Timestamp
		}
		if s.usage == nil && usage != nil {
			s.usage = usage
		}
	}
}

func decodeCodexProjectionEnvelope(line []byte) (codexEnvelope, bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return codexEnvelope{}, false
	}
	var env codexEnvelope
	return env, json.Unmarshal(line, &env) == nil
}

func projectCodexTurnContext(payload json.RawMessage) (model, effort string) {
	var tc codexTurnContext
	if json.Unmarshal(payload, &tc) != nil {
		return "", ""
	}
	model = strings.TrimSpace(tc.Model)
	effort = tc.Effort
	if effort == "" {
		effort = tc.CollaborationMode.Settings.ReasoningEffort
	}
	if v, err := (codexModels{}).ValidateEffort(effort); err == nil {
		effort = v
	} else {
		effort = ""
	}
	return model, effort
}

func projectCodexTokenCount(env codexEnvelope) (codexTokenCountInfo, *CodexUsage, bool) {
	var ev codexTokenCountEvent
	if json.Unmarshal(env.Payload, &ev) != nil || ev.Type != "token_count" {
		return codexTokenCountInfo{}, nil, false
	}
	return ev.Info, codexUsageFromRateLimits(ev.RateLimits, env.Timestamp), true
}

func (s *codexProjectionScanState) complete() bool {
	if s.info == nil || s.effort == "" || s.usage == nil {
		return false
	}
	if _, ok := codexModelPrices[s.modelHint]; ok {
		return true
	}
	return s.model != ""
}

func (s *codexProjectionScanState) projection() CodexRolloutProjection {
	out := CodexRolloutProjection{Effort: s.effort, HasEffort: s.effort != "", Usage: s.usage}
	if s.info == nil {
		return out
	}
	context := contextTelemetryFromTokenCount(*s.info)
	if context.TokensInput != 0 || context.TokensOutput != 0 {
		out.Context = context
		out.HasContext = true
	}
	for _, model := range []string{s.modelHint, s.model} {
		if cost, ok := codexVirtualCost(model, s.info.TotalTokenUsage, s.info.ModelContextWindow); ok {
			out.Cost = CodexTokenCost{CostUSD: cost, Model: model, Observed: parseCodexEventTime(s.observed)}
			out.HasCost = true
			break
		}
	}
	return out
}

// scanCodexRolloutLinesReverse visits plain rollout records newest-first while
// retaining at most maxCodexRolloutLineBytes. A record larger than the limit is
// discarded as chunks are read, so a multi-MiB compacted.replacement_history
// cannot prevent older telemetry from being reached.
func scanCodexRolloutLinesReverse(path string, visit func([]byte) bool) error {
	f, err := os.Open(path) //nolint:gosec // hook supplies Codex's rollout path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	const blockSize int64 = 64 * 1024
	reversed := make([]byte, 0, 64*1024)
	var lineBytes int64
	emit := func() bool {
		if lineBytes > maxCodexRolloutLineBytes {
			slog.Warn("codex-projection: skipping oversized rollout record",
				"path", path, "bytes", lineBytes,
				"limit_bytes", maxCodexRolloutLineBytes, "module", "harness")
		} else {
			for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
				reversed[left], reversed[right] = reversed[right], reversed[left]
			}
			if !visit(reversed) {
				return false
			}
		}
		reversed = reversed[:0]
		lineBytes = 0
		return true
	}

	buf := make([]byte, blockSize)
	for end := info.Size(); end > 0; {
		start := end - blockSize
		if start < 0 {
			start = 0
		}
		n, readErr := f.ReadAt(buf[:end-start], start)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				if lineBytes > 0 {
					lineBytes++ // match the forward reader's newline-inclusive limit/count
				}
				if lineBytes > 0 && !emit() {
					return nil
				}
				continue
			}
			lineBytes++
			if lineBytes <= maxCodexRolloutLineBytes {
				reversed = append(reversed, buf[i])
			}
		}
		end = start
	}
	if lineBytes > 0 {
		emit()
	}
	return nil
}
