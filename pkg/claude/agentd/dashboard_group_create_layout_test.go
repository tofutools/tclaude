package agentd

import (
	"strings"
	"testing"
)

// TestDashboardGroupCreate_AttachmentFieldsAreLast pins the shared form order
// used by blank, group, template, and clone creation in both visual themes.
func TestDashboardGroupCreate_AttachmentFieldsAreLast(t *testing.T) {
	island := dashboardAssetFile(t, "js/group-create-island.js")
	anchors := []string{
		`id="group-create-max-members-row"`,
		`id="group-create-attachment-url"`,
		`id="group-create-attachment-label"`,
		`id="group-create-error"`,
		`class="modal-buttons"`,
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
