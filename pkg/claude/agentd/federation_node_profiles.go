package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func registerFederationNodeProfileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/profiles", handleFederationNodeProfiles)
	mux.HandleFunc("POST /v1/federation/profiles", handleFederationNodeProfiles)
	mux.HandleFunc("GET /v1/federation/profiles/{name}", handleFederationNodeProfiles)
	mux.HandleFunc("PUT /v1/federation/profiles/{name}", handleFederationNodeProfiles)
	mux.HandleFunc("DELETE /v1/federation/profiles/{name}", handleFederationNodeProfiles)
	mux.HandleFunc("POST /v1/federation/profiles/{name}/apply", handleFederationNodeProfileApply)
	mux.HandleFunc("POST /v1/federation/profiles/{name}/offer", handleFederationNodeProfileOffer)
	mux.HandleFunc("PUT /v1/federation/default-peer-profile", handleFederationDefaultPeerProfile)
}
func normalizeFederationNodeProfile(p *db.FederationNodeProfile) error {
	if !nodeGroupNamePattern.MatchString(p.Name) {
		return errors.New("invalid profile name: use 1-64 lowercase letters, digits, dots, hyphens or underscores")
	}
	s := &p.Definition
	if !validRequesterPaysPolicy(s.RequesterPays) || s.TeleportLanding != nil && !validRequesterPaysPolicy(s.TeleportLanding.RequesterPays) {
		return errors.New("requester_pays must be required, allowed or off")
	}
	if err := normalizeTeleportLanding(s.TeleportLanding); err != nil {
		return err
	}
	if s.TrustLevel == "" {
		s.TrustLevel = db.FederationTrustRestricted
	}
	if s.TrustLevel != db.FederationTrustRestricted && s.TrustLevel != db.FederationTrustUnrestricted {
		return errors.New("invalid trust level")
	}
	seen := map[string]bool{}
	pools := []string{}
	for _, ref := range s.Pools {
		g, e := db.GetFederationNodeGroupByID(ref)
		if e != nil {
			g, e = db.GetFederationNodeGroup(strings.TrimPrefix(ref, "group:"))
		}
		if e != nil {
			return fmt.Errorf("pool %s: %w", ref, e)
		}
		if !seen[g.ID] {
			pools = append(pools, g.ID)
			seen[g.ID] = true
		}
	}
	sort.Strings(pools)
	s.Pools = pools
	seen = map[string]bool{}
	for i, g := range s.PeerGrants {
		_, groupSlug := federationPeerSlugs[g.Slug]
		instanceSlug := g.Slug == "config.offer" || g.Slug == PermApprovalsAnswer || g.Slug == PermNodeRead || g.Slug == PermNodeHarnessesRead || g.Slug == PermNodeUpdate || g.Slug == PermNodeHarnessesInstall || g.Slug == PermNodeCredentialsReceive || g.Slug == PermCostsRead || g.Slug == PermFederationAuditRead
		if !groupSlug && !instanceSlug && (g.Slug != PermModelsProxy && g.Slug != PermModelsProxyLeased) {
			return fmt.Errorf("unsupported peer slug %s", g.Slug)
		}
		if instanceSlug && g.Scope != "" {
			return fmt.Errorf("%s must be unscoped", g.Slug)
		}
		if (g.Slug == PermAgentsReceive || g.Slug == PermAgentsTeleportReceive) && g.Scope == "" {
			return fmt.Errorf("%s requires a local group scope", g.Slug)
		}
		if g.Scope != "" && (g.Slug == PermModelsProxy || g.Slug == PermModelsProxyLeased) {
			if !validModelProxyScope(g.Scope) {
				return errors.New("models.proxy scope must be http_proxy=<name>")
			}
		} else if g.Scope != "" {
			if !strings.HasPrefix(g.Scope, "group=") {
				return errors.New("peer scope must be group=<local group>")
			}
			name := strings.TrimPrefix(g.Scope, "group=")
			group, e := db.GetAgentGroupByName(name)
			if e != nil || group == nil {
				if id, pe := strconv.ParseInt(name, 10, 64); pe == nil {
					group, e = db.GetAgentGroupByID(id)
				}
			}
			if e != nil || group == nil || group.IsArchived() {
				return fmt.Errorf("no active local group %s", name)
			}
			g.Scope = db.FederationGroupScope(group.ID)
		}
		if err := normalizeSelectableProfiles(g.Slug, &g.SpawnPolicy); err != nil {
			return err
		}
		if !validRequesterPaysPolicy(g.SpawnPolicy.RequesterPays) || g.SpawnPolicy.RequesterPays != "" && g.Slug != PermGroupsMembersSpawn {
			return errors.New("requester_pays requires groups.members.spawn and must be required, allowed or off")
		}
		if g.SpawnPolicy.JobApproval != "" && (g.Slug != PermJobsRun || (g.SpawnPolicy.JobApproval != "manual" && g.SpawnPolicy.JobApproval != "auto")) {
			return errors.New("job_approval must be auto or manual and requires jobs.run")
		}
		if g.Slug == PermGroupsMembersSpawn || g.Slug == PermJobsRun {
			if g.SpawnPolicy.MaxLive == 0 {
				g.SpawnPolicy.MaxLive = 2
			}
			if g.SpawnPolicy.MaxLive < 1 {
				return errors.New("max_live must be positive")
			}
		} else if !g.SpawnPolicy.Equal(db.FederationSpawnPolicy{}) {
			return errors.New("spawn_policy requires groups.members.spawn")
		}
		key := g.Slug + "\n" + g.Scope
		if seen[key] {
			return fmt.Errorf("duplicate peer grant %s", g.Slug)
		}
		seen[key] = true
		s.PeerGrants[i] = db.FederationPeerGrant{Slug: g.Slug, Scope: g.Scope, SpawnPolicy: g.SpawnPolicy}
	}
	sort.Slice(s.PeerGrants, func(i, j int) bool { a, b := s.PeerGrants[i], s.PeerGrants[j]; return a.Slug+a.Scope < b.Slug+b.Scope })
	perms, msg := normalizeSpawnPermissionOverrides(s.WorkerPermissions)
	if msg != "" {
		return errors.New(msg)
	}
	s.WorkerPermissions = perms
	labels, e := proto.NormalizeNodeLabels(s.Labels)
	if e != nil {
		return e
	}
	if s.Labels != nil {
		s.Labels = labels
	}
	if len(s.Bundle) > 0 {
		var b configbundle.Bundle
		if e = json.Unmarshal(s.Bundle, &b); e != nil {
			return e
		}
		if e = b.Validate(); e != nil {
			return e
		}
		b.Flags = nil
		b.Omitted = nil
		if e = b.Prepare(); e != nil {
			return e
		}
		if len(b.Flags) > 0 {
			return errors.New("profile config bundle contains flagged credentials; exclude flagged items before saving")
		}
		s.Bundle, e = json.Marshal(b)
		if e != nil {
			return e
		}
	}
	return nil
}
func handleFederationNodeProfiles(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage node profiles") {
		return
	}
	// Serialize definition changes with preview/apply, trust-time selection and
	// offer publication so their security checks use one profile revision.
	// GET route patterns also accept HEAD, which must stay read-only.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fedNodeGroupsMu.Lock()
		defer fedNodeGroupsMu.Unlock()
	}

	ref := r.PathValue("name")
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if ref == "" {
			ps, e := db.ListFederationNodeProfiles()
			if e != nil {
				writeFedErr(w, e)
				return
			}
			def, e := db.DefaultFederationNodeProfile()
			if e != nil {
				writeFedErr(w, e)
				return
			}
			writeJSON(w, 200, map[string]any{"profiles": ps, "default": def})
			return
		}
		p, e := db.GetFederationNodeProfile(ref)
		if e != nil {
			writeError(w, 404, "profile", e.Error())
			return
		}
		peers, e := db.ListFederationNodeProfilePeers(p.ID)
		if e != nil {
			writeFedErr(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"profile": p, "applied_peers": peers})
		return
	}
	if r.Method == http.MethodDelete {
		p, e := db.GetFederationNodeProfile(ref)
		if e == nil {
			e = db.DeleteFederationNodeProfile(p.ID)
		}
		if e != nil {
			writeError(w, 409, "profile_in_use", e.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	var p db.FederationNodeProfile
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 17<<20)).Decode(&p); e != nil {
		writeError(w, 400, "json", e.Error())
		return
	}
	if r.Method == http.MethodPost {
		p.ID = ""
		p.Revision = 0
	} else {
		old, e := db.GetFederationNodeProfile(ref)
		if e != nil {
			writeError(w, 404, "profile", e.Error())
			return
		}
		p.ID = old.ID
		if p.Revision != old.Revision {
			writeError(w, 409, "stale_profile", "reload the current profile revision")
			return
		}
	}
	if e := normalizeFederationNodeProfile(&p); e != nil {
		writeError(w, 400, "profile", e.Error())
		return
	}
	if e := db.SaveFederationNodeProfile(&p); e != nil {
		writeError(w, 409, "profile", e.Error())
		return
	}
	setAuditTargetLabel(r, p.Name)
	writeJSON(w, 200, p)
}
func handleFederationDefaultPeerProfile(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "set default peer profile") {
		return
	}
	fedNodeGroupsMu.Lock()
	defer fedNodeGroupsMu.Unlock()
	var in struct {
		Profile string `json:"profile"`
	}
	if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
		writeError(w, 400, "json", e.Error())
		return
	}
	id := ""
	if in.Profile != "" {
		p, e := db.GetFederationNodeProfile(in.Profile)
		if e != nil {
			writeError(w, 404, "profile", e.Error())
			return
		}
		id = p.ID
	}
	if e := db.SetDefaultFederationNodeProfile(id); e != nil {
		writeFedErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"profile_id": id})
}

type fedNodeProfileApplyReq struct {
	Peer               string `json:"peer"`
	Apply              bool   `json:"apply"`
	Token              string `json:"preview_token"`
	ConfirmFingerprint string `json:"confirm_fingerprint"`
}

func handleFederationNodeProfileApply(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "apply node profile") {
		return
	}
	fedNodeGroupsMu.Lock()
	defer fedNodeGroupsMu.Unlock()
	var in fedNodeProfileApplyReq
	if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
		writeError(w, 400, "json", e.Error())
		return
	}
	p, e := db.GetFederationNodeProfile(r.PathValue("name"))
	if e != nil {
		writeError(w, 404, "profile", e.Error())
		return
	}
	peer, e := resolveFederationPeerOpt(in.Peer, false)
	if e != nil {
		writeFedErr(w, e)
		return
	}
	if in.Apply && in.Token == "" {
		writeError(w, 400, "preview_required", "preview this profile before applying")
		return
	}
	if in.Apply && p.Definition.TrustLevel == db.FederationTrustUnrestricted && peer.TrustLevel != db.FederationTrustUnrestricted && in.ConfirmFingerprint != proto.Fingerprint(peer.PubKey) {
		writeError(w, 400, "confirmation_required", "confirm fingerprint "+proto.Fingerprint(peer.PubKey)+": unrestricted grants all peer permissions")
		return
	}
	// Match pool mutation's lock order, and revoke delegated approval epochs before
	// the writer commits a different trust level, membership or peer grant policy.
	finish := func() {}
	if in.Apply {
		finish = lockAwayMutation(peer.InstanceID, true)
	}
	token := ""
	if in.Apply {
		token = in.Token
	}
	plan, e := db.PlanFederationNodeProfile(p.ID, peer.InstanceID, token, nil)
	finish()
	if e != nil {
		if plan != nil {
			writeJSON(w, 409, map[string]any{"error": e.Error(), "plan": plan})
		} else {
			writeError(w, 409, "profile_apply", e.Error())
		}
		return
	}
	if in.Apply {
		broadcastFederationCatalogs()
	}
	setAuditTargetLabel(r, peer.InstanceID+" profile "+p.Name)
	writeJSON(w, 200, plan)
}

// Offers are explicit, separately retryable side effects. Assignment snapshots
// record the last queued digest so re-applying local policy never resends it.
func handleFederationNodeProfileOffer(w http.ResponseWriter, r *http.Request) {
	fedNodeGroupsMu.Lock()
	defer fedNodeGroupsMu.Unlock()
	if !requireHuman(w, r, "offer node profile config") {
		return
	}
	reconcileFederationBundleOffers()
	var in struct {
		Peer string `json:"peer"`
	}
	if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
		writeError(w, 400, "json", e.Error())
		return
	}
	peer, e := resolveFederationPeerOpt(in.Peer, false)
	if e != nil {
		writeFedErr(w, e)
		return
	}
	p, e := db.GetFederationNodeProfile(r.PathValue("name"))
	if e != nil {
		writeError(w, 404, "profile", e.Error())
		return
	}
	a, e := db.GetFederationNodeProfileAssignment(peer.InstanceID)
	if e != nil {
		writeFedErr(w, e)
		return
	}
	if a == nil || a.Profile.ID != p.ID || a.Profile.Revision != p.Revision {
		writeError(w, 409, "profile_not_applied", "apply this profile revision before offering its config")
		return
	}
	b := configbundle.Bundle{Format: configbundle.Format, FormatVersion: 1, Sections: map[string][]configbundle.Item{}}
	if len(p.Definition.Bundle) > 0 {
		if e = json.Unmarshal(p.Definition.Bundle, &b); e != nil {
			writeFedErr(w, e)
			return
		}
	}
	if b.Sections == nil {
		b.Sections = map[string][]configbundle.Item{}
	}
	if p.Definition.Labels != nil {
		items := b.Sections["config"]
		filtered := []configbundle.Item{}
		for _, item := range items {
			if item.Name != "federation.node_labels" {
				filtered = append(filtered, item)
			}
		}
		raw, _ := json.Marshal(p.Definition.Labels)
		b.Sections["config"] = append(filtered, configbundle.Item{Name: "federation.node_labels", Value: raw})
	}
	if len(b.Sections) == 0 {
		writeJSON(w, 200, map[string]any{"skipped": true, "reason": "no config or labels requested"})
		return
	}
	raw, e := json.Marshal(b)
	if e != nil {
		writeFedErr(w, e)
		return
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if a.OfferDigest == digest {
		previous, err := db.GetFederationBundleOffer("out", peer.InstanceID, a.OfferID)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		if previous != nil && (previous.State == "applied" || ((previous.State == "pending" || previous.State == "ready") && previous.Descriptor.ExpiresAt.After(time.Now()))) {
			writeJSON(w, 200, map[string]any{"unchanged": true, "offer_id": a.OfferID})
			return
		}
	}
	if b.CreatedAt == "" {
		b.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if b.TclaudeVersion == "" {
		b.TclaudeVersion = buildversion.AppVersion()
	}
	body, _ := json.Marshal(fedConfigOfferSendReq{Peer: peer.InstanceID, Bundle: &b})
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(body))
	inner.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	handleFederationOfferConfig(rec, inner)
	if rec.Code == 200 {
		var response struct {
			Offer db.FederationBundleOffer `json:"offer"`
		}
		if e = json.Unmarshal(rec.Body.Bytes(), &response); e != nil {
			writeFedErr(w, e)
			return
		}
		if e = db.MarkFederationNodeProfileOffer(peer.InstanceID, digest, response.Offer.Descriptor.ID); e != nil {
			writeFedErr(w, e)
			return
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}
