package harness

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// compactInputs normalizes tool-call arguments, which PAF stores as compact
// JSON.
func compactInputs(t *testing.T, tr *transcript.Transcript) {
	t.Helper()
	for i := range tr.Entries {
		for j := range tr.Entries[i].Blocks {
			b := &tr.Entries[i].Blocks[j]
			if len(b.Input) == 0 {
				continue
			}
			var buf bytes.Buffer
			require.NoError(t, json.Compact(&buf, b.Input))
			b.Input = buf.Bytes()
		}
	}
}

// TestTranscriptPAFRoundTripEachHarness checks that every harness's decoded
// neutral transcript, with its loss report and native blob, survives a PAF
// write and read unchanged.
func TestTranscriptPAFRoundTripEachHarness(t *testing.T) {
	for _, fx := range nativeFixtures {
		t.Run(fx.harness, func(t *testing.T) {
			h := mustHarness(t, fx.harness)
			raw := fx.raw(t)
			tr, err := h.Transcript.Decode(bytes.NewReader(raw), fx.id)
			require.NoError(t, err)
			require.NotEmpty(t, tr.Entries)

			var buf bytes.Buffer
			w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
			native := paf.AgentPath("a", "native/"+h.History.Format()+"/history.jsonl")
			require.NoError(t, w.AddAgent("a", paf.Agent{Name: "a", Native: &paf.Native{Format: h.History.Format(), Path: native, Harness: fx.harness}}, paf.Environment{Harness: paf.Harness{Name: fx.harness}}))
			require.NoError(t, paf.WriteTranscript(w, "a", paf.ScopeEffective, tr))
			require.NoError(t, w.WriteJSON(paf.AgentPath("a", "context/losses.json"), paf.LossesFromReport(tr.Losses)))
			require.NoError(t, w.WriteFile(native, "application/jsonl", raw))
			_, err = w.Close(paf.Producer{Name: "test"})
			require.NoError(t, err)

			a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{})
			require.NoError(t, err)
			got, err := paf.ReadTranscript(a, "a", paf.ScopeEffective, paf.ReadOptions{})
			require.NoError(t, err)
			compactInputs(t, tr)
			compactInputs(t, got)
			assert.Equal(t, tr.Source, got.Source)
			assert.Equal(t, tr.Entries, got.Entries)
			l, ok := a.Losses("a")
			require.True(t, ok)
			assert.Equal(t, tr.Losses.Lines(), paf.ReportFromLosses(l).Lines())
			blob, err := a.ReadEntry(native, 1<<20)
			require.NoError(t, err)
			assert.Equal(t, raw, blob, "the native blob is stored byte for byte")
			require.NoError(t, h.History.Validate(blob, fx.id), "the stored native blob still validates")
		})
	}
}
