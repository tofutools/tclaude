package paf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCarriedSafeOnlyDropsAllOfAnUnsafeExtension(t *testing.T) {
	c := &CarriedExtensions{
		Decls: map[string]ExtensionDecl{"com.a": {Copy: "safe"}, "com.b": {}, "com.c": {Copy: "maybe"}},
		Agent: Extensions{"com.a": json.RawMessage(`1`), "com.b": json.RawMessage(`2`)},
		Lines: map[string]Extensions{"e1": {"com.b": json.RawMessage(`3`)}, "e2": {"com.a": json.RawMessage(`4`)}},
		Entries: []CarriedEntry{
			{Path: "ext/com.a/x"}, {Path: "ext/com.b/y"}, {Path: "ext/com.c/z"},
		},
	}
	got := c.SafeOnly()
	assert.Len(t, got.Decls, 1, "an unknown copy value means unsafe")
	assert.Contains(t, got.Agent, "com.a")
	assert.NotContains(t, got.Agent, "com.b")
	assert.NotContains(t, got.Lines, "e1")
	assert.Contains(t, got.Lines, "e2")
	assert.Equal(t, []CarriedEntry{{Path: "ext/com.a/x"}}, got.Entries)
	assert.Nil(t, (&CarriedExtensions{Decls: map[string]ExtensionDecl{"com.b": {}}}).SafeOnly())
	assert.Nil(t, (*CarriedExtensions)(nil).SafeOnly())
}
