package agentd

import (
	"strings"
	"testing"
)

func TestDashboardMessagesPreactOwnershipAndParityHooks(t *testing.T) {
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	controller := string(mustReadFS(dashboardAssetsFS, "js/mail.js"))
	css := string(mustReadFS(dashboardAssetsFS, "dashboard.css"))
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

	hostStart := strings.Index(css, "#messages-root {")
	if hostStart < 0 {
		t.Fatal("Messages island host is missing its layout rule")
	}
	hostEnd := strings.Index(css[hostStart:], "}")
	if hostEnd < 0 {
		t.Fatal("Messages island host layout rule is unterminated")
	}
	hostRule := css[hostStart : hostStart+hostEnd]
	for _, want := range []string{"display: flex", "flex: 1 1 auto", "min-height: 0"} {
		if !strings.Contains(hostRule, want) {
			t.Errorf("Messages island host layout rule missing %q", want)
		}
	}
}

func TestDashboardViewMessagesOpensFirstMessage(t *testing.T) {
	controller := string(mustReadFS(dashboardAssetsFS, "js/mail.js"))
	start := strings.Index(controller, "async function openMailbox(id) {")
	if start < 0 {
		t.Fatal("Messages controller is missing openMailbox")
	}
	end := strings.Index(controller[start:], "\n}\n\n// --- access requests")
	if end < 0 {
		t.Fatal("could not isolate openMailbox")
	}
	openMailbox := controller[start : start+end]

	awaitSelection := strings.Index(openMailbox, "const selected = await selectMailbox(id);")
	selectFirst := strings.Index(openMailbox, "mail.selectedMsgId = mail.messages[0]?.id ?? null;")
	paintReader := strings.Index(openMailbox, "paintReader();")
	if awaitSelection < 0 || selectFirst < 0 || paintReader < 0 {
		t.Fatalf("openMailbox must await the folder load, select its first message, and paint the reader:\n%s", openMailbox)
	}
	if awaitSelection >= selectFirst || selectFirst >= paintReader {
		t.Fatalf("openMailbox selection steps are out of order:\n%s", openMailbox)
	}
}
