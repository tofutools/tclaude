package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	copilotModelCatalogRefreshInterval = time.Hour
	copilotModelCatalogMaxAge          = 24 * time.Hour
	copilotModelCatalogRequestTimeout  = 30 * time.Second
	copilotModelCatalogMaxResponse     = 16 << 20
	copilotModelCatalogIntegrationID   = "copilot-developer-cli"
)

const copilotEndpointQuery = `query { viewer { copilotEndpoints { api } } }`

type copilotModelCatalogDeps struct {
	lookPath      func(string) (string, error)
	commandOutput func(context.Context, string, ...string) ([]byte, error)
	doRequest     func(*http.Request) (*http.Response, error)
	now           func() time.Time
}

var defaultCopilotModelCatalogDeps = copilotModelCatalogDeps{
	lookPath: exec.LookPath,
	commandOutput: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	},
	doRequest: (&http.Client{Timeout: copilotModelCatalogRequestTimeout}).Do,
	now:       time.Now,
}

type copilotModelCatalogRefreshOutcome struct {
	Missing    []string
	GHCLI      string
	CopilotCLI string
	GHAuth     string
	Endpoint   string
	Models     int
}

type copilotEndpointResponse struct {
	Data struct {
		Viewer struct {
			CopilotEndpoints struct {
				API string `json:"api"`
			} `json:"copilotEndpoints"`
		} `json:"viewer"`
	} `json:"data"`
}

type copilotRemoteModel struct {
	ID           string `json:"id"`
	Capabilities struct {
		Limits struct {
			MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
			MaxPromptTokens        int64 `json:"max_prompt_tokens"`
			MaxOutputTokens        int64 `json:"max_output_tokens"`
		} `json:"limits"`
	} `json:"capabilities"`
}

type copilotRemoteModelResponse struct {
	Data []json.RawMessage `json:"data"`
}

// startCopilotModelCatalogMirror refreshes immediately and hourly thereafter.
// Every failure leaves the last successfully mirrored catalog untouched.
func startCopilotModelCatalogMirror(stop <-chan struct{}) {
	go func() {
		refreshCopilotModelCatalogAndLog(context.Background(), "startup")
		ticker := time.NewTicker(copilotModelCatalogRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				refreshCopilotModelCatalogAndLog(context.Background(), "scheduled")
			}
		}
	}()
}

func refreshCopilotModelCatalogAndLog(parent context.Context, trigger string) {
	startedAt := time.Now()
	slog.Info("copilot-model-catalog: refresh started", "trigger", trigger)
	ctx, cancel := context.WithTimeout(parent, copilotModelCatalogRequestTimeout)
	defer cancel()
	outcome, err := refreshCopilotModelCatalog(ctx, defaultCopilotModelCatalogDeps)
	duration := time.Since(startedAt).Round(time.Millisecond)
	if err != nil {
		// Once both CLIs exist, missing credentials, rejected Copilot access,
		// routing failures and malformed replies are operational failures rather
		// than an optional integration being absent.
		slog.Error("copilot-model-catalog: refresh failed",
			"trigger", trigger, "duration", duration, "error", err,
			"gh_cli", outcome.GHCLI, "copilot_cli", outcome.CopilotCLI,
			"gh_auth", outcome.GHAuth)
		return
	}
	if len(outcome.Missing) > 0 {
		// Copilot is optional. A host that does not have the two command-line
		// prerequisites should say why the mirror is idle without warning.
		slog.Info("copilot-model-catalog: refresh skipped; required CLI not installed",
			"trigger", trigger, "duration", duration,
			"missing", strings.Join(outcome.Missing, ","),
			"gh_cli", outcome.GHCLI, "copilot_cli", outcome.CopilotCLI,
			"gh_auth", outcome.GHAuth)
		return
	}
	slog.Info("copilot-model-catalog: refresh complete",
		"trigger", trigger, "duration", duration,
		"models", outcome.Models, "endpoint", outcome.Endpoint,
		"gh_cli", outcome.GHCLI, "copilot_cli", outcome.CopilotCLI,
		"gh_auth", outcome.GHAuth)
}

// refreshCopilotModelCatalog performs one read-only remote refresh. gh owns
// credential and GitHub-host routing discovery; Copilot's presence is still a
// prerequisite because this catalog is only meaningful for a locally usable
// Copilot harness.
func refreshCopilotModelCatalog(ctx context.Context, deps copilotModelCatalogDeps) (copilotModelCatalogRefreshOutcome, error) {
	outcome := copilotModelCatalogRefreshOutcome{
		GHCLI: "unknown", CopilotCLI: "unknown", GHAuth: "not_checked",
	}
	paths := make(map[string]string, 2)
	for _, name := range []string{"copilot", "gh"} {
		path, err := deps.lookPath(name)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				outcome.Missing = append(outcome.Missing, name)
				if name == "gh" {
					outcome.GHCLI = "missing"
				} else {
					outcome.CopilotCLI = "missing"
				}
				continue
			}
			return outcome, fmt.Errorf("find %s CLI: %w", name, err)
		}
		paths[name] = path
		if name == "gh" {
			outcome.GHCLI = "installed"
		} else {
			outcome.CopilotCLI = "installed"
		}
	}
	if len(outcome.Missing) > 0 {
		return outcome, nil
	}

	tokenOutput, err := deps.commandOutput(ctx, paths["gh"], "auth", "token")
	if err != nil {
		outcome.GHAuth = "unavailable"
		// Never include output from a credential-producing command in logs.
		return outcome, fmt.Errorf("gh authentication unavailable: %w", err)
	}
	token := strings.TrimSpace(string(tokenOutput))
	if token == "" {
		outcome.GHAuth = "unavailable"
		return outcome, errors.New("gh authentication unavailable: gh auth token returned an empty token")
	}
	outcome.GHAuth = "authenticated"

	endpointOutput, err := deps.commandOutput(ctx, paths["gh"], "api", "graphql",
		"-f", "query="+copilotEndpointQuery)
	if err != nil {
		return outcome, fmt.Errorf("resolve Copilot API endpoint: %w%s", err, commandFailureDetail(endpointOutput))
	}
	var endpointResponse copilotEndpointResponse
	if err := json.Unmarshal(endpointOutput, &endpointResponse); err != nil {
		return outcome, fmt.Errorf("decode Copilot API endpoint: %w", err)
	}
	baseURL := strings.TrimSpace(endpointResponse.Data.Viewer.CopilotEndpoints.API)
	modelsURL, err := copilotModelsURL(baseURL)
	if err != nil {
		return outcome, err
	}
	outcome.Endpoint = baseURL

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return outcome, fmt.Errorf("create Copilot model catalog request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Copilot-Integration-Id", copilotModelCatalogIntegrationID)
	req.Header.Set("User-Agent", "tclaude-agentd")
	resp, err := deps.doRequest(req)
	if err != nil {
		return outcome, fmt.Errorf("fetch Copilot model catalog: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, copilotModelCatalogMaxResponse+1))
	if err != nil {
		return outcome, fmt.Errorf("read Copilot model catalog: %w", err)
	}
	if len(body) > copilotModelCatalogMaxResponse {
		return outcome, fmt.Errorf("read Copilot model catalog: response exceeds %d bytes", copilotModelCatalogMaxResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return outcome, fmt.Errorf("fetch Copilot model catalog: HTTP %s%s",
			resp.Status, responseFailureDetail(body))
	}

	var remote copilotRemoteModelResponse
	if err := json.Unmarshal(body, &remote); err != nil {
		return outcome, fmt.Errorf("decode Copilot model catalog: %w", err)
	}
	if len(remote.Data) == 0 {
		return outcome, errors.New("decode Copilot model catalog: response contains no models")
	}
	fetchedAt := deps.now().UTC()
	entries := make([]db.CopilotModelCatalogEntry, 0, len(remote.Data))
	for _, raw := range remote.Data {
		var model copilotRemoteModel
		if err := json.Unmarshal(raw, &model); err != nil {
			return outcome, fmt.Errorf("decode Copilot model catalog entry: %w", err)
		}
		entries = append(entries, db.CopilotModelCatalogEntry{
			ModelID:                model.ID,
			MaxContextWindowTokens: model.Capabilities.Limits.MaxContextWindowTokens,
			MaxPromptTokens:        model.Capabilities.Limits.MaxPromptTokens,
			MaxOutputTokens:        model.Capabilities.Limits.MaxOutputTokens,
			RawJSON:                string(raw),
		})
	}
	if err := db.ReplaceCopilotModelCatalog(entries, fetchedAt); err != nil {
		return outcome, err
	}
	outcome.Models = len(entries)
	return outcome, nil
}

// copilotCatalogContextWindow returns the remote catalog's prompt-token limit
// while the last successful mirror is fresh enough to trust. Lookup failures
// deliberately degrade silently to the static table: this sits on hot meter
// and dashboard paths, while refresh failures are already logged by the
// hourly background job.
func copilotCatalogContextWindow(model string) int64 {
	limit, fresh, err := db.FreshCopilotModelPromptLimit(
		strings.TrimSpace(model), time.Now(), copilotModelCatalogMaxAge)
	if err != nil || !fresh {
		return 0
	}
	return limit
}

func copilotModelsURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("invalid Copilot API endpoint: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid Copilot API endpoint %q", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/models"
	return u.String(), nil
}

func commandFailureDetail(output []byte) string {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return ""
	}
	if len(detail) > 512 {
		detail = detail[:512] + "…"
	}
	return ": " + detail
}

func responseFailureDetail(body []byte) string {
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return ""
	}
	if len(detail) > 512 {
		detail = detail[:512] + "…"
	}
	return ": " + detail
}
