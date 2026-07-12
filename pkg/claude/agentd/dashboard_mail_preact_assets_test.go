package agentd

import (
	"strings"
	"testing"
)

func TestDashboardMessagesPreactOwnershipAndParityHooks(t *testing.T) {
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	controller := string(mustReadFS(dashboardAssetsFS, "js/mail.js"))
	html := string(dashboardIndexHTML)

	for _, forbidden := range []string{"html`<>", "</>`"} {
		if strings.Contains(island, forbidden) {
			t.Errorf("Messages island uses unsupported HTM fragment shorthand %q", forbidden)
		}
	}
	for _, forbidden := range []string{"morphInto", ".innerHTML", "mailboxRowHTML", "accessRowHTML"} {
		if strings.Contains(controller, forbidden) {
			t.Errorf("Messages controller retains legacy render path %q", forbidden)
		}
	}
	for _, want := range []string{
		`id="filter-mailboxes" type="text"`,
		`id="filter-messages" type="text"`,
		`id="mail-show-retired"`,
		`id="mail-show-empty"`,
		`id="mail-show-prev-gens"`,
		`class="mail-gutter" data-boundary="sidebar-list"`,
		`class="mail-gutter" data-boundary="list-reader"`,
		`class="mail-attachment"`,
		`data-kind=${controller.msgKind(message)}`,
	} {
		if !strings.Contains(island, want) {
			t.Errorf("Messages island missing parity hook %q", want)
		}
	}
	for _, want := range []string{
		"if (mail.busy) return;",
		"const deleteURL = messageDeleteEndpoint(mail.selected);",
		"postDeleteMessages(batch, deleteURL)",
	} {
		if !strings.Contains(controller, want) {
			t.Errorf("Messages controller missing busy-safe mutation wiring %q", want)
		}
	}
	if !strings.Contains(html, `<div id="messages-root">`) {
		t.Error("dashboard is missing the stable Messages island host")
	}
	if strings.Contains(html, `<div class="mail-client">`) {
		t.Error("legacy static Messages client remains in dashboard.html")
	}
}
