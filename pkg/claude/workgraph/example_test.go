package workgraph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedExamples_AllLoadAndValidate(t *testing.T) {
	names := exampleNames()
	require.NotEmpty(t, names, "expected at least one embedded example template")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			tmpl, err := loadExample(name)
			require.NoError(t, err)
			assert.NotEmpty(t, tmpl.Name)
			assert.NotEmpty(t, tmpl.Nodes)
			assert.NotEmpty(t, tmpl.Entry)
		})
	}
}

func TestEmbeddedExample_ImplementMicroservice(t *testing.T) {
	tmpl, err := loadExample("implement-microservice")
	require.NoError(t, err)

	assert.Equal(t, "implement-microservice", tmpl.Name)
	assert.Equal(t, SourceExample, tmpl.Source)
	assert.Equal(t, []string{"plan"}, tmpl.Entry)
	assert.Len(t, tmpl.Nodes, 6)

	// review is an enum decision node with the two branch outcomes.
	require.Contains(t, tmpl.Nodes, "review")
	assert.Equal(t, VerifyEnum, tmpl.Nodes["review"].Verify.Kind)
	assert.ElementsMatch(t, []string{"approved", "changes"}, tmpl.Nodes["review"].Verify.Values)

	// test loops back to implement on failure, so it continues on failure.
	require.Contains(t, tmpl.Nodes, "test")
	assert.Equal(t, OnFailContinue, tmpl.Nodes["test"].OnFail)

	// the chart wires up the fix loop and the approved->deploy branch.
	es := edgeSet(tmpl.Edges)
	assert.True(t, es["test->implement|fail"], "expected test --|fail|--> implement")
	assert.True(t, es["review->implement|changes"], "expected review --|changes|--> implement")
	assert.True(t, es["review->deploy|approved"], "expected review --|approved|--> deploy")

	// params include the required service_name and defaulted env.
	var sawService, sawEnv bool
	for _, p := range tmpl.Params {
		switch p.Name {
		case "service_name":
			sawService = true
			assert.True(t, p.IsRequired())
		case "env":
			sawEnv = true
			assert.Equal(t, "staging", p.Default)
		}
	}
	assert.True(t, sawService && sawEnv, "expected service_name and env params")
}
