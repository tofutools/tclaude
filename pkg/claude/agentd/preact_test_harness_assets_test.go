package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestPreactTestHarnessVendorAssets(t *testing.T) {
	wantHashes := map[string]string{
		"jstest/vendor/linkedom.mjs":            "196efeb17c260e001979dbc54a3c30e701a881c6e8a3eedaddc5ad83c99ee5ff",
		"jstest/vendor/preact-test-utils.mjs":   "17a3bfef8f2d7d552b3b5e3f4cc9a92ab82338a9f51607a620267dac71fcd8f3",
		"jstest/vendor/testUtils.module.js.map": "84c6d2b7ee7862c3f2e44e483a0266d2f5cabbeb20c9f6d54c41e4c5fedb89fa",
		"jstest/vendor/LICENSE-linkedom.txt":    "dc6d4961d8b6ee747231582ae9c53ce1d66bf76bc9f5a28f554c0e97210953bf",
	}
	for name, want := range wantHashes {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Errorf("reading required component-test asset %q: %v", name, err)
			continue
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("component-test asset %q hash changed; update the vendor manifest intentionally", name)
		}
	}

	manifest, err := os.ReadFile("jstest/vendor/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"linkedom", "0.18.12", "preact/test-utils", "10.29.7", "offline"} {
		if !strings.Contains(string(manifest), needle) {
			t.Errorf("component-test vendor manifest missing %q", needle)
		}
	}
}
