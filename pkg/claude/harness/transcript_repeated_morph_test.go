package harness

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

func TestTranscriptRepeatedMorphPairMatrix(t *testing.T) {
	for _, source := range nativeFixtures {
		if source.harness == OpenCodeName {
			continue
		}
		for _, target := range nativeFixtures {
			if target.harness == OpenCodeName || target.harness == source.harness {
				continue
			}
			t.Run(source.harness+"->"+target.harness, func(t *testing.T) {
				src := mustHarness(t, source.harness)
				tr, err := src.Transcript.Decode(bytes.NewReader(source.raw(t)), source.id)
				require.NoError(t, err)
				tr.Entries = append(tr.Entries, transcript.Entry{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{{Kind: transcript.BlockSummary, Text: "Remembered compacted decision"}}})
				var baseline, tools int
				for hop := 0; hop < 6; hop++ {
					dstName := target.harness
					if hop%2 == 1 {
						dstName = source.harness
					}
					dst := mustHarness(t, dstName)
					latest := fmt.Sprintf("Latest human instruction at hop %d", hop)
					tr.Entries = append(tr.Entries, transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text(latest)}})
					plan := transcript.NewPlan(tr, transcript.Options{Target: dstName, InlineMedia: dst.Transcript.InlineMedia(), MediaDir: t.TempDir(), ContextWindowTokens: 1000000})
					var buf bytes.Buffer
					id, err := dst.Transcript.Encode(plan, "/dst", &buf)
					require.NoError(t, err)
					require.NoError(t, dst.History.Validate(buf.Bytes(), id))
					tr, err = dst.Transcript.Decode(bytes.NewReader(buf.Bytes()), id)
					require.NoError(t, err)
					text := allText(tr)
					assert.Equal(t, 1, strings.Count(text, "⟦tclaude morph⟧"), "generated notices must not accumulate at hop %d", hop)
					assert.Equal(t, 1, strings.Count(text, fixtureSecret))
					assert.Equal(t, 1, strings.Count(text, fixtureAnswer))
					assert.Contains(t, text, fixtureOutput)
					assert.Equal(t, 1, strings.Count(text, "Remembered compacted decision"))
					assert.Equal(t, 1, strings.Count(text, "⟦Summary of the earlier conversation"))
					for prior := 0; prior <= hop; prior++ {
						assert.Contains(t, text, fmt.Sprintf("Latest human instruction at hop %d", prior))
					}
					if hop == 0 {
						baseline = len(text)
						tools = strings.Count(text, "⟦tool call")
					}
					assert.Equal(t, tools, strings.Count(text, "⟦tool call"), "tool wrappers remain flat")
					assert.LessOrEqual(t, len(text), baseline+600*(hop+1), "growth must be bounded by actual new turns")
				}
			})
		}
	}
}
