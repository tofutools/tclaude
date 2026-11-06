package proto

import "testing"

// Federated text is cleaned on ingress of what a terminal would interpret,
// bidi controls included (tcl-7gm2ec): a U+202E override makes
// "invoice\u202egpj.exe" read as "invoiceexe.jpg".
func TestStripControlsDropsBidiControls(t *testing.T) {
	if got := StripControls("invoice\u202egpj.exe \u2066x\u2069 \u202ay\u202c"); got != "invoicegpj.exe x y" {
		t.Fatalf("got %q", got)
	}
	if got := StripControls("שלום\u200fworld"); got != "שלום\u200fworld" {
		t.Fatalf("a bidi mark is kept, and RTL text with it: got %q", got)
	}
}

// StripControls doubles as a validator: a field it would change is refused.
// A spawn profile named with a bidi override is now one of those.
func TestCatalogDropsASpawnProfileNamedWithABidiControl(t *testing.T) {
	got := sanitizeSpawnProfiles([]CatalogSpawnProfile{{Name: "build\u202eevil"}, {Name: "build"}})
	if len(got) != 1 || got[0].Name != "build" {
		t.Fatalf("got %+v", got)
	}
}
