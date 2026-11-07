package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A peer's group import to this node: the staged-upload path
// (dashboard_peer_uploads.go) runs an upload route for the peer under that
// route's own peer rule. Unrestricted trust finishes any of peerUploadRoutes
// as this node's operator does. A restricted peer finishes only a route with a
// peer gate here, which its grants pass. Staged uploads are keyed to the peer
// that opened them.
//
// Every peer import is a caller other than this node's human
// (withImportForwarded), so it keeps nothing the file asks for: owner seats,
// the group environment, stored and sudo grants and cron jobs drop, listed in
// the reply. Its landing is written to this node's audit log with what
// dropped, for the operator to see.
//
// Spawn attachments stay unrestricted only.

// peerUploadGates are the upload routes a restricted peer may finish.
var peerUploadGates = map[string]bool{
	"POST /api/groups/import":         true,
	"POST /api/groups/import/inspect": true,
	// A task board's files, under the route's own peer rule
	// (servePeerTaskboardUpload).
	"POST /v1/taskboard/tasks/{id}/attachments":    true,
	"POST /v1/groups/{name}/taskboard/tasks":       true,
	"POST /v1/groups/{name}/taskboard/transfer-in": true,
	"POST /v1/groups/{name}/taskboard/ask":         true,
}

func addPeerImportRules(rules map[string]peerViewRule) {
	// Any trusted peer may stage its own uploads; the route it finishes on
	// decides.
	rules["POST /api/peer-uploads"] = peerViewRule{feature: "transfer.uploads", publicRead: true, write: servePeerUploadOpen}
	rules["POST /api/peer-uploads/chunk"] = peerViewRule{feature: "transfer.uploads", publicRead: true, write: servePeerUploadChunk}
	rules["/api/peer-uploads/finish"] = peerViewRule{feature: "transfer.uploads", publicRead: true, write: servePeerUploadFinish}
	// An import reaches a restricted peer only staged: the archive is larger
	// than one peer view request.
	for _, pattern := range []string{"POST /api/groups/import", "POST /api/groups/import/inspect"} {
		rules[pattern] = peerViewRule{feature: "groups.import", requires: PermGroupsImport, write: servePeerImportDirect}
	}
}

func servePeerUploadOpen(w http.ResponseWriter, _ *http.Request, v *peerView, _ peerViewRule) {
	openPeerUpload(w, v.peer.InstanceID)
}

func servePeerUploadChunk(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	appendPeerUpload(w, r, v.peer.InstanceID)
}

// servePeerImportDirect serves an import sent straight through the peer view
// to unrestricted trust; a restricted peer stages it.
func servePeerImportDirect(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if !v.unrestricted() {
		writeError(w, http.StatusForbidden, "staged_only", "upload this through /api/peer-uploads")
		return
	}
	sub := peerAsLocalRequest(r)
	if r.URL.Path == "/api/groups/import" {
		servePeerImportAudited(w, sub, v.peer.InstanceID, peerImportDropsOf(sub))
		return
	}
	peerDashboardMux().ServeHTTP(w, sub)
}

func servePeerUploadFinish(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	peer := v.peer.InstanceID
	finishPeerUpload(w, peerAsLocalRequest(r), peer, func(w http.ResponseWriter, sub *http.Request, pattern string) {
		// A task board's upload runs through its route's own peer rule,
		// which also names the peer in the task's history.
		if servePeerTaskboardUpload(w, sub, v, pattern) {
			return
		}
		if v.unrestricted() {
			if pattern == "POST /api/groups/import" {
				servePeerImportAudited(w, sub, peer, peerImportDropsOf(sub))
				return
			}
			peerDashboardMux().ServeHTTP(w, sub)
			return
		}
		if !peerUploadGates[pattern] {
			writeError(w, http.StatusForbidden, "peer_view_local_only", "this upload requires unrestricted trust")
			return
		}
		check, ok := peerGroupImportGate(w, sub, v)
		if !ok {
			return
		}
		a := &peerGrantAuthority{peer: peer, gates: map[string]bool{PermGroupsImport: true}, author: peerFollowUpAuthor(v, peer)}
		ctx := context.WithValue(sub.Context(), peerGrantKey{}, a)
		ctx = context.WithValue(ctx, processActorKey{}, peerLibraryActor(peer))
		sub = sub.Clone(ctx)
		if pattern == "POST /api/groups/import/inspect" {
			servePeerImportInspect(w, sub, check.members)
			return
		}
		servePeerImportAudited(w, sub, peer, check.drops)
	})
}

// peerImportCheck is what a restricted peer's import gate found.
type peerImportCheck struct {
	// members are the archive's members whose landing needs more than the
	// peer holds.
	members []peerImportMemberNeeds
	drops   string
}

type peerImportMemberNeeds struct {
	Member string   `json:"member"`
	Title  string   `json:"title,omitempty"`
	Needs  []string `json:"needs"`
}

// peerImportRead reads sub's archive as the import handler does, and puts the
// staged body back for it.
func peerImportRead(sub *http.Request) (archive []byte, into, as string, ok bool) {
	raw, err := io.ReadAll(sub.Body)
	if err != nil {
		return nil, "", "", false
	}
	probe := sub.Clone(sub.Context())
	probe.Body = io.NopCloser(bytes.NewReader(raw))
	probe.MultipartForm, probe.Form, probe.PostForm = nil, nil, nil
	archive, into, as, ok = readImportUpload(&peerViewResponse{header: make(http.Header)}, probe)
	sub.Body = io.NopCloser(bytes.NewReader(raw))
	return archive, into, as, ok
}

// peerGroupImportGate checks a restricted peer's import: groups.import and
// node.groups.create unscoped; the group's launch settings and the target
// directory within the peer's spawn grants for every group, as on create;
// embedded saved prompts under prompts.manage; and every member's landing
// harness and model within them too. It answers false having written the
// refusal; an archive the import itself refuses passes, for the handler to
// answer.
func peerGroupImportGate(w http.ResponseWriter, sub *http.Request, v *peerView) (peerImportCheck, bool) {
	peer := v.peer.InstanceID
	for _, slug := range []string{PermGroupsImport, PermNodeGroupsCreate} {
		if !fedPeerHoldsUnscoped(peer, slug) {
			writeError(w, http.StatusForbidden, "permission", "requires "+slug)
			return peerImportCheck{}, false
		}
	}
	for key := range sub.URL.Query() {
		if strings.HasPrefix(key, "keep") {
			writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("the %q option is this node's operator's: a peer import keeps nothing the file asks for", key))
			return peerImportCheck{}, false
		}
	}
	// The gate fails closed: an import it cannot judge is refused, never let
	// through for the handler to decide.
	unjudged := func(err error) (peerImportCheck, bool) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "import: "+err.Error())
		return peerImportCheck{}, false
	}
	archive, into, _, ok := peerImportRead(sub)
	if !ok {
		return unjudged(errors.New("read the uploaded archive"))
	}
	decoded, err := decodeGroupArchive(archive)
	if err != nil {
		return unjudged(err)
	}
	exp := decoded.Export
	if err := peerImportForeignRefs(exp); err != nil {
		writeError(w, http.StatusForbidden, "permission", err.Error())
		return peerImportCheck{}, false
	}
	launch := map[string]json.RawMessage{}
	set := func(key string, value any) {
		raw, _ := json.Marshal(value)
		launch[key] = raw
	}
	if into != "" {
		set("default_cwd", into)
	}
	if exp.Group.DefaultContext != "" {
		set("default_context", exp.Group.DefaultContext)
	}
	if fail := peerLaunchBodyFailure(v, launch); fail != nil {
		writeSpawnFailure(w, fail)
		return peerImportCheck{}, false
	}
	need := func(what string, fields ...string) bool {
		for _, field := range fields {
			if !peerFieldGrantedEverywhere(peer, field) {
				writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("%s decides how this node's agents launch: it takes %s and %s for every group", what, PermGroupsMembersSpawn, fedSpawnFieldSlugs[field]))
				return false
			}
		}
		return true
	}
	if len(exp.Group.DefaultPrompts) > 0 && !need("the group's default prompts", fedOverridePrompts) {
		return peerImportCheck{}, false
	}
	// A legacy archive's default model becomes the group's default profile.
	if exp.Group.DefaultModel != "" && !need("the group's default model", fedOverrideModel, fedOverrideProfile) {
		return peerImportCheck{}, false
	}
	if len(exp.SavedPrompts) > 0 && !fedPeerHoldsUnscoped(peer, PermPromptsManage) {
		writeError(w, http.StatusForbidden, "permission", "the file's saved prompts join this node's library: it takes "+PermPromptsManage)
		return peerImportCheck{}, false
	}
	check := peerImportCheck{drops: peerImportDrops(len(exp.Owners), len(exp.Group.Environment), len(exp.Group.Permissions)+len(exp.Permissions), len(exp.SudoGrants), len(exp.CronJobs))}
	landing, err := parseGroupLandingOptions(sub.URL.Query())
	if err != nil {
		return unjudged(err)
	}
	// The landings the import itself plans (runGroupImport).
	cwd := ""
	if into != "" {
		if cwd, err = filepath.Abs(expandTilde(into)); err != nil {
			return unjudged(err)
		}
	}
	landings, err := planGroupLandings(exp, decoded.Archive, landing, cwd)
	if err != nil {
		return unjudged(err)
	}
	defer closeGroupLandings(landings)
	for _, l := range landings {
		if l == nil || l.historyLanding == nil {
			continue
		}
		var needs []string
		if l.Harness != "" && l.Harness != harness.Default().Name && !peerFieldGrantedEverywhere(peer, fedOverrideHarness) {
			needs = append(needs, fedSpawnFieldSlugs[fedOverrideHarness])
		}
		if l.Model != "" && !peerFieldGrantedEverywhere(peer, fedOverrideModel) {
			needs = append(needs, fedSpawnFieldSlugs[fedOverrideModel])
		}
		if len(needs) > 0 {
			check.members = append(check.members, peerImportMemberNeeds{Member: l.ConvID, Title: l.Title, Needs: needs})
		}
	}
	if len(check.members) > 0 && sub.URL.Path == "/api/groups/import" {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "these members land with more than the peer may launch: the import is refused whole",
			"code":    "permission",
			"members": check.members,
		})
		return check, false
	}
	return check, true
}

// peerImportForeignRefs refuses a file whose messages are addressed to, or
// whose head aliases point at, a conversation it does not carry: the importer
// remaps only the file's own conversations, so such a message would land in
// one of this node's agents' inboxes (from a sender the file chose), and such
// an alias would take mail meant for one of them. A sender outside the file
// reaches only the file's own agents.
func peerImportForeignRefs(exp *groupexport.Export) error {
	messages, aliases := importForeignRefs(exp)
	if len(messages) > 0 {
		if messages[0].malformed {
			return errors.New("a peer import's messages must name their recipients as a list of the file's conversations")
		}
		return errors.New("the file's messages name an agent it does not carry: a peer import's mail stays among its own agents")
	}
	if len(aliases) > 0 {
		return errors.New("the file's head aliases name an agent it does not carry")
	}
	return nil
}

// peerImportDrops describes what a peer import drops, as counts: the
// operator reads it in the audit log, so no file text reaches it.
func peerImportDrops(owners, environment, grants, sudo, cron int) string {
	var parts []string
	for _, c := range []struct {
		n            int
		one, several string
	}{{owners, "owner seat", "owner seats"}, {environment, "environment entry", "environment entries"},
		{grants, "stored grant", "stored grants"}, {sudo, "sudo grant", "sudo grants"}, {cron, "cron job", "cron jobs"}} {
		switch {
		case c.n == 1:
			parts = append(parts, "1 "+c.one)
		case c.n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.several))
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

// peerImportDropsOf reads what an unrestricted peer's import drops.
func peerImportDropsOf(sub *http.Request) string {
	archive, _, _, ok := peerImportRead(sub)
	if !ok {
		return ""
	}
	decoded, err := decodeGroupArchive(archive)
	if err != nil {
		return ""
	}
	exp := decoded.Export
	return peerImportDrops(len(exp.Owners), len(exp.Group.Environment), len(exp.Group.Permissions)+len(exp.Permissions), len(exp.SudoGrants), len(exp.CronJobs))
}

// servePeerImportAudited runs the import and, when it lands, records it in
// this node's audit log: the peer, the group, its members and what dropped.
func servePeerImportAudited(w http.ResponseWriter, sub *http.Request, peer, drops string) {
	out := &peerViewResponse{header: make(http.Header)}
	peerDashboardMux().ServeHTTP(out, sub)
	if out.statusCode() == http.StatusOK {
		var resp struct {
			Group      string `json:"group"`
			AgentCount int    `json:"agent_count"`
		}
		if json.Unmarshal(out.body.Bytes(), &resp) == nil && resp.Group != "" {
			members := fmt.Sprintf("%d members", resp.AgentCount)
			if resp.AgentCount == 1 {
				members = "1 member"
			}
			recordFederationAudit("federation.group_import", "operator@"+peer, "", resp.Group,
				"imported group with "+members+"; dropped "+drops, http.StatusOK)
		}
	}
	for k, vals := range out.Header() {
		w.Header()[k] = vals
	}
	w.WriteHeader(out.statusCode())
	_, _ = w.Write(out.body.Bytes())
}

// servePeerImportInspect runs the import preview and adds the members whose
// landing a restricted peer could not launch, which the import would refuse.
func servePeerImportInspect(w http.ResponseWriter, sub *http.Request, members []peerImportMemberNeeds) {
	out := &peerViewResponse{header: make(http.Header)}
	peerDashboardMux().ServeHTTP(out, sub)
	body := out.body.Bytes()
	if out.statusCode() == http.StatusOK && len(members) > 0 {
		var insp map[string]json.RawMessage
		if json.Unmarshal(body, &insp) == nil {
			insp["peer_refused_members"], _ = json.Marshal(members)
			if raw, err := json.Marshal(insp); err == nil {
				body = raw
			}
		}
	}
	for k, vals := range out.Header() {
		w.Header()[k] = vals
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(out.statusCode())
	_, _ = w.Write(body)
}
