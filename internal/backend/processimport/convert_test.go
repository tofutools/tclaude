package processimport

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const simpleSource = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: retained
name: Retained process
start: start
params:
  count:
    type: number
    required: false
    default: 42
nodes:
  start:
    type: start
    name: Begin
    next: task
  task:
    type: task
    performer:
      kind: human
      profile: reviewer-name
      ask: Review {{ params.count }}?
      prompt: Long context
    next: done
  done:
    type: end
    result: done
layout:
  nodes:
    start: {x: 12, y: 24}
`

func TestInspectAndConvertRequireExplicitIndependentMappings(t *testing.T) {
	inspection, err := Inspect(simpleSource)
	require.NoError(t, err)
	require.False(t, inspection.Diagnostics.HasErrors(), "%+v", inspection.Diagnostics)
	require.Equal(t, "retained", inspection.OriginalID)
	require.Len(t, inspection.Requirements, 1)
	path := inspection.Requirements[0].Path
	require.Equal(t, "/nodes/task/performer", path)
	require.Equal(t, "reviewer-name", inspection.Requirements[0].Profile)
	_, err = Convert(simpleSource, nil)
	require.ErrorContains(t, err, "explicit human mapping")
	binding := Binding{Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}}
	converted, err := Convert(simpleSource, map[string]Binding{path: binding})
	require.NoError(t, err)
	require.Equal(t, simpleSource, converted.Source)
	require.Equal(t, "42", string(converted.Parameters[0].Default))
	require.False(t, converted.Parameters[0].Required)
	require.Equal(t, model.EditorPosition{X: 12, Y: 24}, converted.Layout.Nodes["start"])
	for _, n := range converted.Process.Graph.Nodes {
		if n.ID == "task" {
			require.Equal(t, "Review {{ params.count }}?", n.Performer.Human.Ask)
			require.Equal(t, "Long context", n.Performer.Human.Prompt)
			require.True(t, n.Performer.Human.Operator)
		}
	}
	require.Empty(t, binding.Performer.Human.Ask, "conversion must not mutate supplied mappings")
	_, err = Convert(simpleSource, map[string]Binding{path: binding, "/nodes/missing/performer": binding})
	require.ErrorContains(t, err, "does not identify")
}

func TestSourceBoundaryAndDiagnosticsPreventConversion(t *testing.T) {
	for _, source := range []string{"", strings.Repeat("x", MaxSourceBytes+1), string([]byte{0xff})} {
		_, err := Inspect(source)
		require.Error(t, err)
	}
	invalid := strings.Replace(simpleSource, "name: Begin", "name: Begin\n    unrecognized: true", 1)
	inspection, err := Inspect(invalid)
	require.NoError(t, err)
	require.True(t, inspection.Diagnostics.HasErrors())
	_, err = Convert(invalid, nil)
	require.Error(t, err)
}

func TestConversionDoesNotRoundAnUnsupportedEditorNumber(t *testing.T) {
	source := strings.Replace(simpleSource, "default: 42", "default: 9007199254740993", 1)
	inspection, err := Inspect(source)
	require.NoError(t, err)
	require.False(t, inspection.Diagnostics.HasErrors())
	_, err = Convert(source, map[string]Binding{"/nodes/task/performer": {Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}}})
	require.ErrorContains(t, err, "exact editor JSON support")
}

func TestEditorNumberRoundTripChecksFractionalAndExponentTokens(t *testing.T) {
	for _, raw := range []string{`0.1234567890123456789`, `1.234567890123456789e-1`, `[0.1234567890123456789]`, `{"nested":9007199254740993}`, `1e-999999`} {
		require.ErrorContains(t, exactEditorNumbers([]byte(raw)), "exact editor JSON support", raw)
	}
	for _, raw := range []string{`0.1`, `1e3`, `1.00`, `0e-999999`, `-0.0`, `{"nested":[0.5,42]}`} {
		require.NoError(t, exactEditorNumbers([]byte(raw)), raw)
	}
}

func TestSourceNumbersAreCheckedBeforeYAMLFloatDecoding(t *testing.T) {
	for _, value := range []string{"0.1234567890123456789", "1.234567890123456789e-1", ".1234567890123456789"} {
		source := strings.Replace(simpleSource, "default: 42", "default: "+value, 1)
		_, err := Convert(source, map[string]Binding{"/nodes/task/performer": {Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}}})
		require.ErrorContains(t, err, "exact editor JSON support", value)
	}
	for _, value := range []string{"0.1", ".5", "1_000", "0x10"} {
		source := strings.Replace(simpleSource, "default: 42", "default: "+value, 1)
		_, err := Convert(source, map[string]Binding{"/nodes/task/performer": {Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}}})
		require.NoError(t, err, value)
	}
}
