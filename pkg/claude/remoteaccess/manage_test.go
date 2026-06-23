package remoteaccess_test

import (
	"slices"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
)

func setupTempMaterial(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	if _, err := remoteaccess.Setup(remoteaccess.SetupOptions{
		Bind:        "0.0.0.0:8443",
		Passphrase:  "passphrase-12",
		ClientName:  "phone",
		P12Password: "p12pw",
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
}

func TestValidClientName(t *testing.T) {
	for _, ok := range []string{"phone", "Johans-iPad", "dev_1", "a", "A1b2_c3-d4"} {
		if !remoteaccess.ValidClientName(ok) {
			t.Errorf("ValidClientName(%q) = false, want true", ok)
		}
	}
	// Rejected: empty, traversal, slashes, dots, leading non-alnum, too long.
	for _, bad := range []string{"", "..", "../etc", "a/b", "a.b", "-lead", "_x", "a b", string(make([]byte, 65))} {
		if remoteaccess.ValidClientName(bad) {
			t.Errorf("ValidClientName(%q) = true, want false", bad)
		}
	}
}

func TestServerCertSANs(t *testing.T) {
	setupTempMaterial(t)
	sans, err := remoteaccess.ServerCertSANs()
	if err != nil {
		t.Fatalf("ServerCertSANs: %v", err)
	}
	// Setup's ServerCertHosts always seeds the loopback set.
	if !slices.Contains(sans, "localhost") {
		t.Errorf("SANs %v missing localhost", sans)
	}
	if !slices.Contains(sans, "127.0.0.1") {
		t.Errorf("SANs %v missing 127.0.0.1", sans)
	}
}

func TestListClientsAndP12(t *testing.T) {
	setupTempMaterial(t)
	clients, err := remoteaccess.ListClients()
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "phone" {
		t.Fatalf("ListClients = %+v, want one client named phone", clients)
	}
	if !clients[0].HasP12 {
		t.Errorf("phone should have a downloadable .p12")
	}
	if clients[0].NotAfter.IsZero() {
		t.Errorf("phone cert NotAfter should be populated")
	}

	// A second device shows up after AddClient.
	if _, err := remoteaccess.AddClient("tablet", "p12pw2", ""); err != nil {
		t.Fatalf("AddClient: %v", err)
	}
	clients, _ = remoteaccess.ListClients()
	if len(clients) != 2 {
		t.Fatalf("after AddClient, ListClients = %+v, want 2", clients)
	}

	// The .p12 is readable for a valid device and rejected for a bogus name.
	if data, err := remoteaccess.ClientP12("phone"); err != nil || len(data) == 0 {
		t.Errorf("ClientP12(phone) = %d bytes, err=%v; want non-empty", len(data), err)
	}
	if _, err := remoteaccess.ClientP12("../../etc/passwd"); err == nil {
		t.Error("ClientP12 must reject a traversal name")
	}
}

func TestAddClientRejectsBadName(t *testing.T) {
	setupTempMaterial(t)
	if _, err := remoteaccess.AddClient("../evil", "p12pw", ""); err == nil {
		t.Error("AddClient must reject a path-traversal name")
	}
}

// TestReissueServerCert adds a SAN without rotating the CA: the new server cert
// covers the added host AND keeps the originals, while the CA cert is byte-for-
// byte unchanged (so installed client identities keep verifying).
func TestReissueServerCert(t *testing.T) {
	setupTempMaterial(t)
	caBefore, err := remoteaccess.CACert()
	if err != nil {
		t.Fatalf("CACert: %v", err)
	}

	sans, err := remoteaccess.ReissueServerCert("0.0.0.0:8443", []string{"example.com", "myhost.tailnet.ts.net"})
	if err != nil {
		t.Fatalf("ReissueServerCert: %v", err)
	}
	if !slices.Contains(sans, "example.com") || !slices.Contains(sans, "myhost.tailnet.ts.net") {
		t.Errorf("reissued SANs %v missing the added hosts", sans)
	}
	if !slices.Contains(sans, "localhost") {
		t.Errorf("reissued SANs %v dropped the original localhost", sans)
	}

	// The on-disk cert reflects the addition, cumulatively.
	got, err := remoteaccess.ServerCertSANs()
	if err != nil {
		t.Fatalf("ServerCertSANs after reissue: %v", err)
	}
	if !slices.Contains(got, "example.com") {
		t.Errorf("server cert SANs %v missing example.com after reissue", got)
	}

	// A second reissue keeps the first addition (cumulative union).
	if _, err := remoteaccess.ReissueServerCert("0.0.0.0:8443", []string{"second.example.org"}); err != nil {
		t.Fatalf("second ReissueServerCert: %v", err)
	}
	got, _ = remoteaccess.ServerCertSANs()
	if !slices.Contains(got, "example.com") || !slices.Contains(got, "second.example.org") {
		t.Errorf("server cert SANs %v should keep both additions", got)
	}

	// The CA is untouched — installed devices stay valid.
	caAfter, err := remoteaccess.CACert()
	if err != nil {
		t.Fatalf("CACert after reissue: %v", err)
	}
	if string(caBefore) != string(caAfter) {
		t.Error("ReissueServerCert must NOT rotate the CA")
	}
}
