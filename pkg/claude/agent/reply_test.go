package agent

import (
	"bytes"
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
	tests := []struct {
		name       string
		params     replyParams
		stdin      string
		wantBody   string
		wantRC     int
		wantErrSub string
	}{
		{name: "positional", params: replyParams{ID: "42", Text: "positional text"}, wantBody: "positional text", wantRC: rcOK},
		{name: "body flag", params: replyParams{ID: "42", Body: "flag text"}, wantBody: "flag text", wantRC: rcOK},
		{name: "stdin", params: replyParams{ID: "42", Stdin: true}, stdin: "from stdin", wantBody: "from stdin", wantRC: rcOK},
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

			var gotBody string
			DaemonRequestImpl = func(_, _ string, in, _ any, _ DaemonOpts) error {
				payload, ok := in.(map[string]string)
				require.True(t, ok, "reply payload should be map[string]string, got %T", in)
				gotBody = payload["body"]
				return nil
			}

			var stdout, stderr bytes.Buffer
			rc := runReply(&tt.params, &stdout, &stderr, strings.NewReader(tt.stdin))
			assert.Equal(t, tt.wantRC, rc, "stderr=%s", stderr.String())
			assert.Equal(t, tt.wantBody, gotBody)
			if tt.wantErrSub != "" {
				assert.Contains(t, stderr.String(), tt.wantErrSub)
			}
		})
	}
}
