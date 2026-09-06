package codex

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"encoding/json"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type usageReader struct{ provider *Provider }

func (p *Provider) Usage() ports.UsageReader { return usageReader{provider: p} }
func (usageReader) Capabilities() ports.UsageCapabilities {
	return ports.UsageCapabilities{CollectCounters: true, CoverageNote: "Codex rollout token_count totals; no native monetary cost field"}
}

func (r usageReader) Collect(ctx context.Context, request ports.UsageCollectionRequest) (ports.CollectedUsage, error) {
	path, key, err := r.rollout(ctx, request)
	if err != nil {
		return ports.CollectedUsage{}, err
	}
	if path == "" {
		return ports.CollectedUsage{SourceKey: "codex:" + request.Native.Reference, Source: "codex.rollout.token_count", SourceRevision: "unresolved-v1", ObservedAt: request.Execution.UpdatedAt,
			Coverage: model.UsageCoverage{Counters: model.UsageCoverageUnknown, Cost: model.UsageCoverageUnsupported, Reason: "attributed Codex rollout is unavailable"}, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ports.CollectedUsage{}, err
	}
	counters, observed, partial := collectCodexUsage(raw)
	coverage := model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageUnsupported, Reason: "Codex token_count does not report monetary cost"}
	if partial {
		coverage.Counters = model.UsageCoveragePartial
	}
	if observed.IsZero() {
		observed = request.Execution.UpdatedAt
	}
	return ports.CollectedUsage{SourceKey: key, Source: "codex.rollout.token_count", SourceRevision: digest(raw), ObservedAt: observed, Counters: counters, Coverage: coverage, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
}

func (r usageReader) rollout(ctx context.Context, request ports.UsageCollectionRequest) (string, string, error) {
	if request.History != nil {
		token, err := decodeSourceToken(request.History.SourceToken)
		if err != nil || token.SessionID != request.Native.Reference || filepath.Clean(token.StateRoot) != r.provider.nativeHome {
			return "", "", fmt.Errorf("codex usage history binding is invalid")
		}
		return token.Transcript, "codex:" + sourceFingerprint(token), nil
	}
	var found string
	err := filepath.WalkDir(filepath.Join(r.provider.nativeHome, "sessions"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" || found != "" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err == nil {
			head, _ := parseRolloutHead(raw)
			if head.ID == request.Native.Reference {
				found = path
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return found, "codex:" + request.Native.Reference, nil
}

func collectCodexUsage(raw []byte) ([]model.UsageCounter, time.Time, bool) {
	var latest *struct {
		Input, Cached, Output, Reasoning, Total int64
	}
	var observed time.Time
	partial := false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var envelope struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
			partial = true
			continue
		}
		if envelope.Type != "event_msg" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Info *struct {
				Total *struct {
					Input     int64 `json:"input_tokens"`
					Cached    int64 `json:"cached_input_tokens"`
					Output    int64 `json:"output_tokens"`
					Reasoning int64 `json:"reasoning_output_tokens"`
					Total     int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		}
		if json.Unmarshal(envelope.Payload, &event) != nil || event.Type != "token_count" || event.Info == nil || event.Info.Total == nil {
			continue
		}
		candidate := struct{ Input, Cached, Output, Reasoning, Total int64 }{event.Info.Total.Input, event.Info.Total.Cached, event.Info.Total.Output, event.Info.Total.Reasoning, event.Info.Total.Total}
		if candidate.Input < 0 || candidate.Cached < 0 || candidate.Output < 0 || candidate.Reasoning < 0 {
			partial = true
			continue
		}
		latest = &candidate
		if parsed, err := time.Parse(time.RFC3339Nano, envelope.Timestamp); err == nil {
			observed = parsed.UTC()
		}
	}
	partial = partial || scanner.Err() != nil
	if latest == nil {
		return nil, observed, partial
	}
	return []model.UsageCounter{{Unit: model.UsageInputTokens, Value: latest.Input}, {Unit: model.UsageOutputTokens, Value: latest.Output}, {Unit: model.UsageCacheReadTokens, Value: latest.Cached}, {Unit: model.UsageReasoningTokens, Value: latest.Reasoning}}, observed, partial
}

var _ ports.UsageProvider = (*Provider)(nil)
