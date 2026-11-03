package agentd

import (
	"strings"
	"testing"
)

// TestDashboardGroupCreate_AttachmentFieldsAreLast pins the shared form order
// used by blank, group, template, and clone creation in both visual themes.
// The error slot and footer follow the body; Dialog renders them after its
// children (jstest/ui-dialog-component.test.mjs).
func TestDashboardGroupCreate_AttachmentFieldsAreLast(t *testing.T) {
	island := dashboardAssetFile(t, "js/group-create-island.js")
	anchors := []string{
		`id="group-create-max-members-row"`,
		`id="group-create-attachment-url"`,
		`id="group-create-attachment-label"`,
		"</${Dialog}>",
	}

	previous := -1
	for _, anchor := range anchors {
		at := strings.Index(island, anchor)
		if at < 0 {
			t.Fatalf("shared group-create form is missing %q", anchor)
		}
		if at <= previous {
			t.Fatalf("shared group-create form has %q out of order", anchor)
		}
		previous = at
	}
}
