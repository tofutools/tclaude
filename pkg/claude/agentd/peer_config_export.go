package agentd

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
)

// A config bundle export under config.export, staged like every peer
// download. A restricted peer gets only the sections it may read otherwise,
// and never a bundle flagged as holding credentials: allow_flagged is not an
// option it has, so a flagged item must be skipped. A section it names in
// only= without the grant to read it is refused rather than dropped, so the
// peer is never handed less than it asked for without being told.

// peerConfigExportSectionGrants are the peer grants that read each section
// otherwise: any one of them. A section listed with none (your node's config
// keys and default permissions), or not listed at all, is never a
// restricted peer's.
var peerConfigExportSectionGrants = map[string][]string{
	"roles":             {PermNodeSpawnRead},
	"profiles":          {PermNodeSpawnRead},
	"sandbox-profiles":  {PermNodeSpawnRead},
	"saved-prompts":     {PermNodeSpawnRead, PermPromptsManage},
	"templates":         {PermTemplatesManage},
	"process-templates": {PermProcessTemplatesRead},
}

// peerConfigExportReadable reports whether peer may read a bundle section.
func peerConfigExportReadable(peer, section string) bool {
	return slices.ContainsFunc(peerConfigExportSectionGrants[section], func(slug string) bool {
		return fedPeerHoldsUnscoped(peer, slug)
	})
}

func peerConfigExportGate(v *peerView, r *http.Request) (int, string, string) {
	peer := v.peer.InstanceID
	if !fedPeerHoldsUnscoped(peer, PermConfigExport) {
		return http.StatusForbidden, "permission", "requires " + PermConfigExport
	}
	if status, code, msg := peerQueryWithin(r, "only", "skip"); status != 0 {
		return status, code, msg
	}
	q := r.URL.Query()
	for _, selector := range q["only"] {
		section, _, _ := strings.Cut(selector, "/")
		if !peerConfigExportReadable(peer, section) {
			return http.StatusForbidden, "permission", fmt.Sprintf("the %s section of a config bundle is not yours to read here", section)
		}
	}
	// Every section the bundle carries is readable: the stage skips the rest
	// before this gate runs (peerConfigExportSkips), and each chunk rechecks
	// it, so a section grant revoked mid-download stops the download.
	for _, section := range configbundle.Sections {
		if !slices.Contains(q["skip"], section) && !peerConfigExportReadable(peer, section) {
			return http.StatusForbidden, "permission", fmt.Sprintf("the %s section of this bundle is not yours to read here", section)
		}
	}
	return 0, "", ""
}

// peerConfigExportSkips skips every section the peer may not read.
func peerConfigExportSkips(v *peerView, sub *http.Request) {
	q := sub.URL.Query()
	for _, section := range configbundle.Sections {
		if !peerConfigExportReadable(v.peer.InstanceID, section) && !slices.Contains(q["skip"], section) {
			q.Add("skip", section)
		}
	}
	sub.URL.RawQuery = q.Encode()
}
