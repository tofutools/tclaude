package copilotfixture

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests need no Copilot binary, so they run under plain `go test ./...`
// and protect the property that actually matters for a committed fixture:
// nothing volatile, private or bulky survives sanitization.

func TestSanitizerNormalizesVolatileValues(t *testing.T) {
	s := NewSanitizer("/tmp/run/copilot-home", "/tmp/run/cache", "/tmp/run/work")

	in := "session 66f40749-7cbc-455b-a271-d6e6799d427f at 2026-08-03T17:12:06.026Z " +
		"wrote /tmp/run/copilot-home/session-state via http://127.0.0.1:8899/v1"
	got := s.Text(in)

	assert.Contains(t, got, uuidPlaceholder)
	assert.Contains(t, got, timestampPlaceholder)
	assert.Contains(t, got, "<tmp>/home/session-state")
	assert.Contains(t, got, baseURLPlaceholder)

	assert.NotContains(t, got, "66f40749")
	assert.NotContains(t, got, "2026-08-03T17")
	assert.NotContains(t, got, "/tmp/run")
	assert.NotContains(t, got, ":8899")
}

// A timestamp with an offset rather than Z must normalize too: the CLI injects
// a local-offset <current_datetime> into every user message.
func TestSanitizerNormalizesOffsetTimestamps(t *testing.T) {
	s := NewSanitizer("", "", "")
	assert.Equal(t, timestampPlaceholder, s.Text("2026-08-03T19:15:50.740+02:00"))
}

// The cache directory nests under the same root as home; the longest path must
// win so a parent cannot partially rewrite a child.
func TestSanitizerPrefersLongestPath(t *testing.T) {
	s := NewSanitizer("/tmp/run", "/tmp/run/cache", "/tmp/run/work")
	assert.Equal(t, "<tmp>/cache/pkg", s.Text("/tmp/run/cache/pkg"))
}

func TestRequestObservationKeepsShapeNotContent(t *testing.T) {
	systemPrompt := strings.Repeat("bulk system prompt ", 2000)
	req := RecordedRequest{
		Path: "/v1/chat/completions",
		Header: http.Header{
			"Authorization":      []string{"Bearer "},
			"X-Initiator":        []string{"user"},
			"X-Interaction-Type": []string{"conversation-user"},
		},
		Body: map[string]any{
			"model":          "wire-model",
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			"messages": []any{
				map[string]any{"role": "system", "content": systemPrompt},
				map[string]any{"role": "user", "content": "hello"},
			},
			"tools": []any{
				map[string]any{"function": map[string]any{"name": "view", "parameters": "huge schema"}},
				map[string]any{"function": map[string]any{"name": "bash", "parameters": "huge schema"}},
			},
		},
	}

	obs := NewSanitizer("", "", "").Request(req)

	assert.Equal(t, "/v1/chat/completions", obs.Path)
	assert.Equal(t, "wire-model", obs.Model)
	assert.True(t, obs.Stream)
	assert.True(t, obs.StreamIncludeUsage)
	assert.Equal(t, "user", obs.Initiator)
	assert.Equal(t, "conversation-user", obs.InteractionType)
	assert.Equal(t, []string{"system", "user"}, obs.MessageRoles)
	// Sorted, so a reordering in the CLI's tool registry is not spurious drift.
	assert.Equal(t, []string{"bash", "view"}, obs.ToolNames)
	assert.Equal(t, []string{"messages", "model", "stream", "stream_options", "tools"}, obs.BodyKeys)

	// The whole point: bulk content is reduced to a digest.
	encoded, err := Marshal(obs)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "bulk system prompt")
	assert.NotContains(t, string(encoded), "huge schema")
	assert.Less(t, len(encoded), 2048, "an observation must stay small")
	assert.True(t, strings.HasPrefix(obs.PromptDigest, "sha256:"))
}

// The digest must be sensitive to prompt changes (that is the drift signal)
// but insensitive to the volatile datetime the CLI injects (that would make
// every run differ).
func TestPromptDigestIsStableButDriftSensitive(t *testing.T) {
	s := NewSanitizer("", "", "")
	build := func(content string) RecordedRequest {
		return RecordedRequest{Body: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": content}},
		}}
	}

	a := s.Request(build("<current_datetime>2026-08-03T19:15:50.740+02:00</current_datetime>\n\nhi"))
	b := s.Request(build("<current_datetime>2027-01-01T00:00:00.000+00:00</current_datetime>\n\nhi"))
	assert.Equal(t, a.PromptDigest, b.PromptDigest,
		"an injected datetime must not make two identical prompts differ")

	c := s.Request(build("<current_datetime>2026-08-03T19:15:50.740+02:00</current_datetime>\n\nDIFFERENT"))
	assert.NotEqual(t, a.PromptDigest, c.PromptDigest,
		"a real prompt change must change the digest")
}

// The credential-free signature: the SDK always sends Authorization, but with
// an empty bearer. A real token must be detected as non-empty.
func TestAuthorizationEmptinessDetection(t *testing.T) {
	s := NewSanitizer("", "", "")
	for _, tc := range []struct {
		name      string
		header    string
		wantEmpty bool
	}{
		{"trailing space", "Bearer ", true},
		{"bare", "Bearer", true},
		{"real token", "Bearer ghp_realtokenvalue", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := s.Request(RecordedRequest{
				Header: http.Header{"Authorization": []string{tc.header}},
				Body:   map[string]any{},
			})
			assert.True(t, obs.AuthorizationPresent)
			assert.Equal(t, tc.wantEmpty, obs.AuthorizationEmpty)
		})
	}

	obs := s.Request(RecordedRequest{Header: http.Header{}, Body: map[string]any{}})
	assert.False(t, obs.AuthorizationPresent)
}
