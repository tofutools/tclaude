package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	opencodeHistoryEvidenceVersion uint32 = 1
	historyManifestName                   = "tclaude-history.json"
)

type historyReader struct{ provider *Provider }

type historyManifest struct {
	NativeID string    `json:"native_id"`
	ParentID string    `json:"parent_id,omitempty"`
	CWD      string    `json:"cwd"`
	Updated  time.Time `json:"updated"`
}

type historyEvidence struct {
	StateRoot      string `json:"state_root"`
	NativeID       string `json:"native_id"`
	SourceRevision string `json:"source_revision"`
	Fingerprint    string `json:"fingerprint"`
}

type exportedHistory struct {
	Info struct {
		ID        string `json:"id"`
		ParentID  string `json:"parentID"`
		Directory string `json:"directory"`
		Title     string `json:"title"`
		Time      struct {
			Created int64 `json:"created"`
			Updated int64 `json:"updated"`
		} `json:"time"`
	} `json:"info"`
	Messages []struct {
		Info struct {
			ID   string `json:"id"`
			Role string `json:"role"`
			Time struct {
				Created int64 `json:"created"`
			} `json:"time"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"messages"`
}

type listedSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Updated   int64  `json:"time_updated"`
}

func (p *Provider) History() ports.HistoryReader { return historyReader{provider: p} }

func (historyReader) Capabilities() ports.HistoryCapabilities {
	return ports.HistoryCapabilities{
		MetadataDiscovery: true, ContentRead: true,
		ContinuationPrecision: ports.HistoryPrecisionHead,
		ForkPrecision:         ports.HistoryPrecisionBeforeMessage,
		ForkRequiresExclusive: true,
	}
}

func (r historyReader) Discover(ctx context.Context, request ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	if strings.TrimSpace(request.Scope.Source) != "" {
		return r.discoverNativeRoot(ctx, request)
	}
	root, err := r.configuredRoot(request.Scope.Source)
	if err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	refreshed := time.Now().UTC()
	result := ports.HistoryDiscoveryResult{Coverage: model.HistoryCoverage{
		Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, RefreshedAt: refreshed,
	}}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ports.HistoryDiscoveryResult{}, err
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "execution-") {
			continue
		}
		stateRoot := filepath.Join(root, entry.Name())
		manifest, err := readHistoryManifest(stateRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result.Coverage.Metadata = model.HistoryCoveragePartial
			result.Coverage.Content = model.HistoryCoveragePartial
			continue
		}
		if request.Scope.WorkspaceHint != "" && filepath.Clean(request.Scope.WorkspaceHint) != filepath.Clean(manifest.CWD) {
			continue
		}
		exported, raw, revision, err := r.export(ctx, stateRoot, manifest.NativeID, manifest.CWD)
		if err != nil {
			result.Histories = append(result.Histories, ports.DiscoveredHistory{
				Native:      model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: manifest.NativeID, ObservedAt: refreshed},
				SourceToken: entry.Name(), SourceFingerprint: historySourceFingerprint(stateRoot, manifest.NativeID),
				WorkspaceHint: manifest.CWD, ModifiedAt: manifest.Updated,
				Availability: model.HistoryUnknown,
				Coverage:     model.HistoryCoverage{Metadata: model.HistoryCoveragePartial, Content: model.HistoryCoverageUnknown, RefreshedAt: refreshed},
			})
			result.Coverage.Metadata = model.HistoryCoveragePartial
			result.Coverage.Content = model.HistoryCoveragePartial
			continue
		}
		_ = raw
		if !request.Scope.ModifiedAfter.IsZero() && !milliseconds(exported.Info.Time.Updated).After(request.Scope.ModifiedAfter) {
			continue
		}
		fingerprint := historySourceFingerprint(stateRoot, manifest.NativeID)
		evidence, err := encodeHistoryEvidence(historyEvidence{StateRoot: stateRoot, NativeID: manifest.NativeID, SourceRevision: revision, Fingerprint: fingerprint})
		if err != nil {
			return ports.HistoryDiscoveryResult{}, err
		}
		points := make([]ports.ProviderHistoryPoint, 0, len(exported.Messages)+1)
		for _, message := range exported.Messages {
			points = append(points, ports.ProviderHistoryPoint{Token: message.Info.ID, Kind: model.HistoryPointBeforeMessage, OccurredAt: milliseconds(message.Info.Time.Created)})
		}
		updated := milliseconds(exported.Info.Time.Updated)
		points = append(points, ports.ProviderHistoryPoint{Token: "head", Kind: model.HistoryPointHead, OccurredAt: updated})
		result.Histories = append(result.Histories, ports.DiscoveredHistory{
			Native:      model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: manifest.NativeID, ObservedAt: refreshed},
			SourceToken: entry.Name(), SourceFingerprint: fingerprint, Title: exported.Info.Title, WorkspaceHint: exported.Info.Directory,
			ModifiedAt: updated, Availability: model.HistoryContent, Points: points, Evidence: evidence,
			Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, SourceRevision: revision, RefreshedAt: refreshed},
		})
	}
	sort.Slice(result.Histories, func(i, j int) bool { return result.Histories[i].ModifiedAt.After(result.Histories[j].ModifiedAt) })
	return result, nil
}

// discoverNativeRoot reads an ordinary composition-selected OpenCode XDG
// state root through the official CLI. It needs no platform manifest and does
// not copy configuration or account state into provider-owned storage.
func (r historyReader) discoverNativeRoot(ctx context.Context, request ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	root, err := r.configuredRoot(request.Scope.Source)
	if err != nil {
		return ports.HistoryDiscoveryResult{}, err
	}
	cmd := exec.CommandContext(ctx, r.provider.executable, "db",
		"SELECT id,title,directory,time_updated FROM session WHERE parent_id IS NULL ORDER BY time_updated DESC",
		"--format", "json", "--pure")
	cmd.Dir = root
	cmd.Env = host.MergeEnvironment(os.Environ(), r.provider.runtimeEnvironment(root))
	raw, err := cmd.Output()
	if err != nil {
		return ports.HistoryDiscoveryResult{}, fmt.Errorf("list OpenCode history: %w", err)
	}
	var sessions []listedSession
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return ports.HistoryDiscoveryResult{}, fmt.Errorf("decode OpenCode history list: %w", err)
	}
	refreshed := time.Now().UTC()
	result := ports.HistoryDiscoveryResult{Coverage: model.HistoryCoverage{
		Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, RefreshedAt: refreshed,
	}}
	for _, session := range sessions {
		if !strings.HasPrefix(session.ID, "ses_") || !filepath.IsAbs(session.Directory) {
			result.Coverage.Metadata = model.HistoryCoveragePartial
			continue
		}
		if request.Scope.WorkspaceHint != "" && filepath.Clean(request.Scope.WorkspaceHint) != filepath.Clean(session.Directory) {
			continue
		}
		exported, _, revision, exportErr := r.export(ctx, root, session.ID, session.Directory)
		if exportErr != nil {
			result.Coverage.Content = model.HistoryCoveragePartial
			result.Histories = append(result.Histories, ports.DiscoveredHistory{
				Native:      model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: session.ID, ObservedAt: refreshed},
				SourceToken: session.ID, SourceFingerprint: historySourceFingerprint(root, session.ID),
				Title: session.Title, WorkspaceHint: session.Directory, ModifiedAt: milliseconds(session.Updated),
				Availability: model.HistoryUnknown,
				Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete,
					Content: model.HistoryCoverageUnknown, RefreshedAt: refreshed},
			})
			continue
		}
		updated := milliseconds(exported.Info.Time.Updated)
		if !request.Scope.ModifiedAfter.IsZero() && !updated.After(request.Scope.ModifiedAfter) {
			continue
		}
		fingerprint := historySourceFingerprint(root, session.ID)
		evidence, encodeErr := encodeHistoryEvidence(historyEvidence{StateRoot: root, NativeID: session.ID, SourceRevision: revision, Fingerprint: fingerprint})
		if encodeErr != nil {
			return ports.HistoryDiscoveryResult{}, encodeErr
		}
		points := make([]ports.ProviderHistoryPoint, 0, len(exported.Messages)+1)
		for _, message := range exported.Messages {
			points = append(points, ports.ProviderHistoryPoint{Token: message.Info.ID, Kind: model.HistoryPointBeforeMessage, OccurredAt: milliseconds(message.Info.Time.Created)})
		}
		points = append(points, ports.ProviderHistoryPoint{Token: "head", Kind: model.HistoryPointHead, OccurredAt: updated})
		result.Histories = append(result.Histories, ports.DiscoveredHistory{
			Native:      model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: session.ID, ObservedAt: refreshed},
			SourceToken: session.ID, SourceFingerprint: fingerprint, Title: exported.Info.Title, WorkspaceHint: exported.Info.Directory,
			ModifiedAt: updated, Availability: model.HistoryContent, Points: points, Evidence: evidence,
			Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete,
				SourceRevision: revision, RefreshedAt: refreshed},
		})
	}
	sort.Slice(result.Histories, func(i, j int) bool { return result.Histories[i].ModifiedAt.After(result.Histories[j].ModifiedAt) })
	return result, nil
}

func (r historyReader) Read(ctx context.Context, selection ports.HistorySourceSelection) (ports.HistoryReadResult, error) {
	exported, raw, revision, evidence, err := r.readSelection(ctx, selection)
	if err != nil {
		return ports.HistoryReadResult{}, err
	}
	_ = raw
	result := ports.HistoryReadResult{Evidence: selection.Evidence, Coverage: model.HistoryCoverage{
		Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete,
		SourceRevision: revision, RefreshedAt: time.Now().UTC(),
	}}
	for _, message := range exported.Messages {
		// OpenCode's native fork messageID is an exclusive boundary: the
		// selected message is the first message omitted from the fork.
		if selection.Point != nil && selection.Point.Kind == model.HistoryPointBeforeMessage && selection.Point.Token == message.Info.ID {
			return result, nil
		}
		turn := ports.HistoryTurn{Role: message.Info.Role, Point: ports.ProviderHistoryPoint{
			Token: message.Info.ID, Kind: model.HistoryPointMessage, OccurredAt: milliseconds(message.Info.Time.Created),
		}}
		for _, part := range message.Parts {
			if part.Type == "text" {
				turn.Parts = append(turn.Parts, ports.HistoryPart{Kind: ports.HistoryPartText, Text: part.Text})
			} else {
				turn.Parts = append(turn.Parts, ports.HistoryPart{Kind: ports.HistoryPartUnsupported,
					MediaType: "application/vnd.opencode." + part.Type, Omitted: true})
			}
		}
		result.Turns = append(result.Turns, turn)
	}
	if selection.Point != nil && selection.Point.Kind == model.HistoryPointBeforeMessage {
		return ports.HistoryReadResult{}, fmt.Errorf("OpenCode history point is absent from selected source")
	}
	_ = evidence
	return result, nil
}

func (r historyReader) readSelection(ctx context.Context, selection ports.HistorySourceSelection) (exportedHistory, []byte, string, historyEvidence, error) {
	if selection.Provider != Name || selection.Native.Namespace != NativeNamespace {
		return exportedHistory{}, nil, "", historyEvidence{}, ports.ErrHistoryUnsupported
	}
	evidence, err := decodeHistoryEvidence(selection.Evidence)
	if err != nil {
		return exportedHistory{}, nil, "", historyEvidence{}, err
	}
	if evidence.NativeID != selection.Native.Reference || evidence.Fingerprint != selection.SourceFingerprint {
		return exportedHistory{}, nil, "", historyEvidence{}, fmt.Errorf("OpenCode history evidence does not match selected source")
	}
	cwd := ""
	if manifest, manifestErr := readHistoryManifest(evidence.StateRoot); manifestErr == nil && manifest.NativeID == evidence.NativeID {
		cwd = manifest.CWD
	} else {
		// Ordinary configured roots have no platform manifest. Resolve cwd from
		// the official export and then re-run the strict binding check below.
		cmd := exec.CommandContext(ctx, r.provider.executable, "export", evidence.NativeID, "--pure")
		cmd.Dir = evidence.StateRoot
		cmd.Env = host.MergeEnvironment(os.Environ(), r.provider.runtimeEnvironment(evidence.StateRoot))
		raw, outputErr := cmd.Output()
		if outputErr != nil {
			return exportedHistory{}, nil, "", historyEvidence{}, fmt.Errorf("export OpenCode history: %w", outputErr)
		}
		var probe exportedHistory
		if json.Unmarshal(raw, &probe) != nil || probe.Info.ID != evidence.NativeID || !filepath.IsAbs(probe.Info.Directory) {
			return exportedHistory{}, nil, "", historyEvidence{}, fmt.Errorf("OpenCode history source is unavailable")
		}
		cwd = probe.Info.Directory
	}
	exported, raw, revision, err := r.export(ctx, evidence.StateRoot, evidence.NativeID, cwd)
	if err != nil {
		return exportedHistory{}, nil, "", historyEvidence{}, err
	}
	if revision != evidence.SourceRevision || revision != selection.SourceRevision {
		return exportedHistory{}, nil, "", historyEvidence{}, fmt.Errorf("OpenCode history source revision changed")
	}
	return exported, raw, revision, evidence, nil
}

func (r historyReader) export(ctx context.Context, stateRoot, nativeID, cwd string) (exportedHistory, []byte, string, error) {
	root, rootErr := filepath.Abs(stateRoot)
	info, statErr := os.Stat(root)
	if rootErr != nil || statErr != nil || !info.IsDir() || root == string(filepath.Separator) || !strings.HasPrefix(nativeID, "ses_") {
		return exportedHistory{}, nil, "", fmt.Errorf("invalid OpenCode history source")
	}
	cmd := exec.CommandContext(ctx, r.provider.executable, "export", nativeID, "--pure")
	cmd.Dir = stateRoot
	cmd.Env = host.MergeEnvironment(os.Environ(), r.provider.runtimeEnvironment(stateRoot))
	raw, err := cmd.Output()
	if err != nil {
		return exportedHistory{}, nil, "", fmt.Errorf("export OpenCode history: %w", err)
	}
	var exported exportedHistory
	if err := json.Unmarshal(raw, &exported); err != nil {
		return exportedHistory{}, nil, "", fmt.Errorf("decode OpenCode history export: %w", err)
	}
	if exported.Info.ID != nativeID || filepath.Clean(exported.Info.Directory) != filepath.Clean(cwd) {
		return exportedHistory{}, nil, "", fmt.Errorf("OpenCode export does not match history manifest")
	}
	digest := sha256.Sum256(raw)
	return exported, raw, hex.EncodeToString(digest[:]), nil
}

func (r historyReader) configuredRoot(source string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return r.provider.privateRoot, nil
	}
	root, err := filepath.Abs(source)
	if err != nil || root == string(filepath.Separator) {
		return "", fmt.Errorf("OpenCode history root is invalid")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("OpenCode history root is unavailable")
	}
	return filepath.Clean(root), nil
}

func writeHistoryManifest(stateRoot string, manifest historyManifest) error {
	if !strings.HasPrefix(manifest.NativeID, "ses_") || !filepath.IsAbs(manifest.CWD) {
		return fmt.Errorf("invalid OpenCode history manifest")
	}
	manifest.Updated = time.Now().UTC()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(stateRoot, ".history-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(stateRoot, historyManifestName))
}

func readHistoryManifest(stateRoot string) (historyManifest, error) {
	value, err := os.ReadFile(filepath.Join(stateRoot, historyManifestName))
	if err != nil {
		return historyManifest{}, err
	}
	var manifest historyManifest
	if err := json.Unmarshal(value, &manifest); err != nil {
		return historyManifest{}, err
	}
	if !strings.HasPrefix(manifest.NativeID, "ses_") || !filepath.IsAbs(manifest.CWD) {
		return historyManifest{}, fmt.Errorf("invalid OpenCode history manifest")
	}
	return manifest, nil
}

func encodeHistoryEvidence(value historyEvidence) (model.ProviderEvidence, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return model.ProviderEvidence{}, err
	}
	return model.NewProviderEvidence(Name, opencodeHistoryEvidenceVersion, payload)
}

func decodeHistoryEvidence(envelope model.ProviderEvidence) (historyEvidence, error) {
	if envelope.Provider != Name || envelope.Version != opencodeHistoryEvidenceVersion {
		return historyEvidence{}, fmt.Errorf("unsupported OpenCode history evidence")
	}
	var value historyEvidence
	if err := json.Unmarshal(envelope.Payload, &value); err != nil {
		return historyEvidence{}, err
	}
	if value.StateRoot == "" || value.NativeID == "" || value.SourceRevision == "" || value.Fingerprint == "" {
		return historyEvidence{}, fmt.Errorf("incomplete OpenCode history evidence")
	}
	return value, nil
}

func historySourceFingerprint(stateRoot, nativeID string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(stateRoot) + "\x00" + nativeID))
	return hex.EncodeToString(digest[:])
}

func milliseconds(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

var _ ports.HistoryProvider = (*Provider)(nil)
var _ ports.HistoryReader = historyReader{}
