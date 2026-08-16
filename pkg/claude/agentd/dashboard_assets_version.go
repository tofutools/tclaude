package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"
)

// dashboardAssetsVersion fingerprints the embedded dashboard tree (HTML, CSS,
// JS modules, vendor runtimes, sprites) this binary serves. An agentd upgrade
// usually ships new frontend code; a dashboard left open across the upgrade
// would keep running the OLD modules against the NEW daemon. The snapshot
// carries this fingerprint (assets_version) and the served page is stamped
// with the one it was built from (injectAssetsVersion), so the poll can detect
// the mismatch and reload the page (js/assets-version.js).
//
// A content hash rather than buildversion.AppVersion() on purpose: unstamped
// dev builds all report "(devel)", which cannot distinguish an upgrade, and a
// release whose frontend is unchanged should not force a pointless reload.
var dashboardAssetsVersion = computeDashboardAssetsVersion()

func computeDashboardAssetsVersion() string {
	h := sha256.New()
	// fs.WalkDir walks in deterministic lexical order. The embedded tree is
	// read-only and complete at init, so errors here are build-time
	// impossibilities, not runtime conditions.
	err := fs.WalkDir(dashboardAssetsFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := fs.ReadFile(dashboardAssetsFS, p)
		if readErr != nil {
			return readErr
		}
		// NUL-delimited name+content pairs so file boundaries cannot alias.
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		panic("agentd: hashing embedded dashboard assets: " + err.Error())
	}
	// 16 hex chars ≈ 64 bits — plenty for "did the frontend change", and keeps
	// the meta tag and every 2s snapshot payload small.
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// assetsVersionMarker is the placeholder in dashboard.html <head> that
// injectAssetsVersion replaces with the fingerprint meta tag.
const assetsVersionMarker = "<!--ASSETS_VERSION-->"

// injectAssetsVersion replaces assetsVersionMarker with a meta tag recording
// which embedded frontend built the served page. js/assets-version.js reads it
// as the reload baseline, so a page whose HTML predates the current binary is
// reloaded on its very first poll. It panics if the marker is absent — a
// build-time invariant guarded by a test, like injectBootPreload's.
func injectAssetsVersion(html []byte) []byte {
	if !strings.Contains(string(html), assetsVersionMarker) {
		panic("agentd: dashboard.html is missing the " + assetsVersionMarker + " assets-version marker")
	}
	meta := fmt.Sprintf(`<meta name="tclaude-assets-version" content="%s">`, dashboardAssetsVersion)
	return []byte(strings.Replace(string(html), assetsVersionMarker, meta, 1))
}
