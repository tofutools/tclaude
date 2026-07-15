package agentd

import (
	"strings"
	"testing"
)

// helpersFuncBody returns the source span of a column-0 function in the
// embedded dashboard modules.
func helpersFuncBody(t *testing.T, name string) string {
	t.Helper()
	start := strings.Index(dashboardAssets, "\nfunction "+name+"(")
	if start < 0 {
		t.Fatalf("dashboard assets: function %s not found", name)
	}
	rest := dashboardAssets[start+1:]
	end := strings.Index(rest, "\nfunction ")
	if end < 0 {
		t.Fatalf("dashboard assets: could not bound function %s", name)
	}
	body := rest[:end]
	if i := strings.LastIndex(body, "\n}"); i >= 0 {
		body = body[:i+len("\n}")]
	}
	return body
}

func TestDashboardHTML_RetireButtonWired(t *testing.T) {
	// Grouped and ungrouped menus keep the deliberately conv-keyed launcher.
	tmpl := helpersFuncBody(t, "MemberMenu")
	for _, needle := range []string{
		`selector="conv" act="retire-agent"`,
		`className="warn" regular="retire" wizard="banish"`,
		`group=${group} act="remove-member"`,
		`act="delete-agent" className="danger"`,
	} {
		if !strings.Contains(tmpl, needle) {
			t.Errorf("MemberMenu: missing %q", needle)
		}
	}

	// Row, palette, and DnD all launch the one keyed transaction descriptor.
	for _, needle := range []string{
		"await openRetireAgentDialog(conv, label)",
		"openRetireAgentDialog(a.conv_id, label)",
		"result = await openRetireAgentDialog(conv, label)",
		"return openTransactionDialog({ kind: 'retire-agent', conv, label })",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("single-retire launcher ownership missing %q", needle)
		}
	}

	// The Preact island is the only writer for the modal id. Static markup and
	// imperative request/listener ownership must not coexist with it.
	if got := strings.Count(dashboardAssets, `id="retire-modal"`); got != 1 {
		t.Errorf("retire-modal writer count = %d, want exactly one Preact writer", got)
	}
	for _, retired := range []string{
		"function retireConfirm(",
		"async function retireAgentInteractive(",
		"function retireToast(",
		"async function maybeHandleDanglingRetire(",
	} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("single-retire migration left legacy owner %q", retired)
		}
	}

	// Endpoint and authority remain daemon-owned; the adapter only preserves
	// the raw conv id and exact option query.
	for _, needle := range []string{
		"`/api/agents/${encodeURIComponent(conv)}/retire?${query}`",
		"`shutdown=${choice.shutdown ? 1 : 0}`",
		"choice.deleteWorktree ? '&delete_worktree=1' : ''",
		"state.handoff()",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("transaction retire adapter missing %q", needle)
		}
	}
}

func TestDashboardHTML_RetireIconButtonWired(t *testing.T) {
	tmpl := helpersFuncBody(t, "MemberActions")
	for _, needle := range []string{
		`data-act="retire-agent"`,
		`data-conv=${member.conv_id}`,
		`data-label=${member.title || member.conv_id}`,
		`class="icon-btn warn"`,
		`<${TrashIcon} />`,
		`aria-label="Retire agent"`,
	} {
		if !strings.Contains(tmpl, needle) {
			t.Errorf("retireIconButton: missing %q", needle)
		}
	}
	for _, needle := range []string{`function TrashIcon()`, `class="trash-ico"`, `<svg`} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets: missing %q (trash glyph)", needle)
		}
	}
	if strings.Contains(tmpl, "data-agent=") {
		t.Error("retireIconButton must stay conv-keyed for dangling recovery")
	}
	if !strings.Contains(tmpl, "<${ActionMenu}") || !strings.Contains(tmpl, "<${MemberMenu}") {
		t.Error("MemberActions lost the menu retire twin")
	}
}

func TestDashboardHTML_SingleRetireSpinner(t *testing.T) {
	// The keyed frame keeps the action mounted while busy, then leaves an
	// explicit inline Retry after transport/non-dangling HTTP failures.
	for _, needle := range []string{
		`aria-busy=${busy && busyAction === 'primary' ? 'true' : undefined}`,
		`<span class="btn-spinner" aria-hidden="true"></span>`,
		`retrying ? 'Retrying…' : 'Retiring…'`,
		`primaryLabel=${retrying ? 'Retry' : 'Retire'}`,
		`errorID="retire-error"`,
		`if (activeRef.current) setError(cause?.message || String(cause))`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Preact single-retire feedback missing %q", needle)
		}
	}
	if !strings.Contains(dashboardAssets, ".btn-spinner {") {
		t.Error("dashboard assets: missing the shared .btn-spinner CSS rule")
	}
}
