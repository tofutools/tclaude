package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Peer reach is the part of an agent's or group's permissions that names a
// peer: a grant scoped peer=<peer>/<group> lets the holder use what that
// remote group shares. A holder keeps one row per slug, and a peer-scoped
// row counts only towards peers, so reach is merged into the row rather than
// written over it: adding forge/ops keeps an existing lab/ci, and a row that
// holds the slug for local use is refused instead of silently replaced.

// peerReachReq adds or removes one peer matcher on each slug for one holder:
// an agent (Target, a conv selector) or a local group (Group).
type peerReachReq struct {
	Target string   `json:"target,omitempty"`
	Group  string   `json:"group,omitempty"`
	Slugs  []string `json:"slugs"`
	Peer   string   `json:"peer"`
	Remove bool     `json:"remove,omitempty"`
}

// peerReachRow is one holder's peer-scoped grant of one slug.
type peerReachRow struct {
	Kind    string   `json:"kind"` // agent | group
	Group   string   `json:"group,omitempty"`
	Conv    string   `json:"conv,omitempty"`
	AgentID string   `json:"agent_id,omitempty"`
	Title   string   `json:"title,omitempty"`
	Slug    string   `json:"slug"`
	Peers   []string `json:"peers"`
}

type peerReachResp struct {
	Rows []peerReachRow `json:"rows"`
}

// errPeerReachConflict marks a row that cannot take peer reach.
var errPeerReachConflict = errors.New("peer_reach_conflict")

func handlePermissionsPeerReach(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listPeerReach(w, r)
	case http.MethodPost:
		updatePeerReach(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// listPeerReach lists every peer-scoped grant, optionally only those naming
// one peer (?peer=<label or instance ID>).
func listPeerReach(w http.ResponseWriter, r *http.Request) {
	state, err := snapshotPermissions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server", "permission state unavailable")
		return
	}
	only := ""
	if ref := strings.TrimSpace(r.URL.Query().Get("peer")); ref != "" {
		peer, err := resolveFederationPeerOpt(ref, false)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		only = peer.InstanceID
	}
	pick := func(scope PermissionScope) []string {
		var out []string
		for _, m := range scope[ScopeDimPeer] {
			if ref, _, _ := strings.Cut(m, "/"); only == "" || ref == only {
				out = append(out, m)
			}
		}
		return out
	}
	resp := peerReachResp{Rows: []peerReachRow{}}
	for conv, slugs := range state.Scopes {
		for slug, scope := range slugs {
			if state.Overrides[conv][slug] != db.PermEffectGrant {
				continue
			}
			if peers := pick(scope); len(peers) != 0 {
				resp.Rows = append(resp.Rows, peerReachRow{Kind: "agent", Conv: conv, AgentID: state.AgentIDs[conv], Title: state.Titles[conv], Slug: slug, Peers: peers})
			}
		}
	}
	for group, slugs := range state.GroupScopes {
		for slug, scope := range slugs {
			if peers := pick(scope); len(peers) != 0 {
				resp.Rows = append(resp.Rows, peerReachRow{Kind: "group", Group: group, Slug: slug, Peers: peers})
			}
		}
	}
	sort.Slice(resp.Rows, func(i, j int) bool {
		a, b := resp.Rows[i], resp.Rows[j]
		if a.Kind != b.Kind {
			return a.Kind == "group"
		}
		if a.Group+a.Title+a.Conv != b.Group+b.Title+b.Conv {
			return a.Group+a.Title+a.Conv < b.Group+b.Title+b.Conv
		}
		return a.Slug < b.Slug
	})
	writeJSON(w, http.StatusOK, resp)
}

// updatePeerReach merges one peer matcher into (or out of) a holder's grant
// of each slug, all or nothing. It is human-only: delegating reach to a peer
// is the operator's decision, and the ordinary grant path keeps covering
// agents with permissions.grant.
func updatePeerReach(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "change agents' peer reach") {
		return
	}
	var req peerReachReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	req.Target, req.Group, req.Peer = strings.TrimSpace(req.Target), strings.TrimSpace(req.Group), strings.TrimSpace(req.Peer)
	if (req.Target == "") == (req.Group == "") {
		writeError(w, http.StatusBadRequest, "invalid_arg", "name exactly one holder: target (an agent) or group")
		return
	}
	if req.Target == targetSentinelDefault {
		writeError(w, http.StatusBadRequest, "invalid_arg", "the default-permissions list cannot hold peer reach; name an agent or a group")
		return
	}
	if len(req.Slugs) == 0 || req.Peer == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "slugs and peer are required")
		return
	}
	// Each slug's matcher is canonicalized by the same rules as a grant's
	// scope: the slug must take a peer scope, and the peer label resolves to
	// its immutable instance ID.
	matcher := map[string]string{}
	for _, slug := range req.Slugs {
		if !IsKnownPermSlug(slug) {
			writeError(w, http.StatusBadRequest, "unknown_slug", fmt.Sprintf("unknown permission slug %q", slug))
			return
		}
		raw, _ := json.Marshal(map[string][]string{string(ScopeDimPeer): {req.Peer}})
		canonical, err := canonicalPermissionScopeForSlug(slug, string(raw))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_scope", fmt.Sprintf("%s: %v", slug, err))
			return
		}
		matcher[slug] = permissionScopeFromJSON(canonical)[ScopeDimPeer][0]
	}
	slugs := slices.Clone(req.Slugs)
	slices.Sort(slugs)
	slugs = slices.Compact(slugs)

	holder := "group " + req.Group
	update := func(slug string, cur db.PermissionRow) (string, bool, bool, error) {
		return mergePeerReach(holder, slug, cur, matcher[slug], req.Remove)
	}
	var err error
	if req.Group != "" {
		g, gerr := db.GetAgentGroupByName(req.Group)
		if gerr != nil || g == nil || g.IsArchived() {
			writeError(w, http.StatusNotFound, "not_found", "no such group: "+req.Group)
			return
		}
		err = db.UpdateAgentGroupPermissionScopes(g.ID, slugs, "<human>", update)
	} else {
		target, terr := resolveTarget(req.Target)
		if terr != nil {
			writeError(w, http.StatusNotFound, "not_found", terr.Error())
			return
		}
		if target.AgentID == "" {
			if req.Remove {
				writeJSON(w, http.StatusOK, peerReachResp{Rows: []peerReachRow{}})
				return
			}
			if _, _, err := db.EnsureAgentForConv(target.Key, "grant"); err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			if target.AgentID, err = db.AgentIDForConv(target.Key); err != nil || target.AgentID == "" {
				writeError(w, http.StatusInternalServerError, "io", "no agent behind "+req.Target)
				return
			}
		}
		holder = req.Target
		if target.ConvTitle != "" {
			holder = target.ConvTitle
		}
		err = db.UpdateAgentPermissionScopes(target.AgentID, slugs, "<human>", update)
	}
	switch {
	case errors.Is(err, errPeerReachConflict):
		writeError(w, http.StatusConflict, "peer_reach_conflict", strings.TrimPrefix(err.Error(), errPeerReachConflict.Error()+": "))
		return
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "no such holder")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	listPeerReach(w, r)
}

// mergePeerReach is the row rule. A missing row gains a peer-only grant; a
// peer-only grant gains or loses the matcher (and goes when its last one
// does); a deny or a grant that holds the slug for local use is refused,
// since one row cannot hold both and replacing it would take away what it
// grants.
func mergePeerReach(holder, slug string, cur db.PermissionRow, matcher string, remove bool) (scope string, keep, drop bool, err error) {
	if !cur.Exists {
		if remove {
			return "", true, false, nil
		}
		return peerScopeJSON([]string{matcher}), false, false, nil
	}
	if cur.Effect == db.PermEffectDeny {
		if remove {
			return "", true, false, nil
		}
		return "", false, false, fmt.Errorf("%w: %s is denied %s; clear the deny first (tclaude agent permissions revoke)", errPeerReachConflict, holder, slug)
	}
	parsed := permissionScopeFromJSON(cur.Scope)
	if len(parsed) != 1 || len(parsed[ScopeDimPeer]) == 0 {
		if remove {
			return "", true, false, nil
		}
		what := "everywhere on this node"
		if len(parsed) != 0 {
			what = "on this node (" + permissionScopeDisplay(parsed) + ")"
		}
		return "", false, false, fmt.Errorf("%w: %s holds %s %s, and one grant of a slug holds either local or peer scope; give the peer reach to a group it is in instead, or change that grant in its permission editor", errPeerReachConflict, holder, slug, what)
	}
	peers := slices.Clone(parsed[ScopeDimPeer])
	has := slices.Contains(peers, matcher)
	switch {
	case remove && !has, !remove && has:
		return "", true, false, nil
	case remove:
		peers = slices.DeleteFunc(peers, func(p string) bool { return p == matcher })
		if len(peers) == 0 {
			return "", false, true, nil
		}
	default:
		peers = append(peers, matcher)
	}
	return peerScopeJSON(peers), false, false, nil
}

func peerScopeJSON(peers []string) string {
	peers = slices.Clone(peers)
	slices.Sort(peers)
	raw, _ := json.Marshal(map[string][]string{string(ScopeDimPeer): peers})
	_, canonical, _ := parsePermissionScope(raw)
	return canonical
}
