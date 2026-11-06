package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Sandbox profiles under sandbox.profiles.manage. A restricted peer's profiles
// are its own and only ever narrow: each includes exactly one existing
// profile and adds to it only filesystem denies and network_access "none". An
// include is the base the profile's own entries override, and those entries
// can only hide paths or cut the network, so the profile never confines less
// than its include: it follows an edit your operator makes to the include,
// and a missing include fails its resolution rather than falling back to
// anything (deleting an included profile is refused, and a rename moves the
// reference with it). The include is fixed once the profile exists. The peer
// edits, renames and deletes only the profiles it created. Your operator's
// profiles, the global default profile and the default implementation, and
// the import, directory, repair, read-exclusion and enforcement routes stay
// unrestricted trust's, which manages profiles as this node's operator does.

// peerSandboxProfileKeys are the fields a restricted peer's profile may set.
var peerSandboxProfileKeys = []string{"name", "includes", "filesystem", "network_access"}

func addPeerSandboxProfileRules(rules map[string]peerViewRule) {
	rule := peerViewRule{feature: "sandbox.profiles.manage", requires: PermSandboxProfilesManage}
	create := rule
	create.write = servePeerSandboxProfileCreate
	rules["POST /api/sandbox-profiles"] = create
	byName := rule
	byName.write = servePeerSandboxProfileByName
	rules["PATCH /api/sandbox-profiles/{name}"] = byName
	rules["DELETE /api/sandbox-profiles/{name}"] = byName
	read := rule
	read.serve = servePeerSandboxProfileByName
	rules["GET /api/sandbox-profiles/{name}"] = read
}

func servePeerSandboxProfileCreate(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	raw, ok := peerSandboxProfileBody(w, r, nil)
	if !ok {
		return
	}
	rec := httptest.NewRecorder()
	req := peerAsLocalRequest(r)
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	peerDashboardMux().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var created struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.ID == 0 {
			writeError(w, http.StatusInternalServerError, "io", "the profile was saved, but its id could not be read; this node's operator owns it")
			return
		}
		if err := db.SetSandboxProfilePeerOwner(created.ID, v.peer.InstanceID); err != nil {
			writeError(w, http.StatusInternalServerError, "io", "the profile was saved, but not as yours: "+err.Error())
			return
		}
	}
	for k, vals := range rec.Header() {
		w.Header()[k] = vals
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func servePeerSandboxProfileByName(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	existing, err := db.GetSandboxProfile(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	owner := ""
	if existing != nil {
		if owner, err = db.SandboxProfilePeerOwner(existing.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	}
	if existing == nil || owner != v.peer.InstanceID {
		// Your operator's profiles read through node.spawn.read's catalog;
		// this route serves and changes only the peer's own.
		writeError(w, http.StatusForbidden, "permission", "a peer edits only the sandbox profiles it created")
		return
	}
	// Your operator may have added to the peer's profile since: then it
	// carries more than a peer's shape, which a peer's edit would wipe, so
	// it is theirs to change.
	if r.Method != http.MethodGet && !peerSandboxProfileShape(existing) {
		writeError(w, http.StatusForbidden, "permission", "this node's operator has changed the profile beyond what a peer's profile holds; only they may change it now")
		return
	}
	if r.Method == http.MethodDelete {
		// A profile assigned to a group or as the default, or named by an
		// agent's spawn, would leave them with none, or with whatever is next
		// created under its name: deleting it is your operator's.
		if referenced, err := db.SandboxProfileReferenced(existing.ID, existing.Name); err != nil || referenced {
			writeError(w, http.StatusConflict, "assigned", "the profile is assigned to a group or as the default, or an agent was spawned under it; only this node's operator may delete it")
			return
		}
	}
	req := peerAsLocalRequest(r)
	if r.Method == http.MethodPatch {
		raw, ok := peerSandboxProfileBody(w, r, existing)
		if !ok {
			return
		}
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	}
	peerDashboardMux().ServeHTTP(w, req)
}

// peerSandboxProfileBody reads a restricted peer's profile and checks that it
// only narrows its include (fixed to existing's on an edit). It writes the
// refusal itself.
func peerSandboxProfileBody(w http.ResponseWriter, r *http.Request, existing *db.SandboxProfile) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sandboxProfileMaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "read body: "+err.Error())
		return nil, false
	}
	var body struct {
		Name          string                          `json:"name"`
		Includes      []string                        `json:"includes"`
		Filesystem    []sandboxpolicy.FilesystemGrant `json:"filesystem"`
		NetworkAccess sandboxpolicy.NetworkAccess     `json:"network_access"`
	}
	if fail := peerStructureBody(raw, &body, peerSandboxProfileKeys...); fail != nil {
		fail.msg += " (a peer's profile includes one profile and adds only filesystem denies and network_access none)"
		writeError(w, fail.status, fail.code, fail.msg)
		return nil, false
	}
	if existing != nil {
		if body.Includes != nil && !slices.Equal(body.Includes, existing.Includes) {
			writeError(w, http.StatusForbidden, "permission", "a peer's profile keeps the include it was created with")
			return nil, false
		}
		// The edit replaces the whole profile: one that leaves the include
		// out would drop it, so it is written back.
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "the body must be a JSON object")
			return nil, false
		}
		fields["includes"], _ = json.Marshal(existing.Includes)
		if raw, err = json.Marshal(fields); err != nil {
			writeError(w, http.StatusInternalServerError, "encode", err.Error())
			return nil, false
		}
	} else {
		if len(body.Includes) != 1 {
			writeError(w, http.StatusForbidden, "permission", "a peer's profile includes exactly one existing profile")
			return nil, false
		}
		if base, err := db.GetSandboxProfile(body.Includes[0]); err != nil || base == nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", "included sandbox profile not found")
			return nil, false
		}
	}
	for _, grant := range body.Filesystem {
		if grant.Access != sandboxpolicy.AccessDeny {
			writeError(w, http.StatusForbidden, "permission", "a peer's profile adds only filesystem denies")
			return nil, false
		}
	}
	if body.NetworkAccess != sandboxpolicy.NetworkAccessInherit && body.NetworkAccess != sandboxpolicy.NetworkAccessNone {
		writeError(w, http.StatusForbidden, "permission", "a peer's profile may only cut the network (network_access none)")
		return nil, false
	}
	return raw, true
}

// peerSandboxProfileShape reports whether a stored profile holds only what a
// peer's profile may: one include, filesystem denies, and the network cut
// or inherited.
func peerSandboxProfileShape(p *db.SandboxProfile) bool {
	if len(p.Includes) != 1 || (p.NetworkAccess != sandboxpolicy.NetworkAccessInherit && p.NetworkAccess != sandboxpolicy.NetworkAccessNone) {
		return false
	}
	for _, grant := range p.Filesystem {
		if grant.Access != sandboxpolicy.AccessDeny {
			return false
		}
	}
	return len(p.Tmpfs) == 0 && len(p.Environment) == 0 && len(p.AgentDirectories) == 0 &&
		p.FilesystemRoot == "" && p.HarnessConfig == "" && p.Network == nil && p.UnixSockets == nil &&
		p.ResourceLimits == (sandboxpolicy.ResourceLimits{}) && !p.DarwinAllowMachRegister &&
		!p.DarwinDisableKeychainWrite && len(p.PreLaunch) == 0
}
