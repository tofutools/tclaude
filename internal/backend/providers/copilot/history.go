package copilot

import (
	"bufio"
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
	"gopkg.in/yaml.v3"
)

type historyReader struct{ provider *Provider }
type sourceToken struct {
	StateRoot string `json:"state_root"`
	SessionID string `json:"session_id"`
}
type workspaceMetadata struct {
	ID        string `yaml:"id"`
	CWD       string `yaml:"cwd"`
	Name      string `yaml:"name"`
	CreatedAt string `yaml:"created_at"`
	UpdatedAt string `yaml:"updated_at"`
}

func (historyReader) Capabilities() ports.HistoryCapabilities {
	return ports.HistoryCapabilities{MetadataDiscovery: true, ContentRead: true, ContinuationPrecision: ports.HistoryPrecisionHead, ForkPrecision: ports.HistoryPrecisionNone}
}
func (h historyReader) Discover(ctx context.Context, request ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	root := h.provider.nativeHome
	entries, err := os.ReadDir(filepath.Join(root, "session-state"))
	if errors.Is(err, fs.ErrNotExist) {
		return ports.HistoryDiscoveryResult{Histories: []ports.DiscoveredHistory{}, Coverage: partialCoverage("")}, nil
	}
	if err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	var found []ports.DiscoveredHistory
	partial := false
	for _, state := range entries {
		if !state.IsDir() {
			continue
		}
		item, itemErr := discoverSession(root, state.Name(), request.Scope)
		if itemErr != nil {
			partial = true
			continue
		}
		if item != nil {
			found = append(found, *item)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ModifiedAt.After(found[j].ModifiedAt) })
	coverage := model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoveragePartial, RefreshedAt: time.Now().UTC()}
	if partial {
		coverage.Metadata = model.HistoryCoveragePartial
	}
	return ports.HistoryDiscoveryResult{Histories: found, Coverage: coverage}, nil
}
func discoverSession(stateRoot, id string, scope ports.HistoryDiscoveryScope) (*ports.DiscoveredHistory, error) {
	dir := filepath.Join(stateRoot, "session-state", id)
	metaRaw, err := os.ReadFile(filepath.Join(dir, "workspace.yaml"))
	if err != nil {
		return nil, err
	}
	var meta workspaceMetadata
	if err := yaml.Unmarshal(metaRaw, &meta); err != nil || meta.CWD == "" {
		return nil, fmt.Errorf("invalid Copilot workspace metadata")
	}
	updated := parseTime(meta.UpdatedAt)
	if !scope.ModifiedAfter.IsZero() && !updated.After(scope.ModifiedAfter) {
		return nil, nil
	}
	if scope.WorkspaceHint != "" && filepath.Clean(scope.WorkspaceHint) != filepath.Clean(meta.CWD) {
		return nil, nil
	}
	events, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	revision := digest(events)
	tokenRaw, _ := json.Marshal(sourceToken{StateRoot: stateRoot, SessionID: id})
	point := ports.ProviderHistoryPoint{Token: revision, Kind: model.HistoryPointHead, OccurredAt: updated}
	evidence, _ := model.NewProviderEvidence(Name, evidenceVersion, tokenRaw)
	token := sourceToken{StateRoot: stateRoot, SessionID: id}
	return &ports.DiscoveredHistory{Native: model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: id, ObservedAt: time.Now().UTC()}, SourceToken: string(tokenRaw), SourceFingerprint: sourceFingerprint(token), Title: meta.Name, WorkspaceHint: meta.CWD, ModifiedAt: updated, Availability: model.HistoryContent, Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, SourceRevision: revision, RefreshedAt: time.Now().UTC()}, Points: []ports.ProviderHistoryPoint{point}, Evidence: evidence}, nil
}
func (h historyReader) Read(ctx context.Context, selection ports.HistorySourceSelection) (ports.HistoryReadResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.HistoryReadResult{}, err
	}
	if selection.Provider != Name || selection.Native.Namespace != NativeNamespace {
		return ports.HistoryReadResult{}, fmt.Errorf("copilot history selection has wrong provider")
	}
	token, err := decodeSourceToken(selection.SourceToken)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	if filepath.Clean(token.StateRoot) != h.provider.nativeHome || token.SessionID != selection.Native.Reference {
		return ports.HistoryReadResult{}, fmt.Errorf("copilot history source is outside provider storage")
	}
	if selection.SourceFingerprint != sourceFingerprint(token) {
		return ports.HistoryReadResult{}, fmt.Errorf("copilot history source fingerprint changed")
	}
	if err := verifyHistorySelection(selection, token); err != nil {
		return ports.HistoryReadResult{}, err
	}
	raw, err := os.ReadFile(filepath.Join(token.StateRoot, "session-state", token.SessionID, "events.jsonl"))
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	turns, partial, err := parseEvents(raw)
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
func verifyHistorySelection(selection ports.HistorySourceSelection, token sourceToken) error {
	raw, err := os.ReadFile(filepath.Join(token.StateRoot, "session-state", token.SessionID, "events.jsonl"))
	if err != nil {
		return err
	}
	if digest(raw) != selection.SourceRevision {
		return fmt.Errorf("copilot history source revision changed")
	}
	if selection.Point != nil && selection.Point.Kind != model.HistoryPointHead {
		return ports.ErrHistoryUnsupported
	}
	return nil
}
func parseEvents(raw []byte) ([]ports.HistoryTurn, bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var turns []ports.HistoryTurn
	partial := false
	for scanner.Scan() {
		var event struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Data      struct {
				Content      json.RawMessage `json:"content"`
				Attachments  json.RawMessage `json:"attachments"`
				ToolRequests json.RawMessage `json:"toolRequests"`
			} `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			partial = true
			continue
		}
		role := ""
		switch event.Type {
		case "user.message":
			role = "user"
		case "assistant.message":
			role = "assistant"
		default:
			continue
		}
		var text string
		parts := []ports.HistoryPart{}
		if json.Unmarshal(event.Data.Content, &text) == nil {
			parts = append(parts, ports.HistoryPart{Kind: ports.HistoryPartText, Text: text})
		} else {
			parts = append(parts, ports.HistoryPart{Kind: ports.HistoryPartUnsupported, MediaType: "application/json", Omitted: true})
			partial = true
		}
		if nonemptyJSON(event.Data.Attachments) || nonemptyJSON(event.Data.ToolRequests) {
			parts = append(parts, ports.HistoryPart{Kind: ports.HistoryPartUnsupported, MediaType: "application/json", Omitted: true})
			partial = true
		}
		at := parseTime(event.Timestamp)
		turns = append(turns, ports.HistoryTurn{Point: ports.ProviderHistoryPoint{Token: event.ID, Kind: model.HistoryPointMessage, OccurredAt: at}, Role: role, Parts: parts})
	}
	return turns, partial, scanner.Err()
}
func nonemptyJSON(raw json.RawMessage) bool {
	trim := strings.TrimSpace(string(raw))
	return trim != "" && trim != "null" && trim != "[]" && trim != "{}"
}
func decodeSourceToken(raw string) (sourceToken, error) {
	var token sourceToken
	if err := json.Unmarshal([]byte(raw), &token); err != nil || token.StateRoot == "" || token.SessionID == "" {
		return sourceToken{}, fmt.Errorf("invalid Copilot source token")
	}
	return token, nil
}
func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func sourceFingerprint(token sourceToken) string {
	return digest([]byte(Name + "\x00" + NativeNamespace + "\x00" + filepath.Clean(token.StateRoot) + "\x00" + token.SessionID))
}
func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed.UTC()
}
func partialCoverage(revision string) model.HistoryCoverage {
	return model.HistoryCoverage{Metadata: model.HistoryCoveragePartial, Content: model.HistoryCoveragePartial, SourceRevision: revision, RefreshedAt: time.Now().UTC()}
}

var _ ports.HistoryReader = historyReader{}
