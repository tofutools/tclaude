package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

const (
	copilotModelCatalogRefreshInterval = time.Hour
	copilotModelCatalogMaxAge          = 24 * time.Hour
	copilotModelCatalogRequestTimeout  = 30 * time.Second
	copilotModelCatalogMaxResponse     = 16 << 20
	copilotModelCatalogIntegrationID   = "copilot-developer-cli"
	copilotModelCatalogProcessGrace    = 500 * time.Millisecond
)

const copilotEndpointQuery = `query { viewer { copilotEndpoints { api } } }`

type copilotModelCatalogDeps struct {
	lookPath      func(string) (string, error)
	commandOutput func(context.Context, string, ...string) ([]byte, error)
	doRequest     func(*http.Request) (*http.Response, error)
	fetchEnriched func(context.Context, string, string) ([]json.RawMessage, error)
	now           func() time.Time
}

var defaultCopilotModelCatalogDeps = copilotModelCatalogDeps{
	lookPath: exec.LookPath,
	commandOutput: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	},
	doRequest:     (&http.Client{Timeout: copilotModelCatalogRequestTimeout}).Do,
	fetchEnriched: fetchCopilotEnrichedModels,
	now:           time.Now,
}

type copilotModelCatalogRefreshOutcome struct {
	Missing        []string
	GHCLI          string
	CopilotCLI     string
	GHAuth         string
	Endpoint       string
	Models         int
	RemoteModels   int
	EnrichedModels int
	EnrichedStatus string
	enrichedErr    error
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

type copilotEnrichedModel struct {
	ID           string `json:"id"`
	Capabilities struct {
		Limits struct {
			MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
			MaxPromptTokens        int64 `json:"max_prompt_tokens"`
			MaxOutputTokens        int64 `json:"max_output_tokens"`
		} `json:"limits"`
	} `json:"capabilities"`
	Billing struct {
		TokenPrices struct {
			MaxPromptTokens int64 `json:"maxPromptTokens"`
			ContextMax      int64 `json:"contextMax"`
			LongContext     struct {
				MaxPromptTokens int64 `json:"maxPromptTokens"`
				ContextMax      int64 `json:"contextMax"`
			} `json:"longContext"`
		} `json:"tokenPrices"`
	} `json:"billing"`
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
	if outcome.enrichedErr != nil {
		slog.Warn("copilot-model-catalog: enriched model fetch failed; using remote catalog",
			"trigger", trigger, "duration", duration, "error", outcome.enrichedErr,
			"gh_cli", outcome.GHCLI, "copilot_cli", outcome.CopilotCLI,
			"gh_auth", outcome.GHAuth)
	}
	slog.Info("copilot-model-catalog: refresh complete",
		"trigger", trigger, "duration", duration,
		"models", outcome.Models, "remote_models", outcome.RemoteModels,
		"enriched_models", outcome.EnrichedModels, "enriched", outcome.EnrichedStatus,
		"endpoint", outcome.Endpoint,
		"gh_cli", outcome.GHCLI, "copilot_cli", outcome.CopilotCLI,
		"gh_auth", outcome.GHAuth)
}

// refreshCopilotModelCatalog performs one read-only refresh. The remote
// endpoint supplies broad model coverage; the installed CLI's models.list RPC
// adds account-aware billing/context tiers where available. gh owns credential
// and GitHub-host routing discovery, and Copilot's presence remains a
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
	outcome.RemoteModels = len(remote.Data)
	var enriched []json.RawMessage
	if deps.fetchEnriched == nil {
		outcome.EnrichedStatus = "not_configured"
	} else {
		enriched, err = deps.fetchEnriched(ctx, paths["copilot"], token)
		if err != nil {
			outcome.EnrichedStatus = "failed"
			outcome.enrichedErr = err
			enriched = nil
		} else {
			outcome.EnrichedStatus = "ok"
			outcome.EnrichedModels = len(enriched)
		}
	}
	fetchedAt := deps.now().UTC()
	entries, err := mergeCopilotModelCatalog(remote.Data, enriched)
	if err != nil && len(enriched) > 0 {
		// The remote response is still useful when the newer/experimental CLI
		// surface changes shape. Retry the merge without enrichment so an
		// optional source cannot prevent the established mirror from advancing.
		outcome.EnrichedStatus = "failed"
		outcome.EnrichedModels = 0
		outcome.enrichedErr = err
		entries, err = mergeCopilotModelCatalog(remote.Data, nil)
	}
	if err != nil {
		return outcome, err
	}
	if err := db.ReplaceCopilotModelCatalog(entries, fetchedAt); err != nil {
		return outcome, err
	}
	outcome.Models = len(entries)
	return outcome, nil
}

func mergeCopilotModelCatalog(remote, enriched []json.RawMessage) ([]db.CopilotModelCatalogEntry, error) {
	entries := make(map[string]*db.CopilotModelCatalogEntry, len(remote)+len(enriched))
	order := make([]string, 0, len(remote)+len(enriched))
	for _, raw := range remote {
		var model copilotRemoteModel
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, fmt.Errorf("decode Copilot remote model catalog entry: %w", err)
		}
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			return nil, errors.New("decode Copilot remote model catalog entry: empty model id")
		}
		if _, exists := entries[model.ID]; exists {
			return nil, fmt.Errorf("decode Copilot remote model catalog: duplicate model id %q", model.ID)
		}
		entries[model.ID] = &db.CopilotModelCatalogEntry{
			ModelID:                model.ID,
			MaxContextWindowTokens: model.Capabilities.Limits.MaxContextWindowTokens,
			MaxPromptTokens:        model.Capabilities.Limits.MaxPromptTokens,
			MaxOutputTokens:        model.Capabilities.Limits.MaxOutputTokens,
			RawJSON:                string(raw),
		}
		order = append(order, model.ID)
	}
	for _, raw := range enriched {
		var model copilotEnrichedModel
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, fmt.Errorf("decode Copilot enriched model catalog entry: %w", err)
		}
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			return nil, errors.New("decode Copilot enriched model catalog entry: empty model id")
		}
		entry := entries[model.ID]
		if entry == nil {
			entry = &db.CopilotModelCatalogEntry{ModelID: model.ID}
			entries[model.ID] = entry
			order = append(order, model.ID)
		}
		limits := model.Capabilities.Limits
		if entry.MaxContextWindowTokens == 0 {
			entry.MaxContextWindowTokens = limits.MaxContextWindowTokens
		}
		if entry.MaxOutputTokens == 0 {
			entry.MaxOutputTokens = limits.MaxOutputTokens
		}
		defaultPrompt := model.Billing.TokenPrices.MaxPromptTokens
		if defaultPrompt == 0 {
			defaultPrompt = model.Billing.TokenPrices.ContextMax
		}
		if defaultPrompt == 0 {
			defaultPrompt = limits.MaxPromptTokens
		}
		if defaultPrompt > 0 {
			entry.MaxPromptTokens = defaultPrompt
		}
		longPrompt := model.Billing.TokenPrices.LongContext.MaxPromptTokens
		if longPrompt == 0 {
			longPrompt = model.Billing.TokenPrices.LongContext.ContextMax
		}
		entry.LongContextMaxPromptTokens = longPrompt
		entry.EnrichedJSON = string(raw)
	}

	result := make([]db.CopilotModelCatalogEntry, 0, len(order))
	for _, id := range order {
		result = append(result, *entries[id])
	}
	return result, nil
}

// fetchCopilotEnrichedModels asks the installed CLI for the same account-aware
// model objects its SDK exposes. The GitHub token travels over the child's
// stdin JSON-RPC stream, never argv or logs.
func fetchCopilotEnrichedModels(ctx context.Context, copilotPath, githubToken string) ([]json.RawMessage, error) {
	processCtx, stopProcess := context.WithCancel(ctx)
	defer stopProcess()
	cmd := executil.CommandContextWithGrace(processCtx, copilotModelCatalogProcessGrace, copilotPath,
		"--server", "--stdio", "--no-auto-update", "--log-level", "error")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Copilot server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Copilot server stdout: %w", err)
	}
	// The models.list request carries a GitHub token over stdin. Discard child
	// diagnostics rather than risk a future CLI build echoing request params
	// into an error that agentd would log.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Copilot model server: %w", err)
	}
	stopIOWatch := make(chan struct{})
	go func() {
		select {
		case <-processCtx.Done():
			// EOF is the server protocol's graceful shutdown signal and also
			// prevents a loader/child pair from keeping this descriptor open
			// while process-group escalation runs.
			_ = stdin.Close()
		case <-stopIOWatch:
		}
	}()
	defer func() {
		close(stopIOWatch)
		_ = stdin.Close()
		stopProcess()
		_ = cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	if err := callCopilotStdioRPC(reader, stdin, 1, "connect", struct{}{}, nil); err != nil {
		return nil, fmt.Errorf("connect to Copilot model server: %w", err)
	}
	var result struct {
		Models []json.RawMessage `json:"models"`
	}
	params := struct {
		GitHubToken string `json:"gitHubToken"`
	}{GitHubToken: githubToken}
	if err := callCopilotStdioRPC(reader, stdin, 2, "models.list", params, &result); err != nil {
		return nil, fmt.Errorf("list enriched Copilot models: %w", err)
	}
	if len(result.Models) == 0 {
		return nil, errors.New("list enriched Copilot models: response contains no models")
	}
	return result.Models, nil
}

func callCopilotStdioRPC(reader *bufio.Reader, writer io.Writer, id int64, method string, params, result any) error {
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := writer.Write(body); err != nil {
		return err
	}
	for frames := 0; frames < 16; frames++ {
		frame, err := readCopilotStdioFrame(reader)
		if err != nil {
			return err
		}
		var response struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame, &response); err != nil {
			return fmt.Errorf("decode JSON-RPC response: %w", err)
		}
		if response.ID == nil || *response.ID != id {
			continue
		}
		if response.Error != nil {
			// This RPC stream carries credentials in request params. Do not relay
			// server-authored error text: a future implementation could echo the
			// rejected params and turn an operational log into a credential leak.
			return fmt.Errorf("JSON-RPC error %d", response.Error.Code)
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("decode JSON-RPC result: %w", err)
		}
		return nil
	}
	return fmt.Errorf("JSON-RPC response %d not received", id)
}

func readCopilotStdioFrame(reader *bufio.Reader) ([]byte, error) {
	contentLength := int64(-1)
	for lines := 0; lines < 64; lines++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			contentLength, err = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %w", err)
			}
		}
	}
	if contentLength < 0 || contentLength > copilotModelCatalogMaxResponse {
		return nil, fmt.Errorf("invalid Content-Length %d", contentLength)
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return body, nil
}

// copilotCatalogContextWindow returns the merged catalog's prompt-token limit
// for a context tier while the last successful mirror is fresh enough to
// trust. Lookup failures
// deliberately degrade silently to the static table: this sits on hot meter
// and dashboard paths, while refresh failures are already logged by the
// hourly background job.
func copilotCatalogContextWindow(model, contextTier string) int64 {
	limit, fresh, err := db.FreshCopilotModelPromptLimitForTier(
		strings.TrimSpace(model), strings.TrimSpace(contextTier), time.Now(), copilotModelCatalogMaxAge)
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
