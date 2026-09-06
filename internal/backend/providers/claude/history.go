package claude

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const claudeHistoryEvidenceVersion uint32 = 1

type historyReader struct{}

type historyEvidence struct {
	Path           string `json:"path"`
	NativeID       string `json:"native_id"`
	SourceRevision string `json:"source_revision"`
	Size           int64  `json:"size"`
	Fingerprint    string `json:"fingerprint"`
}

type claudeRecord struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	SessionID   string          `json:"sessionId"`
	CWD         string          `json:"cwd"`
	Timestamp   string          `json:"timestamp"`
	CustomTitle string          `json:"customTitle"`
	Summary     string          `json:"summary"`
	Message     json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (*Provider) History() ports.HistoryReader { return historyReader{} }

func (historyReader) Capabilities() ports.HistoryCapabilities {
	return ports.HistoryCapabilities{
		MetadataDiscovery: true, ContentRead: true,
		ContinuationPrecision: ports.HistoryPrecisionHead,
		ForkPrecision:         ports.HistoryPrecisionHead,
	}
}

func (historyReader) Discover(ctx context.Context, request ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	root, err := historyRoot(request.Scope.Source)
	if err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	refreshed := time.Now().UTC()
	result := ports.HistoryDiscoveryResult{Coverage: model.HistoryCoverage{
		Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, RefreshedAt: refreshed,
	}}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Coverage.Metadata = model.HistoryCoveragePartial
			result.Coverage.Content = model.HistoryCoveragePartial
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			return nil
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		if _, err := uuid.Parse(id); err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			result.Coverage.Metadata = model.HistoryCoveragePartial
			result.Coverage.Content = model.HistoryCoveragePartial
			return nil
		}
		if !request.Scope.ModifiedAfter.IsZero() && !info.ModTime().After(request.Scope.ModifiedAfter) {
			return nil
		}
		discovered, err := discoverClaudeFile(path, root, id, info, refreshed)
		if err != nil {
			result.Coverage.Metadata = model.HistoryCoveragePartial
			result.Coverage.Content = model.HistoryCoveragePartial
			return nil
		}
		if request.Scope.WorkspaceHint != "" && filepath.Clean(discovered.WorkspaceHint) != filepath.Clean(request.Scope.WorkspaceHint) {
			return nil
		}
		result.Histories = append(result.Histories, discovered)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return ports.HistoryDiscoveryResult{Coverage: model.HistoryCoverage{
			Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, RefreshedAt: refreshed,
		}}, nil
	}
	if err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	sort.Slice(result.Histories, func(i, j int) bool {
		return result.Histories[i].ModifiedAt.After(result.Histories[j].ModifiedAt)
	})
	return result, nil
}

func (historyReader) Read(ctx context.Context, selection ports.HistorySourceSelection) (ports.HistoryReadResult, error) {
	if selection.Provider != Name || selection.Native.Namespace != NativeNamespace {
		return ports.HistoryReadResult{}, ports.ErrHistoryUnsupported
	}
	if selection.Point != nil && selection.Point.Kind != model.HistoryPointHead {
		return ports.HistoryReadResult{}, ports.ErrHistoryUnsupported
	}
	evidence, err := decodeClaudeHistoryEvidence(selection.Evidence)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	if evidence.NativeID != selection.Native.Reference || evidence.Fingerprint != selection.SourceFingerprint {
		return ports.HistoryReadResult{}, fmt.Errorf("claude history evidence does not match native source")
	}
	revision, _, err := fingerprintFile(evidence.Path)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	if revision != evidence.SourceRevision || selection.SourceRevision != revision {
		return ports.HistoryReadResult{}, fmt.Errorf("claude history source revision changed")
	}
	file, err := os.Open(evidence.Path)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	defer file.Close()
	result := ports.HistoryReadResult{Coverage: model.HistoryCoverage{
		Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete,
		SourceRevision: revision, RefreshedAt: time.Now().UTC(),
	}, Evidence: selection.Evidence}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	line := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return ports.HistoryReadResult{}, err
		}
		line++
		var record claudeRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			result.Coverage.Content = model.HistoryCoveragePartial
			continue
		}
		turn, ok := normalizeClaudeTurn(record, line)
		if ok {
			result.Turns = append(result.Turns, turn)
		}
	}
	if err := scanner.Err(); err != nil {
		return ports.HistoryReadResult{}, err
	}
	return result, nil
}

func discoverClaudeFile(path, root, id string, info os.FileInfo, refreshed time.Time) (ports.DiscoveredHistory, error) {
	revision, size, err := fingerprintFile(path)
	if err != nil {
		return ports.DiscoveredHistory{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return ports.DiscoveredHistory{}, err
	}
	defer file.Close()
	var title, summary, firstPrompt, cwd string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var record claudeRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if record.SessionID != "" && record.SessionID != id {
			continue
		}
		if record.CWD != "" {
			cwd = record.CWD
		}
		if record.CustomTitle != "" {
			title = record.CustomTitle
		}
		if record.Summary != "" {
			summary = record.Summary
		}
		if firstPrompt == "" {
			if turn, ok := normalizeClaudeTurn(record, 0); ok && turn.Role == "user" {
				for _, part := range turn.Parts {
					if part.Kind == ports.HistoryPartText && part.Text != "" {
						firstPrompt = part.Text
						break
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ports.DiscoveredHistory{}, err
	}
	if title == "" {
		title = summary
	}
	if title == "" {
		title = firstPrompt
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ports.DiscoveredHistory{}, fmt.Errorf("claude history is outside configured root")
	}
	fingerprint := claudeSourceFingerprint(path, id)
	evidence, err := encodeClaudeHistoryEvidence(historyEvidence{Path: path, NativeID: id, SourceRevision: revision, Size: size, Fingerprint: fingerprint})
	if err != nil {
		return ports.DiscoveredHistory{}, err
	}
	return ports.DiscoveredHistory{
		Native:      model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: id, ObservedAt: refreshed},
		SourceToken: filepath.ToSlash(relative), SourceFingerprint: fingerprint, Title: title, WorkspaceHint: cwd,
		ModifiedAt: info.ModTime().UTC(), Availability: model.HistoryContent,
		Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, SourceRevision: revision, RefreshedAt: refreshed},
		Points:   []ports.ProviderHistoryPoint{{Token: "head", Kind: model.HistoryPointHead, OccurredAt: info.ModTime().UTC()}},
		Evidence: evidence,
	}, nil
}

func normalizeClaudeTurn(record claudeRecord, line int) (ports.HistoryTurn, bool) {
	if record.Type != "user" && record.Type != "assistant" || len(record.Message) == 0 {
		return ports.HistoryTurn{}, false
	}
	var message claudeMessage
	if json.Unmarshal(record.Message, &message) != nil {
		return ports.HistoryTurn{}, false
	}
	role := message.Role
	if role == "" {
		role = record.Type
	}
	point := record.UUID
	if point == "" {
		point = fmt.Sprintf("line:%d", line)
	}
	turn := ports.HistoryTurn{Role: role, Point: ports.ProviderHistoryPoint{
		Token: point, Kind: model.HistoryPointMessage, OccurredAt: parseClaudeTime(record.Timestamp),
	}}
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		turn.Parts = append(turn.Parts, ports.HistoryPart{Kind: ports.HistoryPartText, Text: text})
		return turn, true
	}
	var blocks []claudeContentBlock
	if json.Unmarshal(message.Content, &blocks) != nil {
		turn.Parts = append(turn.Parts, ports.HistoryPart{Kind: ports.HistoryPartUnsupported, MediaType: "application/json", Omitted: true})
		return turn, true
	}
	for _, block := range blocks {
		if block.Type == "text" {
			turn.Parts = append(turn.Parts, ports.HistoryPart{Kind: ports.HistoryPartText, Text: block.Text})
		} else {
			turn.Parts = append(turn.Parts, ports.HistoryPart{Kind: ports.HistoryPartUnsupported, MediaType: "application/vnd.claude." + block.Type, Omitted: true})
		}
	}
	return turn, true
}

func historyRoot(value string) (string, error) {
	root, err := filepath.Abs(strings.TrimSpace(value))
	if err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("claude history root is required")
	}
	return filepath.Clean(root), nil
}

func fingerprintFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

func claudeSourceFingerprint(path, nativeID string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(path) + "\x00" + nativeID))
	return hex.EncodeToString(digest[:])
}

func encodeClaudeHistoryEvidence(value historyEvidence) (model.ProviderEvidence, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return model.ProviderEvidence{}, err
	}
	return model.NewProviderEvidence(Name, claudeHistoryEvidenceVersion, payload)
}

func decodeClaudeHistoryEvidence(envelope model.ProviderEvidence) (historyEvidence, error) {
	if envelope.Provider != Name || envelope.Version != claudeHistoryEvidenceVersion {
		return historyEvidence{}, fmt.Errorf("unsupported Claude history evidence")
	}
	var value historyEvidence
	if err := json.Unmarshal(envelope.Payload, &value); err != nil {
		return historyEvidence{}, err
	}
	if value.Path == "" || value.NativeID == "" || value.SourceRevision == "" || value.Fingerprint == "" {
		return historyEvidence{}, fmt.Errorf("incomplete Claude history evidence")
	}
	return value, nil
}

func parseClaudeTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

var _ ports.HistoryProvider = (*Provider)(nil)
var _ ports.HistoryReader = historyReader{}
