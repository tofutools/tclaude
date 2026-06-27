package db

import (
	"reflect"
	"testing"
)

// TestAgentDeletionCounts_AddCoversEveryField is a maintenance tripwire: it sets
// every int64 field of AgentDeletionCounts to 1 and asserts Add sums all of
// them. If someone adds a new field to AgentDeletionCounts but forgets to extend
// Add, that field stays 0 here and the test fails — guarding the actor-level
// cross-generation delete (conv.DeleteAgentAllGenerations) against silently
// undercounting a newly-added table's removals.
func TestAgentDeletionCounts_AddCoversEveryField(t *testing.T) {
	var o AgentDeletionCounts
	ov := reflect.ValueOf(&o).Elem()
	for i := 0; i < ov.NumField(); i++ {
		f := ov.Field(i)
		if f.Kind() != reflect.Int64 {
			t.Fatalf("AgentDeletionCounts.%s is %s, not int64 — update this test if the shape changed",
				ov.Type().Field(i).Name, f.Kind())
		}
		f.SetInt(1)
	}

	var c AgentDeletionCounts
	c.Add(o)

	cv := reflect.ValueOf(&c).Elem()
	for i := 0; i < cv.NumField(); i++ {
		if got := cv.Field(i).Int(); got != 1 {
			t.Errorf("Add did not sum field %s (got %d, want 1) — extend AgentDeletionCounts.Add",
				cv.Type().Field(i).Name, got)
		}
	}
}
