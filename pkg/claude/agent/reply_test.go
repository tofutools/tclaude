package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplyCmdSupportsBodyFlag(t *testing.T) {
	cmd := replyCmd()
	flag := cmd.Flags().Lookup("body")
	require.NotNil(t, flag, "reply should accept --body like message does")
	assert.Equal(t, "string", flag.Value.Type())
	assert.Contains(t, cmd.UseLine(), "[text]")
}

// TestRunReplyBodySources covers the four interchangeable body sources on
// `reply`, which must behave exactly as they do on `message`. Each case
// runs through runReply so the adapter that feeds readBody is exercised,
// not just readBody itself.
func TestRunReplyBodySources(t *testing.T) {
	// Written into a temp file for the --file case, which is otherwise
	// only reachable through the adapter's File field.
	const fileBody = "body loaded from a file\n"
	fileDir := t.TempDir()
	bodyFile := filepath.Join(fileDir, "reply.md")
	require.NoError(t, os.WriteFile(bodyFile, []byte(fileBody), 0o600))

	tests := []struct {
		name        string
		params      replyParams
		stdin       string
		wantBody    string
		wantSubject string
		wantRC      int
		wantErrSub  string
	}{
		{name: "positional", params: replyParams{ID: "42", Text: "positional text"}, wantBody: "positional text", wantRC: rcOK},
		{name: "body flag", params: replyParams{ID: "42", Body: "flag text"}, wantBody: "flag text", wantRC: rcOK},
		{name: "stdin", params: replyParams{ID: "42", Stdin: true}, stdin: "from stdin", wantBody: "from stdin", wantRC: rcOK},
		{name: "file", params: replyParams{ID: "42", File: bodyFile}, wantBody: fileBody, wantRC: rcOK},
		{name: "file dash reads stdin", params: replyParams{ID: "42", File: "-"}, stdin: "piped in", wantBody: "piped in", wantRC: rcOK},
		{
			// --subject rides alongside the body rather than replacing it;
			// the adapter passes it on a separate path from readBody.
			name:        "subject forwarded with body",
			params:      replyParams{ID: "42", Body: "flag text", Subject: "rollback plan"},
			wantBody:    "flag text",
			wantSubject: "rollback plan",
			wantRC:      rcOK,
		},
		{
			name:       "body flag and file conflict",
			params:     replyParams{ID: "42", Body: "flag", File: bodyFile},
			wantRC:     rcInvalidArg,
			wantErrSub: "only one",
		},
		{
			name:       "positional and body flag conflict",
			params:     replyParams{ID: "42", Text: "positional", Body: "flag"},
			wantRC:     rcInvalidArg,
			wantErrSub: "only one",
		},
		{
			name:       "no source",
			params:     replyParams{ID: "42"},
			wantRC:     rcInvalidArg,
			wantErrSub: "--body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevAvail := DaemonAvailableImpl
			prevReq := DaemonRequestImpl
			t.Cleanup(func() {
				DaemonAvailableImpl = prevAvail
				DaemonRequestImpl = prevReq
			})
			DaemonAvailableImpl = func() bool { return true }

			var gotBody, gotSubject string
			DaemonRequestImpl = func(_, _ string, in, _ any, _ DaemonOpts) error {
				payload, ok := in.(map[string]string)
				require.True(t, ok, "reply payload should be map[string]string, got %T", in)
				gotBody = payload["body"]
				gotSubject = payload["subject"]
				return nil
			}

			var stdout, stderr bytes.Buffer
			rc := runReply(&tt.params, &stdout, &stderr, strings.NewReader(tt.stdin))
			assert.Equal(t, tt.wantRC, rc, "stderr=%s", stderr.String())
			assert.Equal(t, tt.wantBody, gotBody)
			assert.Equal(t, tt.wantSubject, gotSubject, "--subject must reach the daemon payload")
			if tt.wantErrSub != "" {
				assert.Contains(t, stderr.String(), tt.wantErrSub)
			}
		})
	}
}
