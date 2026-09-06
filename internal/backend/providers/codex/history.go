package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type historyReader struct{ provider *Provider }
type sourceToken struct {
	StateRoot  string `json:"state_root"`
	SessionID  string `json:"session_id"`
	Transcript string `json:"transcript"`
}
type rolloutHead struct {
	ID, CWD, Title string
	Created        time.Time
	Points         []ports.ProviderHistoryPoint
}

func (historyReader) Capabilities() ports.HistoryCapabilities {
	return ports.HistoryCapabilities{MetadataDiscovery: true, ContentRead: true, ContinuationPrecision: ports.HistoryPrecisionHead, ForkPrecision: ports.HistoryPrecisionTurn}
}
func (h historyReader) Discover(ctx context.Context, request ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	statesRoot := h.provider.nativeHome
	var found []ports.DiscoveredHistory
	partial := false
	err := filepath.WalkDir(statesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			partial = true
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		item, itemErr := discoverRollout(statesRoot, path, request.Scope)
		if itemErr != nil {
			partial = true
		} else if item != nil {
			found = append(found, *item)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ModifiedAt.After(found[j].ModifiedAt) })
	coverage := model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoveragePartial, RefreshedAt: time.Now().UTC()}
	if partial {
		coverage.Metadata = model.HistoryCoveragePartial
	}
	return ports.HistoryDiscoveryResult{Histories: found, Coverage: coverage}, nil
}
func discoverRollout(stateRoot, transcript string, scope ports.HistoryDiscoveryScope) (*ports.DiscoveredHistory, error) {
	raw, err := os.ReadFile(transcript)
	if err != nil {
		return nil, err
	}
	head, partial := parseRolloutHead(raw)
	if head.ID == "" || head.CWD == "" {
		return nil, fmt.Errorf("invalid Codex rollout head")
	}
	info, err := os.Stat(transcript)
	if err != nil {
		return nil, err
	}
	modified := info.ModTime().UTC()
	if !scope.ModifiedAfter.IsZero() && !modified.After(scope.ModifiedAfter) {
		return nil, nil
	}
	if scope.WorkspaceHint != "" && filepath.Clean(scope.WorkspaceHint) != filepath.Clean(head.CWD) {
		return nil, nil
	}
	revision := digest(raw)
	tokenRaw, _ := json.Marshal(sourceToken{StateRoot: stateRoot, SessionID: head.ID, Transcript: transcript})
	content := model.HistoryCoverageComplete
	if partial {
		content = model.HistoryCoveragePartial
	}
	ev, _ := model.NewProviderEvidence(Name, evidenceVersion, tokenRaw)
	token := sourceToken{StateRoot: stateRoot, SessionID: head.ID, Transcript: transcript}
	return &ports.DiscoveredHistory{Native: model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: head.ID, ObservedAt: time.Now().UTC()}, SourceToken: string(tokenRaw), SourceFingerprint: sourceFingerprint(token), Title: head.Title, WorkspaceHint: head.CWD, ModifiedAt: modified, Availability: model.HistoryContent, Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: content, SourceRevision: revision, RefreshedAt: time.Now().UTC()}, Points: head.Points, Evidence: ev}, nil
}
func parseRolloutHead(raw []byte) (rolloutHead, bool) {
	var result rolloutHead
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
		switch envelope.Type {
		case "session_meta":
			var meta struct {
				ID        string `json:"id"`
				CWD       string `json:"cwd"`
				Timestamp string `json:"timestamp"`
			}
			if json.Unmarshal(envelope.Payload, &meta) != nil {
				partial = true
				continue
			}
			result.ID, result.CWD, result.Created = meta.ID, meta.CWD, parseTime(meta.Timestamp)
		case "event_msg":
			var event struct {
				Type    string `json:"type"`
				TurnID  string `json:"turn_id"`
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Payload, &event) != nil {
				partial = true
				continue
			}
			if event.Type == "task_started" && event.TurnID != "" {
				result.Points = append(result.Points, ports.ProviderHistoryPoint{Token: event.TurnID, Kind: model.HistoryPointTurn, OccurredAt: parseTime(envelope.Timestamp)})
			}
			if event.Type == "user_message" && result.Title == "" {
				result.Title = preview(event.Message)
			}
		}
	}
	if scanner.Err() != nil {
		partial = true
	}
	return result, partial
}
func (h historyReader) Read(ctx context.Context, selection ports.HistorySourceSelection) (ports.HistoryReadResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.HistoryReadResult{}, err
	}
	if selection.Provider != Name || selection.Native.Namespace != NativeNamespace {
		return ports.HistoryReadResult{}, fmt.Errorf("codex history selection has wrong provider")
	}
	token, err := decodeSourceToken(selection.SourceToken)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	if filepath.Clean(token.StateRoot) != h.provider.nativeHome || !pathWithin(token.StateRoot, token.Transcript) || token.SessionID != selection.Native.Reference {
		return ports.HistoryReadResult{}, fmt.Errorf("codex history source is outside provider storage")
	}
	if selection.SourceFingerprint != sourceFingerprint(token) {
		return ports.HistoryReadResult{}, fmt.Errorf("codex history source fingerprint changed")
	}
	raw, err := verifyHistorySelection(selection, token)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	turns, partial, err := parseRollout(raw)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	content := model.HistoryCoverageComplete
	if partial {
		content = model.HistoryCoveragePartial
	}
	ev, _ := model.NewProviderEvidence(Name, evidenceVersion, []byte(selection.SourceRevision))
	return ports.HistoryReadResult{Turns: turns, Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: content, SourceRevision: selection.SourceRevision, RefreshedAt: time.Now().UTC()}, Evidence: ev}, nil
}
func verifyHistorySelection(selection ports.HistorySourceSelection, token sourceToken) ([]byte, error) {
	raw, err := os.ReadFile(token.Transcript)
	if err != nil {
		return nil, err
	}
	if digest(raw) != selection.SourceRevision {
		return nil, fmt.Errorf("codex history source revision changed")
	}
	if selection.Point != nil {
		if selection.Point.Kind != model.HistoryPointTurn {
			return nil, ports.ErrHistoryUnsupported
		}
		head, _ := parseRolloutHead(raw)
		found := false
		for _, point := range head.Points {
			if point.Token == selection.Point.Token {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("codex history turn is not in selected source")
		}
	}
	return raw, nil
}
func parseRollout(raw []byte) ([]ports.HistoryTurn, bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var turns []ports.HistoryTurn
	partial := false
	currentTurn := ""
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
		if envelope.Type == "event_msg" {
			var event struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(envelope.Payload, &event) == nil && event.Type == "task_started" {
				currentTurn = event.TurnID
			}
			continue
		}
		if envelope.Type != "response_item" {
			continue
		}
		var item struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(envelope.Payload, &item) != nil {
			partial = true
			continue
		}
		if item.Type != "message" || (item.Role != "user" && item.Role != "assistant") {
			continue
		}
		parts := make([]ports.HistoryPart, 0, len(item.Content))
		for _, part := range item.Content {
			if part.Type == "input_text" || part.Type == "output_text" {
				parts = append(parts, ports.HistoryPart{Kind: ports.HistoryPartText, Text: part.Text})
			} else {
				parts = append(parts, ports.HistoryPart{Kind: ports.HistoryPartUnsupported, MediaType: part.Type, Omitted: true})
				partial = true
			}
		}
		if len(parts) == 0 {
			parts = append(parts, ports.HistoryPart{Kind: ports.HistoryPartUnsupported, MediaType: "application/json", Omitted: true})
			partial = true
		}
		turns = append(turns, ports.HistoryTurn{Point: ports.ProviderHistoryPoint{Token: currentTurn, Kind: model.HistoryPointTurn, OccurredAt: parseTime(envelope.Timestamp)}, Role: item.Role, Parts: parts})
	}
	return turns, partial, scanner.Err()
}
func decodeSourceToken(raw string) (sourceToken, error) {
	var token sourceToken
	if err := json.Unmarshal([]byte(raw), &token); err != nil || token.StateRoot == "" || token.SessionID == "" || token.Transcript == "" {
		return sourceToken{}, fmt.Errorf("invalid Codex source token")
	}
	return token, nil
}
func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func sourceFingerprint(token sourceToken) string {
	return digest([]byte(Name + "\x00" + NativeNamespace + "\x00" + filepath.Clean(token.StateRoot) + "\x00" + token.SessionID + "\x00" + filepath.Clean(token.Transcript)))
}
func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed.UTC()
}
func preview(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 80 {
		return value[:80]
	}
	return value
}

var _ ports.HistoryReader = historyReader{}
