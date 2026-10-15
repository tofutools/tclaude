package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// Without timestamps an entry is part of the open turn only after the
// transcript's last operator message; with no such message, nothing is.
func TestScribeLiveUntimedEntriesNeedAnOperatorMessage(t *testing.T) {
	agent := func(s string) transcript.Entry {
		return transcript.Entry{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{{Kind: transcript.BlockText, Text: s}}}
	}
	human := transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{{Kind: transcript.BlockText, Text: "go"}}}
	texts := func(t *transcript.Transcript) []string {
		cands, dropped := scribeLiveCandidates(t)
		var out []string
		for _, item := range scribeLiveSince(cands, dropped, time.Now()).Items {
			out = append(out, item.Text)
		}
		return out
	}
	assert.Empty(t, texts(&transcript.Transcript{Entries: []transcript.Entry{agent("old turn")}}))
	assert.Equal(t, []string{"new turn"}, texts(&transcript.Transcript{Entries: []transcript.Entry{agent("old turn"), human, agent("new turn")}}))
}
