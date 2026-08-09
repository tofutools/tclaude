package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRefreshCopilotModelCatalogSkipsWhenCLIsAreMissing(t *testing.T) {
	setupTestDB(t)
	commands := 0
	requests := 0
	deps := copilotModelCatalogDeps{
		lookPath: func(name string) (string, error) {
			if name == "gh" {
				return "", exec.ErrNotFound
			}
			return "/bin/" + name, nil
		},
		commandOutput: func(context.Context, string, ...string) ([]byte, error) {
			commands++
			return nil, nil
		},
		doRequest: func(*http.Request) (*http.Response, error) {
			requests++
			return nil, nil
		},
		now: time.Now,
	}

	outcome, err := refreshCopilotModelCatalog(context.Background(), deps)
	require.NoError(t, err)
	assert.Equal(t, []string{"gh"}, outcome.Missing)
	assert.Equal(t, "missing", outcome.GHCLI)
	assert.Equal(t, "installed", outcome.CopilotCLI)
	assert.Equal(t, "not_checked", outcome.GHAuth)
	assert.Zero(t, commands)
	assert.Zero(t, requests)
}

func TestRefreshCopilotModelCatalogMirrorsAuthenticatedResponse(t *testing.T) {
	setupTestDB(t)
	fetchedAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var request *http.Request
	deps := copilotModelCatalogDeps{
		lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		commandOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch strings.Join(args, " ") {
			case "auth token":
				return []byte("secret-token\n"), nil
			default:
				return []byte(`{"data":{"viewer":{"copilotEndpoints":{"api":"https://api.individual.githubcopilot.com"}}}}`), nil
			}
		},
		doRequest: func(req *http.Request) (*http.Response, error) {
			request = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"claude-haiku-4.5","capabilities":{"limits":{"max_context_window_tokens":144000,"max_prompt_tokens":128000,"max_output_tokens":32000}}}]}`)),
			}, nil
		},
		fetchEnriched: func(context.Context, string, string) ([]json.RawMessage, error) {
			return []json.RawMessage{json.RawMessage(`{
				"id":"claude-haiku-4.5",
				"capabilities":{"limits":{"max_prompt_tokens":128000}},
				"billing":{"tokenPrices":{"maxPromptTokens":128000,
					"longContext":{"maxPromptTokens":512000}}}}`)}, nil
		},
		now: func() time.Time { return fetchedAt },
	}

	outcome, err := refreshCopilotModelCatalog(context.Background(), deps)
	require.NoError(t, err)
	assert.Equal(t, 1, outcome.Models)
	assert.Equal(t, "installed", outcome.GHCLI)
	assert.Equal(t, "installed", outcome.CopilotCLI)
	assert.Equal(t, "authenticated", outcome.GHAuth)
	assert.Equal(t, "ok", outcome.EnrichedStatus)
	assert.Equal(t, 1, outcome.EnrichedModels)
	require.NotNil(t, request)
	assert.Equal(t, "https://api.individual.githubcopilot.com/models", request.URL.String())
	assert.Equal(t, "Bearer secret-token", request.Header.Get("Authorization"))
	assert.Equal(t, copilotModelCatalogIntegrationID, request.Header.Get("Copilot-Integration-Id"))
	limit, fresh, err := db.FreshCopilotModelPromptLimit(
		"claude-haiku-4.5", fetchedAt.Add(time.Hour), copilotModelCatalogMaxAge)
	require.NoError(t, err)
	assert.True(t, fresh)
	assert.Equal(t, int64(128_000), limit)
	limit, fresh, err = db.FreshCopilotModelPromptLimitForTier(
		"claude-haiku-4.5", "long_context", fetchedAt.Add(time.Hour), copilotModelCatalogMaxAge)
	require.NoError(t, err)
	assert.True(t, fresh)
	assert.Equal(t, int64(512_000), limit)
}

func TestMergeCopilotModelCatalogCombinesCoverageAndPrefersTierMetadata(t *testing.T) {
	entries, err := mergeCopilotModelCatalog(
		[]json.RawMessage{json.RawMessage(`{"id":"remote-only","capabilities":{"limits":{"max_prompt_tokens":100}}}`)},
		[]json.RawMessage{
			json.RawMessage(`{"id":"remote-only","billing":{"tokenPrices":{"maxPromptTokens":110,"longContext":{"maxPromptTokens":900}}}}`),
			json.RawMessage(`{"id":"enriched-only","capabilities":{"limits":{"max_prompt_tokens":200}}}`),
		},
	)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "remote-only", entries[0].ModelID)
	assert.Equal(t, int64(110), entries[0].MaxPromptTokens)
	assert.Equal(t, int64(900), entries[0].LongContextMaxPromptTokens)
	assert.NotEmpty(t, entries[0].RawJSON)
	assert.NotEmpty(t, entries[0].EnrichedJSON)
	assert.Equal(t, "enriched-only", entries[1].ModelID)
	assert.Equal(t, int64(200), entries[1].MaxPromptTokens)
}

func TestCallCopilotStdioRPCFramesRequestAndDecodesResult(t *testing.T) {
	response := []byte(`{"jsonrpc":"2.0","id":2,"result":{"models":[{"id":"auto"}]}}`)
	reader := bufio.NewReader(bytes.NewReader([]byte(
		"Content-Length: " + strconv.Itoa(len(response)) + "\r\n\r\n" + string(response))))
	var writer bytes.Buffer
	var result struct {
		Models []json.RawMessage `json:"models"`
	}
	require.NoError(t, callCopilotStdioRPC(reader, &writer, 2, "models.list",
		map[string]string{"gitHubToken": "test-token"}, &result))
	require.Len(t, result.Models, 1)
	assert.Contains(t, writer.String(), `"method":"models.list"`)
	assert.Contains(t, writer.String(), `"gitHubToken":"test-token"`)
}

func TestRefreshCopilotModelCatalogFailurePreservesLastGoodMirror(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	require.NoError(t, db.ReplaceCopilotModelCatalog([]db.CopilotModelCatalogEntry{
		{ModelID: "claude-haiku-4.5", MaxPromptTokens: 128_000},
	}, now))
	deps := copilotModelCatalogDeps{
		lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		commandOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if strings.Join(args, " ") == "auth token" {
				return []byte("secret-token"), nil
			}
			return []byte(`{"data":{"viewer":{"copilotEndpoints":{"api":"https://api.githubcopilot.com"}}}}`), nil
		},
		doRequest: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK",
				Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		},
		now: func() time.Time { return now.Add(time.Hour) },
	}

	_, err := refreshCopilotModelCatalog(context.Background(), deps)
	require.ErrorContains(t, err, "response contains no models")
	limit, fresh, lookupErr := db.FreshCopilotModelPromptLimit(
		"claude-haiku-4.5", now.Add(time.Hour), copilotModelCatalogMaxAge)
	require.NoError(t, lookupErr)
	assert.True(t, fresh)
	assert.Equal(t, int64(128_000), limit)
}

func TestRefreshCopilotModelCatalogTreatsMissingAuthAsError(t *testing.T) {
	setupTestDB(t)
	deps := copilotModelCatalogDeps{
		lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		commandOutput: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("must not be logged"), errors.New("exit status 1")
		},
		doRequest: func(*http.Request) (*http.Response, error) {
			t.Fatal("request must not run without authentication")
			return nil, nil
		},
		now: time.Now,
	}

	outcome, err := refreshCopilotModelCatalog(context.Background(), deps)
	require.ErrorContains(t, err, "gh authentication unavailable")
	assert.NotContains(t, err.Error(), "must not be logged")
	assert.Equal(t, "unavailable", outcome.GHAuth)
}
